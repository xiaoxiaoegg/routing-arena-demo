// internal/strategy/s5_constrained.go
// v6 约束式选路 S5_Constrained：硬约束 + 目标优化。
//
// 与 S3（加权求和选最高分）的区别：S3 是"软权衡"，S5 是"业务口径"——
//   在「延迟 ≤ 上限」的渠道里选成本最低；
//   全部超上限时选延迟最低的（软兜底，保证有得选，绝不返回 0）。
//
// 典型业务表达："1 秒内最便宜" / "首字延迟 1.5s 内挑性价比最高的"。
// 延迟取值：健康探测的实时延迟优先，否则知识库平均延迟。

package strategy

import (
	"math"

	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/stats"
)

// S5Constrained 约束式选路策略
type S5Constrained struct {
	Scorer               // 复用候选收集（样本门槛/水位/健康过滤）
	LatencyCapMs int     // 延迟硬上限（默认 1500ms）
	Prober       *health.Prober // 实时延迟来源（nil = 用知识库平均延迟）
}

// ID 策略标识
func (s *S5Constrained) ID() string { return "S5_Constrained" }

// WithCap 设置延迟上限
func (s *S5Constrained) WithCap(ms int) *S5Constrained {
	if ms > 0 {
		s.LatencyCapMs = ms
	}
	return s
}

// Pick 约束选路：
//  1. 收集可参与渠道（复用 Scorer 的样本/水位/健康过滤）
//  2. 取各渠道延迟（实时优先）
//  3. 延迟 ≤ cap 的渠道里选成本最低；没有则全局选延迟最低
func (s *S5Constrained) Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int {
	pool := s.collectCandidates(rc, kb, candidates)
	if len(pool) == 0 {
		return 0
	}
	capMs := s.LatencyCapMs
	if capMs <= 0 {
		capMs = 1500
	}

	bestWithin := -1
	bestWithinCost := math.MaxFloat64
	bestFallback := -1
	bestFallbackLat := int(^uint(0) >> 1)

	for _, c := range pool {
		lat := c.latency
		// 有实时探测延迟时优先用实时值（更贴近当前状态）
		if s.Prober != nil {
			if real := s.Prober.Get(c.id).LatencyMs; real > 0 {
				lat = real
			}
		}
		// 约束内：成本最低
		if lat <= capMs && c.cost < bestWithinCost {
			bestWithinCost = c.cost
			bestWithin = c.id
		}
		// 兜底候选：延迟最低
		if lat < bestFallbackLat {
			bestFallbackLat = lat
			bestFallback = c.id
		}
	}
	if bestWithin != -1 {
		return bestWithin
	}
	return bestFallback
}
