// internal/strategy/adaptive_test.go
// v6 时段自适应权重测试：时段映射正确 + S3 实际选路随小时变化。

package strategy

import (
	"math"
	"testing"

	"routing-arena-demo/internal/stats"
)

func TestAdaptiveWeightsMapping(t *testing.T) {
	// 白天 9-18：重延迟（体验优先）
	day := AdaptiveWeights(10)
	if day.Latency <= day.Cost {
		t.Errorf("白天应重延迟: 成本%.2f 延迟%.2f", day.Cost, day.Latency)
	}
	// 夜间 19-23 / 0-6：重成本（省钱优先）
	night := AdaptiveWeights(22)
	if night.Cost <= night.Latency {
		t.Errorf("夜间应重成本: 成本%.2f 延迟%.2f", night.Cost, night.Latency)
	}
	if night.Cost <= day.Cost {
		t.Errorf("夜间成本权重应高于白天: 白天%.2f 夜间%.2f", day.Cost, night.Cost)
	}
	// 凌晨 0-6 也是重成本
	if AdaptiveWeights(3).Cost != AdaptiveWeights(22).Cost {
		t.Errorf("凌晨应和夜间同样重成本")
	}
	// 权重总和 = 1
	for _, h := range []int{0, 6, 7, 9, 12, 18, 19, 23} {
		w := AdaptiveWeights(h)
		if math.Abs(w.Cost+w.Latency+w.Health-1.0) > 1e-9 {
			t.Errorf("hour=%d 权重和应=1: %+v", h, w)
		}
	}
}

// mkAdaptiveKB 两渠道：渠道1 快但贵、渠道2 慢但便宜
// 白天（重延迟）→ 渠道1；夜间（重成本）→ 渠道2
func mkAdaptiveKB() *stats.Knowledge {
	return &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.50, AvgLatencyMs: 300},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.10, AvgLatencyMs: 3000},
	}}
}

func TestS3AdaptivePicksByHour(t *testing.T) {
	kb := mkAdaptiveKB()
	candidates := []int{1, 2}
	rc := &RouteContext{Model: "gpt-4o"}

	s := &Scorer{Weights: DefaultHybridWeights()}
	s.SetAdaptive(AdaptiveWeights)
	s.WithSeed(7) // P2C 确定性（候选只有 2 个时本来就全选）

	// 白天 10 点：重延迟 → 选快的渠道1
	rc.Hour = 10
	if got := s.Pick(rc, kb, candidates); got != 1 {
		t.Errorf("白天应选快渠道1，实际 %d", got)
	}
	// 夜间 22 点：重成本 → 选便宜的渠道2
	rc.Hour = 22
	if got := s.Pick(rc, kb, candidates); got != 2 {
		t.Errorf("夜间应选便宜渠道2，实际 %d", got)
	}
	// 不设 Hour（固定权重）→ 默认权重 0.4/0.3/0.3 的行为
	rc.Hour = -1
	_ = s.Pick(rc, kb, candidates) // 不 panic 即可
}

func TestS3FixedWeightsUnchangedByDefault(t *testing.T) {
	// 没开自适应 → 固定权重（回归：v6 之前的行为不变）
	kb := mkAdaptiveKB()
	rc := &RouteContext{Model: "gpt-4o", Hour: 22} // 即使带了 hour 也不应生效
	s := &Scorer{Weights: DefaultHybridWeights()}
	s.WithSeed(7)
	if got := s.Pick(rc, kb, candidates2()); got == 0 {
		t.Errorf("固定权重模式应正常选路")
	}
}

func candidates2() []int { return []int{1, 2} }
