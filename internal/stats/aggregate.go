// internal/stats/aggregate.go
// 按 (渠道, 模型) 聚合消费日志，产出各渠道的成本/延迟统计。
// 这是 S1/S2 策略决策的"历史知识库"。

package stats

import (
	"sort"
	"strconv"
	"strings"

	"routing-arena-demo/internal/logreader"
)

// ChannelStats 单个渠道×模型的统计
type ChannelStats struct {
	ChannelID    int     // 渠道 ID
	Model        string  // 模型名
	Requests     int     // 请求数（样本量）
	AvgCostPer1K float64 // 平均千 token 成本（元）
	AvgLatencyMs int     // 平均延迟（毫秒）
}

// Knowledge 聚合结果知识库：按 (渠道, 模型) 索引
type Knowledge struct {
	ByChannelModel map[string]*ChannelStats
}

// keyOf 组合渠道+模型为 map key
func keyOf(channelID int, model string) string {
	return strconv.Itoa(channelID) + ":" + model
}

// Split 按时间顺序把日志切成「训练集 / 回放集」两段。
//
// 为什么需要：如果 Knowledge 和评估用的都是同一批日志，就是「用同一批数据
// 既定价又评分」——S1_MinCost 必然选到自己定价最低的渠道，节省率必然漂亮，
// 属于样本内自证。正确做法是：用前段（策略"见过"的历史）建知识库，
// 在后段（策略"没见过"的未来）上评估，结论才有样本外效力。
//
// trainRatio 为训练集占比，越界会钳制到 [0.1, 0.9]；数据不足 2 条时全部归训练集。
func Split(recs []logreader.LogRecord, trainRatio float64) (train, test []logreader.LogRecord) {
	if trainRatio < 0.1 {
		trainRatio = 0.1
	}
	if trainRatio > 0.9 {
		trainRatio = 0.9
	}
	if len(recs) < 2 {
		return recs, nil
	}

	// 按时间排序（日志通常已有序，这里保证顺序正确，不改动入参切片）
	sorted := make([]logreader.LogRecord, len(recs))
	copy(sorted, recs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })

	cut := int(float64(len(sorted)) * trainRatio)
	if cut < 1 {
		cut = 1
	}
	if cut >= len(sorted) {
		cut = len(sorted) - 1
	}
	return sorted[:cut], sorted[cut:]
}

// Aggregate 聚合所有记录
func Aggregate(recs []logreader.LogRecord) *Knowledge {
	k := &Knowledge{ByChannelModel: make(map[string]*ChannelStats)}

	// 按 (channel, model) 分组累加
	type acc struct {
		totalQuota   float64 // 总花费
		totalTokens  int     // 总 token 数
		totalLatency int     // 总延迟
		count        int     // 请求数
	}
	accum := make(map[string]*acc)

	for _, r := range recs {
		key := keyOf(r.ChannelID, r.Model)
		a, ok := accum[key]
		if !ok {
			a = &acc{}
			accum[key] = a
		}
		a.totalQuota += r.Quota
		a.totalTokens += r.PromptTokens + r.CompletionTokens
		a.totalLatency += r.UseTimeMs
		a.count++
	}

	// 计算平均值
	for key, a := range accum {
		parts := strings.SplitN(key, ":", 2)
		chID, _ := strconv.Atoi(parts[0])
		model := parts[1]

		k.ByChannelModel[key] = &ChannelStats{
			ChannelID:    chID,
			Model:        model,
			Requests:     a.count,
			AvgCostPer1K: a.totalQuota / float64(a.totalTokens) * 1000,
			AvgLatencyMs: a.totalLatency / a.count,
		}
	}
	return k
}
