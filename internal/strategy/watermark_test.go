// internal/strategy/watermark_test.go
// v5 新增：并发水位拦截测试。
//
// 验证点：
//   1. 水位未超 → 渠道正常参与选路
//   2. 活跃并发 ≥ 配额*90% → 渠道被踢出候选池（S3 不会选到它）
//   3. 请求结束（Release）→ 水位恢复 → 渠道重新参与
//   4. 无配额渠道永远放行

package strategy

import (
	"testing"

	"routing-arena-demo/internal/stats"
)

// mkWatermarkKB 构造 2 渠道知识库：渠道1 综合最优（便宜+快），渠道2 次之
func mkWatermarkKB() *stats.Knowledge {
	return &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.10, AvgLatencyMs: 50},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.60, AvgLatencyMs: 100},
	}}
}

// TestWatermarkAllowsUnderQuota：水位未超 → 放行
func TestWatermarkAllowsUnderQuota(t *testing.T) {
	w := NewWatermark(map[int]int{1: 100})
	if !w.Allow(1) {
		t.Fatal("活跃 0 < 90，应放行")
	}
	// 活跃 50 < 90 → 仍放行
	w.Acquire(1)
	w.Acquire(1)
	if !w.Allow(1) {
		t.Fatal("活跃 50 < 90，应放行")
	}
}

// TestWatermarkBlocksAt90Percent：达到 90% 水位 → 拦截
func TestWatermarkBlocksAt90Percent(t *testing.T) {
	w := NewWatermark(map[int]int{1: 100})
	// 活跃 90 = 配额*90% → 拦截
	for i := 0; i < 90; i++ {
		w.Acquire(1)
	}
	if w.Allow(1) {
		t.Fatal("活跃 90 已达 90% 水位线，应拦截")
	}
	// 89 → 放行（整数运算：89 < 90）
	w.Release(1)
	if !w.Allow(1) {
		t.Fatal("活跃 89 < 90，应放行")
	}
}

// TestWatermarkNoQuotaAlwaysAllows：无配额渠道永远放行
func TestWatermarkNoQuotaAlwaysAllows(t *testing.T) {
	w := NewWatermark(map[int]int{1: 100}) // 渠道2 无配额
	for i := 0; i < 1000; i++ {
		w.Acquire(2)
	}
	if !w.Allow(2) {
		t.Fatal("无配额渠道应永远放行")
	}
}

// TestS3SkipsOverWatermarkChannel：S3 算分时踢出超水位渠道。
// 渠道1 综合最优（便宜+快），但若它水位超限 → S3 必须选渠道2（不能发给堵死的渠道）
func TestS3SkipsOverWatermarkChannel(t *testing.T) {
	// 渠道1 配额 100，活跃 95（超 90 水位）→ 被踢出
	w := NewWatermark(map[int]int{1: 100})
	for i := 0; i < 95; i++ {
		w.Acquire(1)
	}

	s3 := &S3Hybrid{Scorer: Scorer{
		Weights:   DefaultHybridWeights(),
		Watermark: w,
	}}
	rc := &RouteContext{Model: "gpt-4o"}
	kb := mkWatermarkKB()
	candidates := []int{1, 2}

	// 渠道1 被水位拦截 → 只剩渠道2 可选
	got := s3.Pick(rc, kb, candidates)
	if got != 2 {
		t.Fatalf("渠道1 超水位应被踢出，S3 应选渠道2，实际选了 %d", got)
	}
	t.Log("渠道1 超水位 → 被踢出候选池 → S3 选渠道2（防 TTFT 飙升）")

	// 渠道1 请求结束（水位恢复）→ 重新参与，S3 又选回它
	for i := 0; i < 95; i++ {
		w.Release(1)
	}
	got = s3.Pick(rc, kb, candidates)
	if got != 1 {
		t.Fatalf("水位恢复后 S3 应重新选渠道1（最优），实际选了 %d", got)
	}
	t.Log("渠道1 水位恢复 → 重新参与 → S3 选回最优渠道1")
}

// TestEngineLevelWatermarkAllStrategies v6：引擎级水位对全部策略生效
// （S1 也要绕开超水位渠道，而不是只有 S3/S4 内部检查）
func TestEngineLevelWatermarkAllStrategies(t *testing.T) {
	kb := mkWatermarkKB()
	eng := NewEngine()
	candidates := []int{1, 2}
	rc := &RouteContext{Model: "gpt-4o"}

	// 无水位：S1 选最便宜渠道1
	eng.ResetWatermark()
	_, ch := eng.SelectChannel(rc, kb, candidates, "S1_MinCost")
	if ch != 1 {
		t.Fatalf("无水位时 S1 应选渠道1，实际 %d", ch)
	}

	// 引擎级水位：渠道1 活跃并发 = 配额 → 超 90% → S1 也被迫绕开
	w := NewWatermark(map[int]int{1: 100})
	w.SetActive(1, 100)
	eng.WithWatermark(w)
	_, ch = eng.SelectChannel(rc, kb, candidates, "S1_MinCost")
	if ch != 2 {
		t.Fatalf("引擎级水位下 S1 应绕开渠道1 选渠道2，实际 %d", ch)
	}
	t.Log("引擎级水位：S1 也会绕开超水位渠道（不只是 S3/S4）")

	// 全部渠道都超水位 → 保留原候选（兜底语义：没有别的选择只能用现状）
	w2 := NewWatermark(map[int]int{1: 100, 2: 100})
	w2.SetActive(1, 100)
	w2.SetActive(2, 100)
	eng.WithWatermark(w2)
	_, ch = eng.SelectChannel(rc, kb, candidates, "S1_MinCost")
	if ch == 0 {
		t.Fatal("全部渠道超水位时应兜底保留原候选（S1 仍应选出渠道）")
	}
}

// ResetWatermark 清空引擎级水位（测试辅助）
func (e *RoutingEngine) ResetWatermark() {
	e.watermark = nil
	e.s3.Watermark = nil
	e.s4.Watermark = nil
}
