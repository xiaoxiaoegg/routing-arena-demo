// internal/strategy/s1_mincost.go
// S1_MinCost：极限低价优先——选平均千 token 成本最低的渠道。
// 知识来自消费日志聚合（真实发生的成本），不是估算。

package strategy

import (
	"routing-arena-demo/internal/stats"
)

// S1MinCost 低价优先策略
type S1MinCost struct{}

func (s *S1MinCost) ID() string { return "S1_MinCost" }

// Pick 在候选渠道中选历史平均千 token 成本最低的
func (s *S1MinCost) Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int {
	best := 0
	bestCost := 1e18 // 极大值
	for _, chID := range candidates {
		cs, ok := kb.ByChannelModel[channelModelKey(chID, rc.Model)]
		if !ok {
			continue
		}
		// 样本太少（<10）不参与：统计无意义
		if cs.Requests < 10 {
			continue
		}
		if cs.AvgCostPer1K < bestCost {
			bestCost = cs.AvgCostPer1K
			best = chID
		}
	}
	return best
}
