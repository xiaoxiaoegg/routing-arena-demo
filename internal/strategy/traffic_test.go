// internal/strategy/traffic_test.go
// S3/S4 综合策略 + 流量分配器单测。

package strategy

import (
	"testing"

	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
)

// mkHybridKB 构造知识库：渠道1 便宜但慢(0.3, 3000ms)，渠道2 贵但快(0.8, 800ms)
func mkHybridKB() *stats.Knowledge {
	var recs []logreader.LogRecord
	for i := 0; i < 20; i++ {
		recs = append(recs,
			logreader.LogRecord{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.6, UseTimeMs: 3000},
			logreader.LogRecord{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 1.6, UseTimeMs: 800},
		)
	}
	return stats.Aggregate(recs)
}

func TestS3HybridBalancesCostAndLatency(t *testing.T) {
	// 默认权重（成本40/延迟30/健康30）下，S3 应综合权衡：
	// 渠道1 便宜(0.3)但慢(3000)，渠道2 贵(0.8)但快(800)
	// 成本分: 渠道1=100, 渠道2=0；延迟分: 渠道1=0, 渠道2=100；健康都 100
	// S1 分 = 0.4*100+0.3*0+0.3*100 = 70；S2 分 = 0.4*0+0.3*100+0.3*100 = 60
	// → S3 应选渠道1（综合分更高）
	kb := mkHybridKB()
	rc := &RouteContext{Model: "gpt-4o"}
	s3 := &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights()}}
	got := s3.Pick(rc, kb, []int{1, 2})
	if got != 1 {
		t.Errorf("S3 默认权重应选渠道1（综合70>60），实际 %d", got)
	}
}

func TestS3WeightsShiftPick(t *testing.T) {
	// 调高延迟权重（延迟90/成本10）→ 应选快的渠道2
	kb := mkHybridKB()
	rc := &RouteContext{Model: "gpt-4o"}
	s3 := &S3Hybrid{Scorer: Scorer{Weights: HybridWeights{Cost: 0.1, Latency: 0.9, Health: 0.0}}}
	got := s3.Pick(rc, kb, []int{1, 2})
	if got != 2 {
		t.Errorf("高延迟权重下 S3 应选渠道2（快），实际 %d", got)
	}
}

func TestS3ExcludesDownChannel(t *testing.T) {
	// 健康探测：渠道1 故障 → S3 排除它，即使它最便宜
	kb := mkHybridKB()
	rc := &RouteContext{Model: "gpt-4o"}
	prober := health.NewProber([]int{1, 2}, 0, func(id int) health.State {
		if id == 1 {
			return health.State{ChannelID: 1, Up: false, LatencyMs: 0, ErrorRate: 1.0}
		}
		return health.State{ChannelID: 2, Up: true, LatencyMs: 800, ErrorRate: 0}
	})
	prober.ProbeOnce()
	s3 := &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights(), Prober: prober}}
	got := s3.Pick(rc, kb, []int{1, 2})
	if got != 2 {
		t.Errorf("渠道1 故障应被排除，S3 应选渠道2，实际 %d", got)
	}
}

func TestS4RobustPrefersHealth(t *testing.T) {
	// S4 健康权重 60%：渠道2 错误率 40%（健康60分），渠道1 健康100分
	// 即使渠道1 慢且贵，S4 也应选它（健康优先）
	kb := mkHybridKB()
	rc := &RouteContext{Model: "gpt-4o"}
	prober := health.NewProber([]int{1, 2}, 0, func(id int) health.State {
		if id == 2 {
			return health.State{ChannelID: 2, Up: true, LatencyMs: 800, ErrorRate: 0.4} // 40% 错误
		}
		return health.State{ChannelID: 1, Up: true, LatencyMs: 3000, ErrorRate: 0.01}
	})
	prober.ProbeOnce()
	s4 := &S4Robust{Scorer: Scorer{Weights: RobustWeights(), Prober: prober}}
	got := s4.Pick(rc, kb, []int{1, 2})
	if got != 1 {
		t.Errorf("S4 健康优先应选渠道1（健康），实际 %d", got)
	}
}

func TestTrafficSplitterDistribution(t *testing.T) {
	// 流量分配：S0 80% / S1 15% / S3 5%，10000 次统计应接近比例
	ts := NewTrafficSplitter(map[string]float64{"S0_Champion": 80, "S1_MinCost": 15, "S3_Hybrid": 5}).WithSeed(7)
	count := map[string]int{}
	n := 20000
	for i := 0; i < n; i++ {
		count[ts.PickStrategy()]++
	}
	// 粗粒度断言（±3% 容差）
	assert := func(id string, expect float64) {
		got := float64(count[id]) / float64(n) * 100
		if got < expect-3 || got > expect+3 {
			t.Errorf("%s 比例 %.1f%% 偏离期望 %.1f%%（±3%% 容差）", id, got, expect)
		}
	}
	assert("S0_Champion", 80)
	assert("S1_MinCost", 15)
	assert("S3_Hybrid", 5)
}

func TestTrafficSplitterValidate(t *testing.T) {
	// 比例校验：总和 100% 合法，超出拒绝
	if err := Validate(map[string]float64{"S0_Champion": 80, "S1_MinCost": 20}); err != nil {
		t.Errorf("100%% 应合法: %v", err)
	}
	if err := Validate(map[string]float64{"S0_Champion": 50, "S1_MinCost": 20}); err == nil {
		t.Error("总和 70% 应拒绝")
	}
	if err := Validate(map[string]float64{}); err == nil {
		t.Error("空分配应拒绝")
	}
}

func TestTrafficSplitterReproducible(t *testing.T) {
	// 固定 seed 可复现
	ts1 := NewTrafficSplitter(map[string]float64{"S0_Champion": 80, "S1_MinCost": 20}).WithSeed(42)
	ts2 := NewTrafficSplitter(map[string]float64{"S0_Champion": 80, "S1_MinCost": 20}).WithSeed(42)
	same := true
	for i := 0; i < 200; i++ {
		if ts1.PickStrategy() != ts2.PickStrategy() {
			same = false
			break
		}
	}
	if !same {
		t.Error("相同 seed 应产生相同序列")
	}
}
