// internal/strategy/s2_minlat.go
// S2_MinLat：极速响应优先——选平均延迟最低的可用渠道。
// 知识来自消费日志聚合的真实耗时（UseTimeMs）。
//
// P3-1 增强：接入健康探测——不可用渠道直接排除，用实时延迟参与排序
// （避免把故障渠道的历史低延迟当成可选项）。

package strategy

import (
	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/stats"
)

// S2MinLat 延迟优先策略
type S2MinLat struct {
	// Prober 健康探测器（nil = 不启用，回退纯历史平均）
	Prober *health.Prober
}

func (s *S2MinLat) ID() string { return "S2_MinLat" }

// Pick 在候选渠道中选历史平均延迟最低的（排除不可用渠道）
func (s *S2MinLat) Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int {
	best := 0
	bestLat := int(^uint(0) >> 1) // int 最大值
	for _, chID := range candidates {
		// P3-1：健康探测——不可用渠道排除
		if s.Prober != nil {
			st := s.Prober.Get(chID)
			if !st.Up {
				continue // 渠道故障，排除
			}
		}
		cs, ok := kb.ByChannelModel[channelModelKey(chID, rc.Model)]
		if !ok {
			continue
		}
		// 样本太少（<10）不参与
		if cs.Requests < 10 {
			continue
		}

		lat := cs.AvgLatencyMs
		// P3-1：有实时延迟时用实时值（更贴近当前状态）
		if s.Prober != nil {
			if real := s.Prober.Get(chID).LatencyMs; real > 0 {
				lat = real
			}
		}

		if lat < bestLat {
			bestLat = lat
			best = chID
		}
	}
	return best
}
