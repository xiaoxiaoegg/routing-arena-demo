// internal/strategy/s3_hybrid.go
// S3_Hybrid：综合加权策略——实时算 Score = 0.4*成本 + 0.3*延迟 + 0.3*健康度，选总分最高。
// 作用：兼顾省钱与体验，真正的"接班人"（综合实战类）。
//
// 与 S1/S2 的差别：S1/S2 只看单一维度（极端探底），S3 多维度加权（综合实战）。
// 权重可通过 SetWeights 动态调整（管理后台权重微调器）。
//
// v5 升级：S3/S4 的 Scorer 引入 P2C (Power of Two Choices) 算法——
// 不再遍历全部候选渠道选最高分，而是随机抽 2 个算分选最高。
//   - 复杂度 O(N) → O(1)，省网关 CPU
//   - 天然随机性，避免"赢家通吃"导致最高分渠道瞬间被打爆

package strategy

import (
	"math"
	"math/rand"

	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/stats"
)

// HybridWeights 综合策略的评分权重（默认：成本40% / 延迟30% / 健康度30%）
type HybridWeights struct {
	Cost    float64 // 成本维度权重
	Latency float64 // 延迟维度权重
	Health  float64 // 健康度权重
}

// DefaultHybridWeights S3 默认权重（成本40 / 延迟30 / 健康30）
func DefaultHybridWeights() HybridWeights {
	return HybridWeights{Cost: 0.4, Latency: 0.3, Health: 0.3}
}

// RobustWeights S4 默认权重（成本20 / 延迟20 / 健康60）——大促/故障期保稳
func RobustWeights() HybridWeights {
	return HybridWeights{Cost: 0.2, Latency: 0.2, Health: 0.6}
}

// Scorer 加权评分器：给候选渠道按"成本/延迟/健康度"三维打分
type Scorer struct {
	Weights   HybridWeights
	Prober    *health.Prober // 健康探测（nil = 健康度按满分算）
	Watermark *Watermark     // 并发水位拦截（nil = 不启用）v5
	rng       *rand.Rand     // P2C 随机源（nil = 用全局 rand；注入 seed 可复现）
	adaptive  func(hour int) HybridWeights // v6 时段自适应权重（nil = 固定权重）
}

// SetAdaptive 启用按时段自适应权重（v6）；传 nil 恢复固定权重。
// Pick 时若 rc.Hour 有效，用 adaptive(hour) 的权重评分。
func (s *Scorer) SetAdaptive(f func(hour int) HybridWeights) {
	s.adaptive = f
}

// WithSeed 注入独立随机源（P2C 测试可复现：固定 seed → 固定抽取结果）
func (s *Scorer) WithSeed(seed int64) *Scorer {
	s.rng = rand.New(rand.NewSource(seed))
	return s
}

// cand 参与评分的候选渠道（成本/延迟/健康度三维）
type cand struct {
	id      int
	cost    float64 // 千token成本
	latency int     // 延迟 ms
	health  float64 // 健康度 [0,100]
}

// pickTwo P2C 核心：从候选池随机抽 2 个（不足 2 个则全用）。
// 复杂度 O(1)，且随机性避免"赢家通吃"——最高分渠道不会被每请求都选中。
func (s *Scorer) pickTwo(pool []cand) []cand {
	if len(pool) <= 2 {
		return pool
	}
	// 抽第一个
	i := s.randInt(len(pool))
	// 抽第二个（保证不同下标）
	j := s.randInt(len(pool))
	for j == i {
		j = s.randInt(len(pool))
	}
	return []cand{pool[i], pool[j]}
}

// randInt 返回 [0,n) 随机整数（有 rng 用独立源，否则全局）
func (s *Scorer) randInt(n int) int {
	if s.rng != nil {
		return s.rng.Intn(n)
	}
	return rand.Intn(n)
}

// Pick P2C 选路：先随机抽 2 个候选渠道，再在这 2 个里加权评分选最高分。
// 样本不足（<10）的渠道不参与抽取。
func (s *Scorer) Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int {
	if len(candidates) == 0 {
		return 0
	}
	pool := s.collectCandidates(rc, kb, candidates)
	if len(pool) == 0 {
		return 0
	}

	// P2C：随机抽 2 个渠道，在这 2 个里加权评分选最高
	//    （不再遍历全部，O(N)→O(1)；随机性防"赢家通吃"打爆单渠道）
	// 权重：自适应开启时按 rc.Hour 取时段权重，否则固定权重
	return s.pickBest(s.pickTwo(pool), s.weightsFor(rc))
}

// collectCandidates 收集可参与评分的渠道（样本充足 + 水位放行 + 健康在线），
// 附上成本/延迟/健康度三维画像。抽出来独立成函数，便于 benchmark 公平对比
// P2C 与全遍历（两者共享同一套候选收集逻辑）。
func (s *Scorer) collectCandidates(rc *RouteContext, kb *stats.Knowledge, candidates []int) []cand {
	var pool []cand
	for _, chID := range candidates {
		cs, ok := kb.ByChannelModel[channelModelKey(chID, rc.Model)]
		if !ok {
			continue
		}
		if cs.Requests < 10 {
			continue // 样本不足不参与
		}
		// v5 并发水位拦截：超配额 90% 的渠道直接踢出候选池（不给算分机会）
		if s.Watermark != nil && !s.Watermark.Allow(chID) {
			continue
		}
		c := cand{id: chID, cost: cs.AvgCostPer1K, latency: cs.AvgLatencyMs, health: 100}
		if s.Prober != nil {
			st := s.Prober.Get(chID)
			if !st.Up {
				continue // 故障渠道直接排除（健康度 0 都不给机会）
			}
			// 健康度 = 100 - 错误率*100（错误率 5% → 95 分）
			c.health = math.Max(0, 100-st.ErrorRate*100)
			// 有实时延迟时用实时值
			if st.LatencyMs > 0 {
				c.latency = st.LatencyMs
			}
		}
		pool = append(pool, c)
	}
	return pool
}

// pickBest 在候选池里按加权评分选最高分（组内 min-max 归一化）。
// 全遍历（旧 S3）就是对这个函数传整个池子。
func (s *Scorer) pickBest(pool []cand, w HybridWeights) int {
	if len(pool) == 0 {
		return 0
	}

	// 归一化基准
	minCost, maxCost := math.MaxFloat64, 0.0
	minLat := int(^uint(0) >> 1)
	maxLat := 0
	for _, c := range pool {
		if c.cost < minCost {
			minCost = c.cost
		}
		if c.cost > maxCost {
			maxCost = c.cost
		}
		if c.latency < minLat {
			minLat = c.latency
		}
		if c.latency > maxLat {
			maxLat = c.latency
		}
	}

	best := 0
	bestScore := -1.0
	for _, c := range pool {
		score := s.scoreOf(c, minCost, maxCost, minLat, maxLat, w)
		if score > bestScore {
			bestScore = score
			best = c.id
		}
	}
	return best
}

// weightsFor 取本次评分权重：自适应开启且 rc.Hour 有效 → 按时段权重，否则固定权重
func (s *Scorer) weightsFor(rc *RouteContext) HybridWeights {
	if s.adaptive != nil && rc.Hour >= 0 && rc.Hour < 24 {
		return s.adaptive(rc.Hour)
	}
	return s.Weights
}

// scoreOf 单渠道加权评分（v6：权重可按时段传入）
func (s *Scorer) scoreOf(c cand, minCost, maxCost float64, minLat, maxLat int, w HybridWeights) float64 {
	costScore := 100.0
	if maxCost > minCost {
		costScore = (1 - (c.cost-minCost)/(maxCost-minCost)) * 100
	}
	latScore := 100.0
	if maxLat > minLat {
		latScore = (1 - float64(c.latency-minLat)/float64(maxLat-minLat)) * 100
	}
	return w.Cost*costScore + w.Latency*latScore + w.Health*c.health
}

// S3Hybrid 综合加权策略（默认权重可调）
type S3Hybrid struct {
	Scorer
}

func (s *S3Hybrid) ID() string { return "S3_Hybrid" }

// SetWeights 动态调整权重（管理后台权重微调器调用）
func (s *S3Hybrid) SetWeights(w HybridWeights) {
	s.Weights = w
}
