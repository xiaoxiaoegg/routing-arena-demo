// internal/strategy/circuit_breaker_3state_test.go
// v5 升级：Sentinel 式三态熔断专测。
//
// 核心验证点（与两态的区别）：
//   1. 冷却结束 ≠ 全量恢复：先进 Half-Open，只放 1 个探测
//   2. 探测失败 → 重新 Open 重新计时（防二次雪崩）——这是 v5 的灵魂
//   3. 探测成功 → 回 Closed 全量恢复
//   4. Half-Open 下并发只放行 1 个探测

package strategy

import (
	"testing"
	"time"
)

// TestBreakerHalfOpenProbeFailRetrips：探测失败 → 重新 Open 并重新计时。
// 这是三态熔断防"二次雪崩"的关键：
//   上游没恢复时，探测失败 → 不能直接全量恢复，而是重新进入熔断。
func TestBreakerHalfOpenProbeFailRetrips(t *testing.T) {
	now := time.Now()
	cb := mkBreaker(&now)

	// 触发熔断：6 失败 4 成功（60%）
	for i := 0; i < 4; i++ {
		cb.Report("S1_MinCost", true, 500*time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		cb.Report("S1_MinCost", false, 500*time.Millisecond)
	}
	if cb.CanUse("S1_MinCost") {
		t.Fatal("应已熔断")
	}

	// 冷却结束 → Half-Open 放行探测
	now = now.Add(6 * time.Minute)
	if !cb.CanUse("S1_MinCost") {
		t.Fatal("冷却结束应放行探测")
	}
	if got := cb.StateName("S1_MinCost"); got != "half_open" {
		t.Fatalf("放行探测后应处于 half_open，实际 %s", got)
	}

	// 探测失败 → 重新 Open，重新计时
	cb.Report("S1_MinCost", false, 500*time.Millisecond)
	if got := cb.StateName("S1_MinCost"); got != "open" {
		t.Fatalf("探测失败应回 open，实际 %s", got)
	}
	// 重新计时：此刻立即 CanUse 应被拒（冷却期从 0 重新开始）
	if cb.CanUse("S1_MinCost") {
		t.Fatal("探测失败后应重新熔断，不能立即放行")
	}
	// 冷却期内（+1 分钟）仍被拒
	now = now.Add(time.Minute)
	if cb.CanUse("S1_MinCost") {
		t.Fatal("重新计时后冷却期内应仍被拒")
	}
	// 第二个冷却期结束（+5 分钟）→ 再次 Half-Open 放行
	now = now.Add(5 * time.Minute)
	if !cb.CanUse("S1_MinCost") {
		t.Fatal("第二个冷却期结束应再次进入 Half-Open")
	}
	t.Log("探测失败 → 重新 Open → 第二个冷却期后才再试探（防二次雪崩）")
}

// TestBreakerHalfOpenProbeSuccessRecovers：探测成功 → 回 Closed 全量恢复
func TestBreakerHalfOpenProbeSuccessRecovers(t *testing.T) {
	now := time.Now()
	cb := mkBreaker(&now)

	// 触发熔断
	for i := 0; i < 4; i++ {
		cb.Report("S2_MinLat", true, 500*time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		cb.Report("S2_MinLat", false, 500*time.Millisecond)
	}
	if cb.CanUse("S2_MinLat") {
		t.Fatal("应已熔断")
	}

	// 冷却结束 → Half-Open 放行 1 个探测
	now = now.Add(6 * time.Minute)
	if !cb.CanUse("S2_MinLat") {
		t.Fatal("冷却结束应放行探测")
	}

	// 探测成功 → 回 Closed
	cb.Report("S2_MinLat", true, 300*time.Millisecond)
	if got := cb.StateName("S2_MinLat"); got != "closed" {
		t.Fatalf("探测成功应回 closed，实际 %s", got)
	}
	// 全量恢复：连续请求都应放行
	for i := 0; i < 10; i++ {
		if !cb.CanUse("S2_MinLat") {
			t.Fatalf("全量恢复后第 %d 次请求应放行", i)
		}
	}
	t.Log("探测成功 → 回 Closed → 全量恢复")
}

// TestBreakerHalfOpenOnlyOneProbe：Half-Open 下并发只放 1 个探测。
// 模拟两个请求同时调用 CanUse（探测在途时第二个必须被拒）。
func TestBreakerHalfOpenOnlyOneProbe(t *testing.T) {
	now := time.Now()
	cb := mkBreaker(&now)

	// 触发熔断
	for i := 0; i < 4; i++ {
		cb.Report("S3_Hybrid", true, 500*time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		cb.Report("S3_Hybrid", false, 500*time.Millisecond)
	}
	if cb.CanUse("S3_Hybrid") {
		t.Fatal("应已熔断")
	}

	// 冷却结束 → Half-Open
	now = now.Add(6 * time.Minute)
	if !cb.CanUse("S3_Hybrid") {
		t.Fatal("第一个探测应放行")
	}
	// 探测在途：后续全部被拒（无论调多少次）
	for i := 0; i < 5; i++ {
		if cb.CanUse("S3_Hybrid") {
			t.Fatalf("探测在途中第 %d 次调用应被拒（只放 1 个）", i)
		}
	}
	// 探测失败 → 回 Open；然后第二个冷却期 → 又能放 1 个新探测
	cb.Report("S3_Hybrid", false, 500*time.Millisecond)
	now = now.Add(6 * time.Minute)
	if !cb.CanUse("S3_Hybrid") {
		t.Fatal("新一轮冷却结束应能再次放行探测")
	}
	t.Log("Half-Open 只放 1 个探测；失败 → 新一轮冷却 → 再探测")
}

// TestBreakerTrippedStateDuringHalfOpen：IsTripped 在 Half-Open 期间应为 true
// （半开仍是保护态，只有探测成功回到 Closed 才算真正恢复）
func TestBreakerTrippedStateDuringHalfOpen(t *testing.T) {
	now := time.Now()
	cb := mkBreaker(&now)

	// 触发熔断
	for i := 0; i < 4; i++ {
		cb.Report("S1_MinCost", true, 500*time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		cb.Report("S1_MinCost", false, 500*time.Millisecond)
	}
	_ = cb.CanUse("S1_MinCost") // 触发熔断

	// 冷却结束 → Half-Open（放行探测）
	now = now.Add(6 * time.Minute)
	if !cb.CanUse("S1_MinCost") {
		t.Fatal("应进入 Half-Open")
	}
	if !cb.IsTripped("S1_MinCost") {
		t.Fatal("Half-Open 期间仍应算保护态（IsTripped=true）")
	}
	// 探测成功 → 才真正恢复
	cb.Report("S1_MinCost", true, 300*time.Millisecond)
	if cb.IsTripped("S1_MinCost") {
		t.Fatal("探测成功回到 Closed 后 IsTripped 应为 false")
	}
	t.Log("Half-Open 是保护态；探测成功才算真正恢复")
}
