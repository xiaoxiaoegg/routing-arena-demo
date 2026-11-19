// internal/replay/replay.go
// 回放引擎：逐条日志让每个策略"模拟选路"，并计算反事实成本/延迟。

package replay

import (
	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

// Result 单个策略的回放汇总
type Result struct {
	StrategyID string  // 策略 ID
	TotalCost  float64 // 总成本（元）
	AvgLatency int     // 平均延迟（ms）
	Requests   int     // 处理的请求数
	// 反事实成本：选到渠道 X 时，用知识库里 X 的千token均价 × 本条token数
}

// Run 对全部记录回放一个策略
// pickChannel: 返回策略为该请求选的渠道（candidates 里是"该请求实际渠道"用于 S0）
func Run(recs []logreader.LogRecord, kb *stats.Knowledge, r strategy.Router, pickChannel func(rc *strategy.RouteContext, kb *stats.Knowledge, candidates []int) int) *Result {
	res := &Result{StrategyID: r.ID()}

	for _, rec := range recs {
		rc := &strategy.RouteContext{Model: rec.Model, Group: rec.Group, Hour: hourOf(rec.Time)}
		// S0 用实际渠道；其他策略用候选渠道集合
		var candidates []int
		if r.ID() == "S0_Champion" {
			candidates = []int{rec.ChannelID}
		} else {
			candidates = strategy.CandidatesOf(kb, rec.Model)
		}

		chosen := pickChannel(rc, kb, candidates)
		if chosen == 0 {
			continue // 无可选渠道，跳过
		}

		// 反事实成本：用知识库里 chosen 渠道的千token均价 × 本条 token 数
		cs, ok := kb.ByChannelModel[channelModelKey(chosen, rec.Model)]
		if ok {
			tokens := rec.PromptTokens + rec.CompletionTokens
			cost := cs.AvgCostPer1K / 1000 * float64(tokens)
			res.TotalCost += cost
			res.AvgLatency += cs.AvgLatencyMs
		}
		res.Requests++
	}
	if res.Requests > 0 {
		res.AvgLatency /= res.Requests
	}
	return res
}

// channelModelKey 组合渠道+模型为知识库 key（与 stats 包一致）
func channelModelKey(channelID int, model string) string {
	return intToStr(channelID) + ":" + model
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
