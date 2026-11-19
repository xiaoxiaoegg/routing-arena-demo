// internal/strategy/engine_test.go
// RoutingEngine 引擎单测：默认 S0、分桶切流、未注册回退、选不出回退。

package strategy

import (
	"testing"

	"routing-arena-demo/internal/stats"
)

// mkTestKB 构造一个有两个渠道的知识库：渠道1 便宜(0.25)，渠道2 快(100ms)
func mkTestKB() *stats.Knowledge {
	kb := &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.25, AvgLatencyMs: 2000},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.60, AvgLatencyMs: 100},
	}}
	return kb
}

// mkSparseKB 构造"样本不足"的知识库（<10 请求）：S1/S2 会因样本不足选不出渠道
func mkSparseKB() *stats.Knowledge {
	kb := &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 3, AvgCostPer1K: 0.25, AvgLatencyMs: 2000},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 2, AvgCostPer1K: 0.60, AvgLatencyMs: 100},
	}}
	return kb
}

func mkCandidates() []int { return []int{1, 2} }

func TestEngineDefaultIsS0(t *testing.T) {
	// 新引擎默认策略必须是 S0（现状收编，行为零变化）
	e := NewEngine()
	if e.DefaultID() != "S0_Champion" {
		t.Fatalf("默认策略应为 S0_Champion，实际 %s", e.DefaultID())
	}
}

func TestEngineSelectChannelDefault(t *testing.T) {
	// 默认选路返回 S0（通过 SelectDefault 兼容调用）
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	strategyID, _ := e.SelectDefault(rc, mkTestKB(), mkCandidates())
	if strategyID != "S0_Champion" {
		t.Fatalf("默认选路应返回 S0_Champion，实际 %s", strategyID)
	}
}

func TestEngineSetDefaultSwitch(t *testing.T) {
	// 切换默认策略为 S1 后，选路应走 S1
	e := NewEngine()
	if err := e.SetDefault("S1_MinCost"); err != nil {
		t.Fatalf("SetDefault 失败: %v", err)
	}
	rc := &RouteContext{Model: "gpt-4o"}
	strategyID, chosen := e.SelectDefault(rc, mkTestKB(), mkCandidates())
	if strategyID != "S1_MinCost" {
		t.Fatalf("切换后应走 S1_MinCost，实际 %s", strategyID)
	}
	if chosen != 1 {
		t.Fatalf("S1 应选成本最低的渠道1，实际 %d", chosen)
	}
}

func TestEngineSetDefaultUnknown(t *testing.T) {
	// 注册不存在的策略应报错
	e := NewEngine()
	if err := e.SetDefault("S9_Unknown"); err == nil {
		t.Fatal("设置未注册策略应报错")
	}
}

func TestEngineUnknownStrategyFallback(t *testing.T) {
	// 指定不存在的策略 ID：回退默认策略 S0（不 panic）
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	strategyID, chosen := e.SelectChannel(rc, mkTestKB(), mkCandidates(), "S9_Unknown")
	if strategyID != "S0_Champion" {
		t.Fatalf("未注册策略应回退 S0，实际 %s", strategyID)
	}
	if chosen == 0 {
		t.Fatal("回退 S0 后应选到渠道")
	}
}

func TestEnginePickZeroFallback(t *testing.T) {
	// 核心兜底：候选策略（S1）因样本不足选不出（chosen==0）→ 回退默认 S0
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	strategyID, chosen := e.SelectChannel(rc, mkSparseKB(), mkCandidates(), "S1_MinCost")
	if strategyID != "S0_Champion" {
		t.Fatalf("S1 选不出时应回退 S0，实际 %s", strategyID)
	}
	if chosen == 0 {
		t.Fatal("回退后必须选到渠道，不能丢请求")
	}
}

func TestEngineBucketRatioZero(t *testing.T) {
	// ratio=0：100% 走默认策略，绝不切流
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	b := NewBucket(0, "S1_MinCost")
	for i := 0; i < 100; i++ {
		strategyID, _ := e.Route(rc, mkTestKB(), mkCandidates(), b)
		if strategyID != "S0_Champion" {
			t.Fatalf("ratio=0 应全走 S0，第 %d 次实际 %s", i, strategyID)
		}
	}
}

func TestEngineBucketRatioFull(t *testing.T) {
	// ratio=1：100% 走候选策略
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	b := NewBucket(1.0, "S1_MinCost")
	for i := 0; i < 100; i++ {
		strategyID, _ := e.Route(rc, mkTestKB(), mkCandidates(), b)
		if strategyID != "S1_MinCost" {
			t.Fatalf("ratio=1 应全走 S1，第 %d 次实际 %s", i, strategyID)
		}
	}
}

func TestEngineBucketRatioPartial(t *testing.T) {
	// ratio=0.3：约 30% 走候选，70% 走默认（统计性验证，允许波动）
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	b := NewBucket(0.3, "S1_MinCost")
	s1Count := 0
	total := 5000
	for i := 0; i < total; i++ {
		strategyID, _ := e.Route(rc, mkTestKB(), mkCandidates(), b)
		if strategyID == "S1_MinCost" {
			s1Count++
		}
	}
	ratio := float64(s1Count) / float64(total)
	if ratio < 0.25 || ratio > 0.35 {
		t.Fatalf("ratio=0.3 期望约 30%% 走 S1，实际 %.1f%%", ratio*100)
	}
}

func TestEngineBucketNil(t *testing.T) {
	// 分桶器为 nil：走默认策略（等价于无分桶，现状）
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	strategyID, _ := e.Route(rc, mkTestKB(), mkCandidates(), nil)
	if strategyID != "S0_Champion" {
		t.Fatalf("无分桶应走默认 S0，实际 %s", strategyID)
	}
}

func TestEngineNoCandidates(t *testing.T) {
	// 无候选渠道：返回 NONE/0，不 panic
	e := NewEngine()
	rc := &RouteContext{Model: "gpt-4o"}
	strategyID, chosen := e.SelectDefault(rc, mkTestKB(), nil)
	if strategyID != "NONE" || chosen != 0 {
		t.Fatalf("无候选应返回 NONE/0，实际 %s/%d", strategyID, chosen)
	}
}
