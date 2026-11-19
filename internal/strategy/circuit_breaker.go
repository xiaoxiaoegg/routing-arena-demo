// internal/strategy/circuit_breaker.go
// 策略级熔断器（v5 升级：Sentinel 式三态状态机）：保护真实流量不被坏策略伤害。
//
// 规则（对齐 v5 方案）：
//   - 滑动窗口内（默认 1 分钟）错误率 > 10%  → 熔断该策略（Closed → Open）
//   - 滑动窗口内 P99 延迟 > 8s                → 熔断该策略（Closed → Open）
//   - Open 状态：该策略不参与选路（回退默认 S0）
//   - 冷却期（默认 5 分钟）结束后 → Half-Open（半开）：
//       仅放行 1 个探测请求试探；成功 → 回 Closed 全量恢复，
//       失败 → 重新 Open 并重新计时（防恢复瞬间二次雪崩）
//
// 熔断只作用于"候选策略"（S1/S2/S3/S4），默认策略 S0 是兜底，永不熔断。

package strategy

import (
	"sort"
	"sync"
	"time"
)

// BreakerConfig 熔断器配置
type BreakerConfig struct {
	Window   time.Duration // 统计滑动窗口（默认 1 分钟）
	Cooldown time.Duration // 冷却期（默认 5 分钟）
	ErrRate  float64       // 错误率阈值（默认 0.10 = 10%）
	P99Limit time.Duration // P99 延迟上限（默认 8s）
	// OnTrip 熔断触发回调（v6 告警钩子）：策略从 Closed 进入 Open 时调用，
	// reason 描述触发原因（错误率/P99/半开探测失败）。注意在锁内调用，回调需轻量
	// （写日志/追加告警文件即可，勿做耗时 IO）。
	OnTrip func(strategyID, reason string)
}

// breakerStateEnum 熔断三态（Sentinel 式）
type breakerStateEnum int

const (
	stateClosed   breakerStateEnum = iota // 正常：请求放行，滑动窗口统计
	stateOpen                             // 熔断：拒绝请求，等冷却期
	stateHalfOpen                         // 半开：只放 1 个探测请求试探
)

// CircuitBreaker 策略级熔断器
type CircuitBreaker struct {
	mu         sync.Mutex
	now        func() time.Time        // 可注入时钟（测试用）
	cfg        BreakerConfig
	strategies map[string]*breakerState
}

// breakerSample 单次请求样本
type breakerSample struct {
	at      time.Time
	success bool
	latency time.Duration
}

// breakerState 单个策略的熔断状态
type breakerState struct {
	samples       []breakerSample // 滑动窗口内样本
	state         breakerStateEnum // 当前状态：closed/open/halfOpen
	tripTime      time.Time        // 进入 Open 的时刻（冷却计时起点）
	probeInFlight bool             // Half-Open 下是否已有探测请求在途（只放 1 个）
}

// NewCircuitBreaker 创建熔断器；零配置用默认值
func NewCircuitBreaker(cfg BreakerConfig) *CircuitBreaker {
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5 * time.Minute
	}
	if cfg.ErrRate <= 0 {
		cfg.ErrRate = 0.10
	}
	if cfg.P99Limit <= 0 {
		cfg.P99Limit = 8 * time.Second
	}
	return &CircuitBreaker{
		now:        time.Now,
		cfg:        cfg,
		strategies: make(map[string]*breakerState),
	}
}

// state 获取（必要时创建）策略状态
func (cb *CircuitBreaker) state(id string) *breakerState {
	s, ok := cb.strategies[id]
	if !ok {
		s = &breakerState{state: stateClosed}
		cb.strategies[id] = s
	}
	return s
}

// Report 上报一次请求结果：成功/失败 + 延迟。
// 在滑动窗口内保留样本，并清理过期样本。
// Half-Open 下上报即探测结果：成功 → 回 Closed 全量恢复；失败 → 重新 Open 计时。
func (cb *CircuitBreaker) Report(strategyID string, success bool, latency time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s := cb.state(strategyID)
	cutoff := cb.now().Add(-cb.cfg.Window)
	// 只保留窗口内样本
	kept := s.samples[:0]
	for _, smp := range s.samples {
		if smp.at.After(cutoff) {
			kept = append(kept, smp)
		}
	}
	kept = append(kept, breakerSample{at: cb.now(), success: success, latency: latency})
	s.samples = kept

	// Half-Open 下：本次上报就是那个探测请求的结果
	if s.state == stateHalfOpen {
		s.probeInFlight = false
		if success {
			// 探测成功 → 上游已恢复 → 回 Closed，全量恢复（清空旧样本重新统计）
			s.state = stateClosed
			s.samples = nil
		} else {
			// 探测失败 → 上游未恢复 → 重新 Open 并重新计时（防二次雪崩）
			s.state = stateOpen
			s.tripTime = cb.now()
			cb.notifyTrip(strategyID, "half-open probe failed")
		}
	}
}

// notifyTrip 熔断触发回调（锁内调用，保持轻量）
func (cb *CircuitBreaker) notifyTrip(strategyID, reason string) {
	if cb.cfg.OnTrip != nil {
		cb.cfg.OnTrip(strategyID, reason)
	}
}

// CanUse 该策略当前是否可用。
// 三态判定：
//   - Closed：检查是否达到熔断条件（错误率/P99）→ 触发则转 Open 并拒绝
//   - Open：冷却期未过 → 拒绝；冷却期已过 → 转 Half-Open，放行 1 个探测
//   - Half-Open：已有探测在途 → 拒绝（只放 1 个）；否则放行探测
func (cb *CircuitBreaker) CanUse(strategyID string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s, ok := cb.strategies[strategyID]
	if !ok {
		return true // 没上报过样本 → 可用
	}

	switch s.state {
	case stateClosed:
		// 检查是否达到熔断条件
		if cb.tripCondition(s) {
			s.state = stateOpen
			s.tripTime = cb.now()
			cb.notifyTrip(strategyID, "err-rate/P99 exceeded")
			return false
		}
		return true

	case stateOpen:
		// 冷却期过了吗？过了 → 转 Half-Open 放行 1 个探测请求
		if cb.now().Sub(s.tripTime) >= cb.cfg.Cooldown {
			s.state = stateHalfOpen
			s.probeInFlight = true // 本次调用即探测请求
			return true
		}
		return false

	case stateHalfOpen:
		// 只放 1 个：已有探测在途则拒绝
		if s.probeInFlight {
			return false
		}
		s.probeInFlight = true
		return true
	}
	return true
}

// tripCondition 判定熔断条件：错误率 > 阈值 或 P99 延迟 > 上限
func (cb *CircuitBreaker) tripCondition(s *breakerState) bool {
	if len(s.samples) == 0 {
		return false
	}
	// 错误率
	errCount := 0
	for _, smp := range s.samples {
		if !smp.success {
			errCount++
		}
	}
	errRate := float64(errCount) / float64(len(s.samples))
	if errRate > cb.cfg.ErrRate {
		return true
	}
	// P99 延迟：样本太少（<10）时 P99 没有统计意义，只看错误率
	lat := make([]int, len(s.samples))
	for i, smp := range s.samples {
		lat[i] = int(smp.latency.Milliseconds())
	}
	if len(lat) < 10 {
		return false
	}
	sort.Ints(lat)
	idx := int(float64(len(lat)) * 0.99)
	if idx >= len(lat) {
		idx = len(lat) - 1
	}
	p99 := lat[idx]
	if time.Duration(p99)*time.Millisecond > cb.cfg.P99Limit {
		return true
	}
	return false
}

// IsTripped 该策略当前是否处于熔断保护态（Open 或 Half-Open）（展示用）
func (cb *CircuitBreaker) IsTripped(strategyID string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if s, ok := cb.strategies[strategyID]; ok {
		return s.state != stateClosed
	}
	return false
}

// StateName 返回当前状态名（展示用）：closed / open / half_open
func (cb *CircuitBreaker) StateName(strategyID string) string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if s, ok := cb.strategies[strategyID]; ok {
		switch s.state {
		case stateOpen:
			return "open"
		case stateHalfOpen:
			return "half_open"
		default:
			return "closed"
		}
	}
	return "closed"
}

// SetNow 注入时钟（测试用）
func (cb *CircuitBreaker) SetNow(f func() time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.now = f
}
