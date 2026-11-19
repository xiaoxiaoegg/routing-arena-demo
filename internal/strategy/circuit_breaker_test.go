// internal/strategy/circuit_breaker_test.go
// P1-6 熔断器单测：错误率熔断、P99 熔断、冷却恢复、引擎回退。

package strategy

import (
	"strings"
	"testing"
	"time"

	"routing-arena-demo/internal/stats"
)

// 用可注入时钟构造熔断器（避免真实时间等待）
func mkBreaker(now *time.Time) *CircuitBreaker {
	cb := NewCircuitBreaker(BreakerConfig{
		Window:   time.Minute,
		Cooldown: 5 * time.Minute,
		ErrRate:  0.10,               // 10%
		P99Limit: 8 * time.Second,    // 8s
	})
	cb.SetNow(func() time.Time { return *now })
	return cb
}

func TestBreakerNotTrippedOnHealthy(t *testing.T) {
	// 健康流量（少量失败）：不熔断
	now := time.Now()
	cb := mkBreaker(&now)
	for i := 0; i < 100; i++ {
		cb.Report("S1_MinCost", true, 500*time.Millisecond)
	}
	// 5 个失败（5% < 10%）
	for i := 0; i < 5; i++ {
		cb.Report("S1_MinCost", false, 500*time.Millisecond)
	}
	if !cb.CanUse("S1_MinCost") {
		t.Fatal("错误率 5% 不应熔断")
	}
}

func TestBreakerTripsOnErrorRate(t *testing.T) {
	// 错误率 50% > 10%：熔断
	now := time.Now()
	cb := mkBreaker(&now)
	for i := 0; i < 5; i++ {
		cb.Report("S1_MinCost", true, 500*time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		cb.Report("S1_MinCost", false, 500*time.Millisecond)
	}
	if cb.CanUse("S1_MinCost") {
		t.Fatal("错误率 50% 应触发熔断")
	}
	if !cb.IsTripped("S1_MinCost") {
		t.Fatal("熔断状态应标记 IsTripped")
	}
}

func TestBreakerTripsOnP99Latency(t *testing.T) {
	// P99 延迟超 8s：熔断
	// 注意 P99 = 排序后第 99 个值（索引 98）。要让它落到慢值，
	// 需要至少 2 个慢请求（98 快 + 2 慢），否则 P99 仍是快值。
	now := time.Now()
	cb := mkBreaker(&now)
	// 98 个快请求（1s）
	for i := 0; i < 98; i++ {
		cb.Report("S2_MinLat", true, 1*time.Second)
	}
	// 2 个 10s 慢请求 → P99 = 10s > 8s
	for i := 0; i < 2; i++ {
		cb.Report("S2_MinLat", true, 10*time.Second)
	}
	if cb.CanUse("S2_MinLat") {
		t.Fatal("P99 延迟超限应触发熔断")
	}
}

func TestBreakerCooldownRecovery(t *testing.T) {
	// 熔断 → 冷却期内仍熔断 → 冷却期结束后进入 Half-Open（半开探测）
	now := time.Now()
	cb := mkBreaker(&now)

	// 触发熔断：10 个请求 6 个失败（60%）
	for i := 0; i < 4; i++ {
		cb.Report("S1_MinCost", true, 500*time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		cb.Report("S1_MinCost", false, 500*time.Millisecond)
	}
	if cb.CanUse("S1_MinCost") {
		t.Fatal("错误率 60% 应熔断")
	}
	if !cb.IsTripped("S1_MinCost") {
		t.Fatal("应处于熔断状态")
	}

	// 冷却期内（+1 分钟 < 5 分钟）：仍熔断
	now = now.Add(time.Minute)
	if cb.CanUse("S1_MinCost") {
		t.Fatal("冷却期内应仍熔断")
	}

	// 冷却期结束（+6 分钟 > 5 分钟）：进入 Half-Open，放行 1 个探测请求
	now = now.Add(6 * time.Minute)
	if !cb.CanUse("S1_MinCost") {
		t.Fatal("冷却期结束应进入 Half-Open 放行探测")
	}
	// 探测在途：Half-Open 只放 1 个，第二个应被拒
	if cb.CanUse("S1_MinCost") {
		t.Fatal("Half-Open 已有探测在途，不应再放行")
	}
	// 探测成功 → 回 Closed，全量恢复
	cb.Report("S1_MinCost", true, 500*time.Millisecond)
	if !cb.CanUse("S1_MinCost") {
		t.Fatal("探测成功应全量恢复")
	}
	if cb.IsTripped("S1_MinCost") {
		t.Fatal("恢复后 IsTripped 应为 false")
	}
	if got := cb.StateName("S1_MinCost"); got != "closed" {
		t.Fatalf("恢复后状态应为 closed，实际 %s", got)
	}
}

func TestBreakerNoReportMeansOK(t *testing.T) {
	// 没上报过样本的策略：可用
	cb := NewCircuitBreaker(BreakerConfig{})
	if !cb.CanUse("S9_Unknown") {
		t.Fatal("未上报过样本的策略应可用")
	}
}

func TestEngineSkipsTrippedStrategy(t *testing.T) {
	// 集成验证：S1 熔断后，引擎选路应回退 S0
	now := time.Now()
	e := NewEngine()
	cb := mkBreaker(&now)
	e.breaker = cb

	// 触发 S1 熔断：6 失败 4 成功（60% 错误）
	for i := 0; i < 4; i++ {
		cb.Report("S1_MinCost", true, 500*time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		cb.Report("S1_MinCost", false, 500*time.Millisecond)
	}

	// 引擎指定 S1 选路 → 应回退 S0
	rc := &RouteContext{Model: "gpt-4o"}
	kb := &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.25, AvgLatencyMs: 2000},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.60, AvgLatencyMs: 100},
	}}
	strategyID, chosen := e.SelectChannel(rc, kb, []int{1, 2}, "S1_MinCost")
	if strategyID != "S0_Champion" {
		t.Fatalf("S1 熔断后应回退 S0，实际 %s", strategyID)
	}
	if chosen == 0 {
		t.Fatal("回退后必须选到渠道")
	}
}

// TestTripConditionSingleSampleNoPanic 回归：单条样本（1 个失败）不能 panic
// （P99 在样本 <10 时无统计意义，跳过延迟判定；错误率 100% 仍应熔断）
func TestTripConditionSingleSampleNoPanic(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{})
	cb.Report("S1_MinCost", false, 9000*time.Millisecond)
	// 不应 panic，且 1/1 = 100% 错误率 → 熔断
	if cb.CanUse("S1_MinCost") {
		t.Fatal("单条失败样本错误率 100%%，应熔断")
	}
	// 单条成功样本 → 不熔断，也不 panic
	cb2 := NewCircuitBreaker(BreakerConfig{})
	cb2.Report("S1_MinCost", true, 100*time.Millisecond)
	if !cb2.CanUse("S1_MinCost") {
		t.Fatal("单条成功样本不应熔断")
	}
}

// TestTripConditionFewSamples 少量样本（3 条）也不 panic，且判定合理
func TestTripConditionFewSamples(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{})
	for i := 0; i < 3; i++ {
		cb.Report("S1_MinCost", i == 0, 100*time.Millisecond) // 1/3 失败
	}
	// 33% > 10% → 熔断（且不 panic）
	if cb.CanUse("S1_MinCost") {
		t.Fatal("1/3 失败错误率 33%%，应熔断")
	}
}

// TestOnTripCallback v6：熔断触发回调（告警钩子）
func TestOnTripCallback(t *testing.T) {
	var trips []string
	cb := NewCircuitBreaker(BreakerConfig{
		OnTrip: func(id, reason string) { trips = append(trips, id+":"+reason) },
	})
	for i := 0; i < 10; i++ {
		cb.Report("S1_MinCost", false, 100*time.Millisecond)
	}
	cb.CanUse("S1_MinCost") // 触发熔断（错误率 100%）
	if len(trips) != 1 {
		t.Fatalf("熔断触发应回调 1 次，实际 %d 次 (%v)", len(trips), trips)
	}
	if !strings.Contains(trips[0], "S1_MinCost") {
		t.Errorf("回调应包含策略 ID: %s", trips[0])
	}

	// 未触发时不回调
	var trips2 []string
	cb2 := NewCircuitBreaker(BreakerConfig{
		OnTrip: func(id, reason string) { trips2 = append(trips2, id) },
	})
	cb2.Report("S2_MinLat", true, 100*time.Millisecond)
	cb2.CanUse("S2_MinLat")
	if len(trips2) != 0 {
		t.Errorf("健康策略不应触发回调: %v", trips2)
	}
}
