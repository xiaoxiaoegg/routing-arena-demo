// internal/strategy/router_test.go
// 策略单元测试：验证 S1 选便宜的、S2 选快的。

package strategy

import (
	"testing"

	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
)

// 构造知识库：渠道1 便宜但慢，渠道2 贵但快
func buildTestKnowledge() *stats.Knowledge {
	var recs []logreader.LogRecord
	// 渠道1：千token成本 0.5，延迟 3000ms（20 条样本）
	for i := 0; i < 20; i++ {
		recs = append(recs, logreader.LogRecord{
			ChannelID: 1, Model: "gpt-4o",
			PromptTokens: 1000, CompletionTokens: 1000,
			Quota: 1.0, UseTimeMs: 3000,
		})
	}
	// 渠道2：千token成本 2.0，延迟 1000ms（20 条样本）
	for i := 0; i < 20; i++ {
		recs = append(recs, logreader.LogRecord{
			ChannelID: 2, Model: "gpt-4o",
			PromptTokens: 1000, CompletionTokens: 1000,
			Quota: 4.0, UseTimeMs: 1000,
		})
	}
	return stats.Aggregate(recs)
}

func TestS1PicksCheapest(t *testing.T) {
	kb := buildTestKnowledge()
	rc := &RouteContext{Model: "gpt-4o"}
	candidates := []int{1, 2}

	s1 := &S1MinCost{}
	got := s1.Pick(rc, kb, candidates)
	if got != 1 {
		t.Errorf("S1 应选渠道1（便宜），实际选了 %d", got)
	}
	t.Logf("S1_MinCost 选中渠道 %d（千token成本最低）", got)
}

func TestS2PicksFastest(t *testing.T) {
	kb := buildTestKnowledge()
	rc := &RouteContext{Model: "gpt-4o"}
	candidates := []int{1, 2}

	s2 := &S2MinLat{}
	got := s2.Pick(rc, kb, candidates)
	if got != 2 {
		t.Errorf("S2 应选渠道2（快），实际选了 %d", got)
	}
	t.Logf("S2_MinLat 选中渠道 %d（延迟最低）", got)
}

func TestS0PicksActual(t *testing.T) {
	s0 := &S0Champion{}
	got := s0.Pick(nil, nil, []int{5})
	if got != 5 {
		t.Errorf("S0 应返回实际渠道 5，实际 %d", got)
	}
}

// P3-1：S2 接入健康探测——排除故障渠道
func TestS2ExcludesDownChannel(t *testing.T) {
	kb := buildTestKnowledge() // 渠道2 延迟最低（1000ms）
	rc := &RouteContext{Model: "gpt-4o"}
	candidates := []int{1, 2}

	// 渠道2 故障（down），渠道1 健康
	prober := newTestProber(map[int]bool{2: false})
	s2 := &S2MinLat{Prober: prober}
	got := s2.Pick(rc, kb, candidates)
	if got != 1 {
		t.Errorf("渠道2 故障应被排除，S2 应选渠道1（唯一可用），实际选了 %d", got)
	}
	t.Logf("S2 排除故障渠道后选中 %d", got)
}

// P3-1：S2 用实时延迟替代历史平均
func TestS2UsesLiveLatency(t *testing.T) {
	kb := buildTestKnowledge() // 历史：渠道1=3000ms(慢)，渠道2=1000ms(快)
	rc := &RouteContext{Model: "gpt-4o"}
	candidates := []int{1, 2}

	// 实时：渠道1 变快（200ms），渠道2 变慢（5000ms）→ S2 应选渠道1
	prober := newTestProberWithLatency(map[int]int{1: 200, 2: 5000})
	s2 := &S2MinLat{Prober: prober}
	got := s2.Pick(rc, kb, candidates)
	if got != 1 {
		t.Errorf("实时延迟下 S2 应选渠道1（200ms），实际选了 %d", got)
	}
	t.Logf("S2 用实时延迟后选中 %d", got)
}

// newTestProber 构造测试探测器：map[渠道ID]是否可用（其余默认健康 500ms）
func newTestProber(up map[int]bool) *health.Prober {
	return health.NewProber([]int{1, 2}, 0, func(id int) health.State {
		if u, ok := up[id]; ok {
			return health.State{ChannelID: id, Up: u, LatencyMs: 500, ErrorRate: 0}
		}
		return health.State{ChannelID: id, Up: true, LatencyMs: 500, ErrorRate: 0}
	})
}

// newTestProberWithLatency 构造测试探测器：map[渠道ID]实时延迟（全部可用）
func newTestProberWithLatency(latency map[int]int) *health.Prober {
	return health.NewProber([]int{1, 2}, 0, func(id int) health.State {
		if l, ok := latency[id]; ok {
			return health.State{ChannelID: id, Up: true, LatencyMs: l, ErrorRate: 0}
		}
		return health.State{ChannelID: id, Up: true, LatencyMs: 500, ErrorRate: 0}
	})
}
