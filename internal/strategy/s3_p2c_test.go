// internal/strategy/s3_p2c_test.go
// v5 升级：P2C (Power of Two Choices) 算法测试。
//
// 验证点：
//   1. 候选 <=2 时：P2C 直接用全部（等价旧逻辑，行为不变）
//   2. 候选 >2 时：引入随机性，但"偏向最优"——最优渠道被选中的概率最高
//   3. WithSeed 可复现：同 seed → 同抽取结果
//   4. P2C 不会"赢家通吃"——最差渠道也有机会被选中（因为可能抽到它且另一个更差）

package strategy

import (
	"testing"

	"routing-arena-demo/internal/stats"
)

// mkP2CKB 构造 3 个渠道的知识库：
//   渠道1: 最便宜(0.1) + 最快(50ms)  → 综合最优
//   渠道2: 中等(0.5) + 中等(500ms)
//   渠道3: 最贵(2.0) + 最慢(5000ms) → 综合最差
func mkP2CKB() *stats.Knowledge {
	return &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.10, AvgLatencyMs: 50},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.50, AvgLatencyMs: 500},
		"3:gpt-4o": {ChannelID: 3, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 2.00, AvgLatencyMs: 5000},
	}}
}

// mkP2CTwoCandKB 构造只有 2 个渠道的知识库（P2C 应直接用全部，行为不变）
func mkP2CTwoCandKB() *stats.Knowledge {
	return &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.25, AvgLatencyMs: 2000},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.60, AvgLatencyMs: 100},
	}}
}

// TestS3P2CTwoCandidatesNoChange：候选只有 2 个时，P2C 直接全用，必选综合最优
func TestS3P2CTwoCandidatesNoChange(t *testing.T) {
	s3 := &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights()}}
	rc := &RouteContext{Model: "gpt-4o"}
	kb := mkP2CTwoCandKB()
	candidates := []int{1, 2}

	// 渠道1: 便宜(0.25)但慢(2000ms)；渠道2: 贵(0.6)但快(100ms)
	// 默认权重 0.4成本/0.3延迟/0.3健康 → 归一化后渠道1 综合更优（成本差巨大）
	// 2 候选时 P2C 用全部两个，必然稳定选渠道1（行为不变，等价旧逻辑）
	for i := 0; i < 50; i++ {
		got := s3.Pick(rc, kb, candidates)
		if got != 1 {
			t.Fatalf("2 候选时 P2C 应稳定选最优渠道1，第 %d 次选了 %d", i, got)
		}
	}
	t.Log("2 候选场景：P2C 直接用全部，稳定选最优渠道1（等价旧逻辑）")
}

// TestS3P2CBiasedToBest：候选 >2 时，P2C 偏向最优但不独占（防赢家通吃）。
// 数学期望（3 渠道、1 个最优）：
//   最优选中率 = 抽到它的概率 2/3 ≈ 66.7%（抽到必赢）
//   次优选中率 = 没抽到最优且抽到它 ≈ 1/3 ≈ 33.3%
//   最差选中率 = 0（即使被抽到，对手也永远更优）
func TestS3P2CBiasedToBest(t *testing.T) {
	s3 := &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights()}}
	rc := &RouteContext{Model: "gpt-4o"}
	kb := mkP2CKB()
	candidates := []int{1, 2, 3}

	// 固定 seed 保证可复现
	s3.WithSeed(42)

	counts := map[int]int{}
	const N = 5000
	for i := 0; i < N; i++ {
		got := s3.Pick(rc, kb, candidates)
		counts[got]++
	}
	bestRate := float64(counts[1]) / N
	t.Logf("P2C %d 次选路分布：渠道1(最优)=%d(%.1f%%), 渠道2=%d(%.1f%%), 渠道3(最差)=%d(%.1f%%)",
		N, counts[1], bestRate*100, counts[2], float64(counts[2])/N*100, counts[3], float64(counts[3])/N*100)

	// 1. 偏向最优：最优渠道被选中率 > 50%（显著高于均匀分布的 33%）
	if bestRate <= 0.5 {
		t.Errorf("P2C 应偏向最优渠道，选中率应>50%%，实际 %.1f%%", bestRate*100)
	}
	// 2. 不赢家通吃：最优渠道选中率 < 95%（不该几乎独占）
	if bestRate >= 0.95 {
		t.Errorf("P2C 应分散流量（防赢家通吃），最优渠道选中率应<95%%，实际 %.1f%%", bestRate*100)
	}
	// 3. 次优渠道（2）有机会承接流量
	if counts[2] == 0 {
		t.Errorf("次优渠道2 应有机会被选中，实际 0 次")
	}
	// 4. 最差渠道被选中率低（接近 0，但不断言严格为 0，留随机波动余地）
	if counts[3] > counts[2] {
		t.Errorf("最差渠道3 选中率不应超过次优渠道2：渠道3=%d 渠道2=%d", counts[3], counts[2])
	}
}

// TestS3P2CDeterministicSeed：同 seed → 同结果（可复现）
func TestS3P2CDeterministicSeed(t *testing.T) {
	rc := &RouteContext{Model: "gpt-4o"}
	kb := mkP2CKB()
	candidates := []int{1, 2, 3}

	run := func() []int {
		s3 := &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights()}}
		s3.WithSeed(7)
		out := make([]int, 20)
		for i := 0; i < 20; i++ {
			out[i] = s3.Pick(rc, kb, candidates)
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同 seed 结果应一致，第 %d 次不同：%d vs %d", i, a[i], b[i])
		}
	}
	t.Logf("同 seed 20 次选路序列可复现：%v", a)
}

// TestS3P2CSingleCandidate：只剩 1 个可用渠道时，P2C 直接返回它
func TestS3P2CSingleCandidate(t *testing.T) {
	s3 := &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights()}}
	rc := &RouteContext{Model: "gpt-4o"}
	kb := mkP2CKB()
	got := s3.Pick(rc, kb, []int{1})
	if got != 1 {
		t.Fatalf("单候选应返回渠道1，实际 %d", got)
	}
}
