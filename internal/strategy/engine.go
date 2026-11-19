// internal/strategy/engine.go
// RoutingEngine 路由引擎：策略池 + 默认 S0（现状收编）。
// P1 核心：网关选路只跟引擎打交道，策略可插拔，默认 100% 走 S0。
//
// 职责分离设计（自检后调优）：
//   - 分桶器（Bucket）只负责"这条请求走哪个策略"——按比例决策
//   - 引擎（SelectChannel）负责"用指定策略选渠道 + 兜底回退"
//   - 两者解耦后，P1-6 熔断 / P2 管理切策略都能挂在"选策略"环节

package strategy

import (
	"fmt"
	"math/rand"
	"time"

	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/stats"
)

// RoutingEngine 路由引擎
type RoutingEngine struct {
	strategies map[string]Router // 策略池：ID → 策略
	defaultID  string            // 当前生效策略 ID（默认 S0，分桶器/管理后台可切换）
	breaker    *CircuitBreaker   // P1-6 熔断器（nil = 不启用熔断）
	watermark  *Watermark        // v6 引擎级并发水位（nil = 不启用；对全部策略生效）
	s2         *S2MinLat         // S2 实例引用（P3-1 健康探测注入）
	s3         *S3Hybrid         // S3 实例引用（权重微调）
	s4         *S4Robust         // S4 实例引用（权重微调）
	s5         *S5Constrained    // v6 S5 实例引用（约束选路）
}

// NewEngine 创建引擎并注册六个策略
func NewEngine() *RoutingEngine {
	e := &RoutingEngine{
		strategies: make(map[string]Router),
		defaultID:  "S0_Champion",
		s2:         &S2MinLat{},
		s3:         &S3Hybrid{Scorer: Scorer{Weights: DefaultHybridWeights()}},
		s4:         &S4Robust{Scorer: Scorer{Weights: RobustWeights()}},
		s5:         &S5Constrained{Scorer: Scorer{Weights: DefaultHybridWeights()}, LatencyCapMs: 1500},
	}
	e.Register(&S0Champion{})
	e.Register(&S1MinCost{})
	e.Register(e.s2)
	e.Register(e.s3)
	e.Register(e.s4)
	e.Register(e.s5)
	return e
}

// WithHealthProber 注入健康探测器（P3-1）：S2/S3/S4/S5 选路时排除故障渠道、用实时延迟
func (e *RoutingEngine) WithHealthProber(p *health.Prober) *RoutingEngine {
	e.s2.Prober = p
	e.s3.Prober = p
	e.s4.Prober = p
	e.s5.Prober = p
	return e
}

// WithWatermark 注入并发水位拦截器（v6：引擎级，对全部策略生效——候选池先过滤，
// 再交策略算分；S3/S4/S5 内部仍保留各自的检查，双保险）
func (e *RoutingEngine) WithWatermark(w *Watermark) *RoutingEngine {
	e.watermark = w
	e.s3.Watermark = w
	e.s4.Watermark = w
	e.s5.Watermark = w
	return e
}

// WithAdaptiveWeights 启用 S3/S4 按时段自适应权重（v6，默认关闭=固定权重）
func (e *RoutingEngine) WithAdaptiveWeights() *RoutingEngine {
	e.s3.SetAdaptive(AdaptiveWeights)
	e.s4.SetAdaptive(AdaptiveWeights)
	return e
}

// WithStrategySeed 给综合策略注入固定随机源（P2C 可复现）。
// 用途：对照实验（固定 vs 自适应）时，两次跑的 P2C 抽签完全一致，
// 指标差异只来自权重，信号干净。
func (e *RoutingEngine) WithStrategySeed(seed int64) *RoutingEngine {
	e.s3.WithSeed(seed)
	e.s4.WithSeed(seed)
	return e
}

// SetHybridWeights 调整 S3 权重（管理后台权重微调器调用）
func (e *RoutingEngine) SetHybridWeights(w HybridWeights) {
	e.s3.SetWeights(w)
}

// SetRobustWeights 调整 S4 权重
func (e *RoutingEngine) SetRobustWeights(w HybridWeights) {
	e.s4.SetWeights(w)
}

// WithBreaker 启用熔断器（链式调用：NewEngine().WithBreaker(...)）
func (e *RoutingEngine) WithBreaker(cfg BreakerConfig) *RoutingEngine {
	e.breaker = NewCircuitBreaker(cfg)
	return e
}

// Breaker 返回熔断器（无则创建默认，便于外部上报样本）
func (e *RoutingEngine) Breaker() *CircuitBreaker {
	if e.breaker == nil {
		e.breaker = NewCircuitBreaker(BreakerConfig{})
	}
	return e.breaker
}

// ReportOutcome 请求结束后回告结果（v6：回放循环里调用，熔断器据此维护状态）。
// 没启用熔断器时是空操作——这样回放/场景里熔断真正生效：
// 策略老选故障渠道 → 失败率超阈值 → 熔断 → 后续请求自动回退默认策略。
func (e *RoutingEngine) ReportOutcome(strategyID string, success bool, latencyMs int) {
	if e.breaker == nil {
		return
	}
	e.breaker.Report(strategyID, success, time.Duration(latencyMs)*time.Millisecond)
}

// Register 注册策略到池
func (e *RoutingEngine) Register(r Router) {
	e.strategies[r.ID()] = r
}

// SelectChannel 用指定策略选渠道；策略选不出（chosen==0）或已熔断时回退默认策略兜底。
// rc: 请求上下文；kb: 知识库；candidates: 候选渠道；strategyID: 期望使用的策略
// 返回 (实际使用的策略ID, 选中的渠道ID)
func (e *RoutingEngine) SelectChannel(rc *RouteContext, kb *stats.Knowledge, candidates []int, strategyID string) (string, int) {
	if len(candidates) == 0 {
		return "NONE", 0
	}

	// v6 引擎级水位拦截：先过滤候选池，再交策略算分（对全部策略生效）。
	// 过滤后为空则保留原候选（"没有别的选择只能用现状"的兜底语义，
	// 也保证 S0 基线回放不会被水位卡死——现状请求真实发生过）。
	if e.watermark != nil {
		if filtered := filterAllowed(e.watermark, candidates); len(filtered) > 0 {
			candidates = filtered
		}
	}

	// 期望策略存在且未被熔断才尝试；否则直接走默认
	if s, ok := e.strategies[strategyID]; ok {
		if e.breaker != nil && !e.breaker.CanUse(strategyID) {
			// 策略熔断中 → 跳过，走默认兜底
			strategyID = e.defaultID
		} else if chosen := s.Pick(rc, kb, candidates); chosen != 0 {
			return s.ID(), chosen
		}
		// 候选策略选不出渠道（如样本不足）→ 落回默认策略
	}

	// 默认策略兜底；默认策略本身不可用时返回 NONE（绝不 panic）
	if s, ok := e.strategies[e.defaultID]; ok {
		if chosen := s.Pick(rc, kb, candidates); chosen != 0 {
			return s.ID(), chosen
		}
	}
	return "NONE", 0
}

// SelectDefault 按当前默认策略选渠道（兼容旧调用：等效 SelectChannel(..., e.defaultID)）
func (e *RoutingEngine) SelectDefault(rc *RouteContext, kb *stats.Knowledge, candidates []int) (string, int) {
	return e.SelectChannel(rc, kb, candidates, e.defaultID)
}

// SetDefault 切换默认策略（P1 分桶器/P2 管理后台调用）
func (e *RoutingEngine) SetDefault(id string) error {
	if _, ok := e.strategies[id]; !ok {
		return fmt.Errorf("策略 %s 未注册", id)
	}
	e.defaultID = id
	return nil
}

// DefaultID 当前生效策略 ID
func (e *RoutingEngine) DefaultID() string { return e.defaultID }

// StrategyIDs 已注册的所有策略 ID（用于展示）
func (e *RoutingEngine) StrategyIDs() []string {
	var ids []string
	for id := range e.strategies {
		ids = append(ids, id)
	}
	return ids
}

// filterAllowed 过滤掉超过并发水位 90% 的渠道（引擎级候选池过滤）
func filterAllowed(w *Watermark, candidates []int) []int {
	if w == nil {
		return candidates
	}
	var out []int
	for _, ch := range candidates {
		if w.Allow(ch) {
			out = append(out, ch)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 分桶器（P1-3）：按比例把流量切给候选策略，默认全走 S0。
// 职责单一：只决定"这条请求走哪个策略"，选渠道交给引擎。
//   - ratio=0    → 100% 走默认策略（现状）
//   - ratio=0.05 → 5% 走候选策略
// ---------------------------------------------------------------------------

// Bucket 分桶器：按比例决策请求走哪个策略
type Bucket struct {
	ratio    float64   // 切给候选策略的比例 [0,1]
	strategy string    // 候选策略 ID
	rng      *rand.Rand // 独立随机源（nil=用全局；注入 seed 可复现）
}

// NewBucket 创建分桶器；ratio 越界自动钳制到 [0,1]
func NewBucket(ratio float64, strategy string) *Bucket {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	return &Bucket{ratio: ratio, strategy: strategy}
}

// WithSeed 注入独立随机源（Go 1.20+ 全局 rand.Seed 已弃用失效，
// 固定 seed 必须用 rand.New(rand.NewSource(seed)) 才能可复现）
func (b *Bucket) WithSeed(seed int64) *Bucket {
	b.rng = rand.New(rand.NewSource(seed))
	return b
}

// PickStrategy 决定这条请求走哪个策略
func (b *Bucket) PickStrategy() string {
	if b.ratio <= 0 {
		return ""
	}
	if b.rng != nil {
		if b.rng.Float64() < b.ratio {
			return b.strategy
		}
		return ""
	}
	if rand.Float64() < b.ratio {
		return b.strategy
	}
	return ""
}

// Picker 策略选择器接口：分桶器（Bucket）和流量分配器（TrafficSplitter）都实现
type Picker interface {
	PickStrategy() string
}

// Route 完整选路：选择器决策策略 → 引擎选渠道（含兜底回退）
// b 可以是 *Bucket（单候选分桶）或 *TrafficSplitter（多策略流量分配）
func (e *RoutingEngine) Route(rc *RouteContext, kb *stats.Knowledge, candidates []int, b Picker) (string, int) {
	strategyID := e.defaultID
	if b != nil {
		if picked := b.PickStrategy(); picked != "" {
			strategyID = picked
		}
	}
	return e.SelectChannel(rc, kb, candidates, strategyID)
}
