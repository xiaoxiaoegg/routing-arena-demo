// internal/strategy/p2c_bench_test.go
// v6 性能基准：量化 P2C 的 O(N)→O(1) 收益，以及水位拦截的并发开销。
//
// 跑法：go test ./internal/strategy/ -bench=P2C -benchmem -run=^$
//       go test ./internal/strategy/ -bench=Watermark -benchmem -run=^$ -cpu=1,4,8

package strategy

import (
	"math/rand"
	"strconv"
	"testing"

	"routing-arena-demo/internal/stats"
)

// mkBenchKB 造 n 个候选渠道的知识库（随机价格/延迟，样本充足）
func mkBenchKB(n int, seed int64) *stats.Knowledge {
	rng := rand.New(rand.NewSource(seed))
	m := make(map[string]*stats.ChannelStats, n)
	for i := 1; i <= n; i++ {
		m[strconv.Itoa(i)+":gpt-4o"] = &stats.ChannelStats{
			ChannelID:    i,
			Model:        "gpt-4o",
			Requests:     1000,
			AvgCostPer1K: 0.1 + rng.Float64()*0.4,
			AvgLatencyMs: 300 + rng.Intn(3000),
		}
	}
	return &stats.Knowledge{ByChannelModel: m}
}

func benchPick(b *testing.B, nCand int, useP2C bool) {
	kb := mkBenchKB(nCand, 7)
	rc := &RouteContext{Model: "gpt-4o"}
	candidates := make([]int, nCand)
	for i := range candidates {
		candidates[i] = i + 1
	}
	s := &Scorer{Weights: DefaultHybridWeights()}
	if useP2C {
		s.WithSeed(7)
	}
	b.ResetTimer()
	w := DefaultHybridWeights()
	for i := 0; i < b.N; i++ {
		pool := s.collectCandidates(rc, kb, candidates)
		if useP2C {
			s.pickBest(s.pickTwo(pool), w)
		} else {
			s.pickBest(pool, w)
		}
	}
}

func BenchmarkP2C_4(b *testing.B)  { benchPick(b, 4, true) }
func BenchmarkScan_4(b *testing.B) { benchPick(b, 4, false) }
func BenchmarkP2C_16(b *testing.B) { benchPick(b, 16, true) }
func BenchmarkScan_16(b *testing.B) { benchPick(b, 16, false) }
func BenchmarkP2C_64(b *testing.B) { benchPick(b, 64, true) }
func BenchmarkScan_64(b *testing.B) { benchPick(b, 64, false) }

// BenchmarkWatermarkAllowParallel 水位判定并发安全 + 开销（多核扩展性）
func BenchmarkWatermarkAllowParallel(b *testing.B) {
	w := NewWatermark(map[int]int{1: 1000})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w.Allow(1)
		}
	})
}

// BenchmarkSelectChannelWatermark 引擎级水位 + P2C 全链路选路开销
func BenchmarkSelectChannelWatermark(b *testing.B) {
	kb := mkBenchKB(16, 7)
	eng := NewEngine()
	eng.WithWatermark(NewWatermark(map[int]int{1: 1000}))
	rc := &RouteContext{Model: "gpt-4o"}
	candidates := make([]int, 16)
	for i := range candidates {
		candidates[i] = i + 1
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eng.SelectChannel(rc, kb, candidates, "S3_Hybrid")
	}
}
