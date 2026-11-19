// internal/strategy/router.go
// 路由策略统一接口 + 策略池注册。

package strategy

import (
	"sort"
	"strconv"

	"routing-arena-demo/internal/stats"
)

// RouteContext 选路所需上下文：一条请求该选哪个渠道
type RouteContext struct {
	Model string // 模型名
	Group string // 会话/用户标识（v6 粘性路由用；空 = 不启用粘性）
	Hour  int    // 请求发生的小时 [0,23]（v6 自适应权重用；<0 或 >23 = 不启用）
	// 未来可扩展：Path / Retry
}

// Router 策略统一接口
type Router interface {
	ID() string // 策略 ID："S0_Champion" / "S1_MinCost" / "S2_MinLat"
	// Pick 从候选渠道里选一个；kb 是历史知识库（S1/S2 用它决策）
	Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int
}

// CandidatesOf 取出某个模型的所有候选渠道 ID（从知识库推导）。
// 结果按渠道 ID 升序——map 遍历无序会导致重试目标/熔断时机抖动，
// 排序后回放结果完全可复现。
func CandidatesOf(kb *stats.Knowledge, model string) []int {
	seen := map[int]bool{}
	var out []int
	for key, cs := range kb.ByChannelModel {
		_ = key
		if cs.Model == model {
			if !seen[cs.ChannelID] {
				seen[cs.ChannelID] = true
				out = append(out, cs.ChannelID)
			}
		}
	}
	sort.Ints(out)
	return out
}

// channelModelKey 组合渠道+模型为知识库 key（与 stats 包一致）
func channelModelKey(channelID int, model string) string {
	return strconv.Itoa(channelID) + ":" + model
}
