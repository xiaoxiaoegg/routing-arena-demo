// internal/strategy/s5_constrained_test.go
// v6 约束式选路测试：延迟上限内选最便宜、全部超限时选最快、兜底不返回 0。

package strategy

import (
	"testing"

	"routing-arena-demo/internal/stats"
)

// mkS5KB 三渠道：1 贵但快、2 便宜但超慢、3 中等（1.2s，在 1.5s 内）
func mkS5KB() *stats.Knowledge {
	return &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.50, AvgLatencyMs: 500},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.10, AvgLatencyMs: 3000},
		"3:gpt-4o": {ChannelID: 3, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.30, AvgLatencyMs: 1200},
	}}
}

func TestS5PicksCheapestWithinCap(t *testing.T) {
	kb := mkS5KB()
	rc := &RouteContext{Model: "gpt-4o"}
	s := &S5Constrained{Scorer: Scorer{Weights: DefaultHybridWeights()}, LatencyCapMs: 1500}

	// cap=1500：渠道1(500ms) 和 渠道3(1200ms) 在约束内 → 选更便宜的渠道3
	got := s.Pick(rc, kb, []int{1, 2, 3})
	if got != 3 {
		t.Fatalf("1.5s 内最便宜应为渠道3，实际 %d", got)
	}
}

func TestS5AllWithinCapPicksCheapest(t *testing.T) {
	kb := mkS5KB()
	rc := &RouteContext{Model: "gpt-4o"}
	// cap 足够大：全部在约束内 → 选全局最便宜渠道2
	s := &S5Constrained{Scorer: Scorer{Weights: DefaultHybridWeights()}, LatencyCapMs: 5000}
	if got := s.Pick(rc, kb, []int{1, 2, 3}); got != 2 {
		t.Fatalf("全在约束内应选最便宜渠道2，实际 %d", got)
	}
}

func TestS5FallbackFastestWhenAllOverCap(t *testing.T) {
	kb := mkS5KB()
	rc := &RouteContext{Model: "gpt-4o"}
	// cap=200：全部超限 → 兜底选延迟最低渠道1
	s := &S5Constrained{Scorer: Scorer{Weights: DefaultHybridWeights()}, LatencyCapMs: 200}
	if got := s.Pick(rc, kb, []int{1, 2, 3}); got != 1 {
		t.Fatalf("全部超限应兜底选最快渠道1，实际 %d", got)
	}
}

func TestS5DefaultCap(t *testing.T) {
	kb := mkS5KB()
	rc := &RouteContext{Model: "gpt-4o"}
	// 未设 cap → 默认 1500ms
	s := &S5Constrained{Scorer: Scorer{Weights: DefaultHybridWeights()}}
	if got := s.Pick(rc, kb, []int{1, 2, 3}); got != 3 {
		t.Fatalf("默认 cap 应同 1500ms（选渠道3），实际 %d", got)
	}
}

func TestS5EmptyCandidates(t *testing.T) {
	s := &S5Constrained{Scorer: Scorer{Weights: DefaultHybridWeights()}, LatencyCapMs: 1500}
	if got := s.Pick(&RouteContext{Model: "gpt-4o"}, mkS5KB(), nil); got != 0 {
		t.Fatalf("无候选应返回 0，实际 %d", got)
	}
}
