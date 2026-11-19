// internal/report/report_test.go
// P0 结论判定（Conclusion）单测：覆盖三个阈值分支 + 边界情况。

package report

import (
	"strings"
	"testing"

	"routing-arena-demo/internal/replay"
)

// mkResults 构造回放结果：s0Cost 为基线成本，candCost 为候选策略成本
func mkResults(s0Cost, candCost float64) []*replay.Result {
	return []*replay.Result{
		{StrategyID: "S0_Champion", TotalCost: s0Cost, AvgLatency: 2000, Requests: 100},
		{StrategyID: "S1_MinCost", TotalCost: candCost, AvgLatency: 1500, Requests: 100},
	}
}

func TestConclusionWorthContinuing(t *testing.T) {
	// 省 13.3% ≥ 10% → 值得继续
	got := Conclusion(mkResults(8.33, 7.22))
	if !strings.Contains(got, "值得继续") || !strings.Contains(got, "S1_MinCost") {
		t.Errorf("省≥10%% 应判定为值得继续，got: %q", got)
	}
}

func TestConclusionMarginal(t *testing.T) {
	// 省 7% 在 5%~10% → 勉强可行
	got := Conclusion(mkResults(100, 93))
	if !strings.Contains(got, "勉强可行") {
		t.Errorf("省 5%%~10%% 应判定为勉强可行，got: %q", got)
	}
}

func TestConclusionNotWorth(t *testing.T) {
	// 省 3% < 5% → 价值存疑
	got := Conclusion(mkResults(100, 97))
	if !strings.Contains(got, "价值存疑") || !strings.Contains(got, "降级") {
		t.Errorf("省<5%% 应判定为价值存疑，got: %q", got)
	}
}

func TestConclusionBoundary10(t *testing.T) {
	// 恰好省 10% → 值得继续（≥ 边界含）
	got := Conclusion(mkResults(100, 90))
	if !strings.Contains(got, "值得继续") {
		t.Errorf("恰好省10%% 应判定为值得继续，got: %q", got)
	}
}

func TestConclusionBoundary5(t *testing.T) {
	// 恰好省 5% → 勉强可行（≥ 边界含）
	got := Conclusion(mkResults(100, 95))
	if !strings.Contains(got, "勉强可行") {
		t.Errorf("恰好省5%% 应判定为勉强可行，got: %q", got)
	}
}

func TestConclusionNoBaseline(t *testing.T) {
	// 无 S0 基线 → 无法下结论
	got := Conclusion([]*replay.Result{
		{StrategyID: "S1_MinCost", TotalCost: 7.22},
	})
	if !strings.Contains(got, "无法下结论") {
		t.Errorf("无 S0 基线应无法下结论，got: %q", got)
	}
}

func TestConclusionNoCandidate(t *testing.T) {
	// 只有 S0，无候选策略 → 无法下结论
	got := Conclusion([]*replay.Result{
		{StrategyID: "S0_Champion", TotalCost: 8.33},
	})
	if !strings.Contains(got, "无法下结论") {
		t.Errorf("无候选策略应无法下结论，got: %q", got)
	}
}
