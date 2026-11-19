// internal/strategy/s0_champion.go
// S0_Champion：基线策略——复现"当时实际选中的渠道"。
// 真实网关是 priority+weight 加权随机；影子竞技场里我们没有完整权重信息，
// 所以用日志里实际发生的渠道作为基线（这就是"现状行为"）。

package strategy

import (
	"routing-arena-demo/internal/stats"
)

// S0Champion 基线策略
type S0Champion struct{}

func (s *S0Champion) ID() string { return "S0_Champion" }

// Pick 返回实际渠道（由回放引擎通过 candidates[0] 传入）
func (s *S0Champion) Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int {
	if len(candidates) == 0 {
		return 0
	}
	return candidates[0]
}
