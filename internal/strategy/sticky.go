// internal/strategy/sticky.go
// v6 会话粘性路由：同一会话（Group）固定走同一渠道。
//
// 为什么值得做：
//   - 前缀缓存命中：同一用户连续请求走同一上游，前缀 KV 缓存命中率高，
//     直接省重复计费的 token
//   - 用户感知稳定：渠道不来回跳，链路一致
//   - 有兜底：绑定渠道不可用（不在候选/故障/熔断）时自动解除绑定换渠道
//
// 用法：NewSticky(inner) 包装任意策略，ID() 返回内部策略 ID，
// 所以可以直接注册进引擎顶替原策略（SelectChannel 用原 ID 就能命中）。
// 切换次数（Switches）是核心指标：粘性把"同一用户被切来切去"的次数降下来。

package strategy

import (
	"sync"

	"routing-arena-demo/internal/stats"
)

// StickyRouter 会话粘性包装
type StickyRouter struct {
	inner Router
	mu    sync.Mutex
	bind  map[string]int // group → 绑定的渠道 ID
	// 统计：同一 group 相邻请求换了渠道的次数（首次绑定不算）
	switches int
	last     map[string]int // group → 上次实际渠道
}

// NewSticky 创建粘性包装
func NewSticky(inner Router) *StickyRouter {
	return &StickyRouter{
		inner: inner,
		bind:  make(map[string]int),
		last:  make(map[string]int),
	}
}

// ID 返回内部策略 ID（可顶替原策略注册进引擎）
func (s *StickyRouter) ID() string { return s.inner.ID() }

// InnerID 内部策略 ID（展示用）
func (s *StickyRouter) InnerID() string { return s.inner.ID() }

// Switches 渠道切换次数（同一 group 换了渠道）
func (s *StickyRouter) Switches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switches
}

// Bound 当前绑定数（展示用）
func (s *StickyRouter) Bound() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bind)
}

// Pick 粘性选路：绑定渠道仍可用 → 直接用；否则重新选并绑定。
func (s *StickyRouter) Pick(rc *RouteContext, kb *stats.Knowledge, candidates []int) int {
	if rc.Group == "" {
		// 无会话标识 → 退化为内部策略原逻辑
		return s.inner.Pick(rc, kb, candidates)
	}

	s.mu.Lock()
	bound, ok := s.bind[rc.Group]
	s.mu.Unlock()

	// 绑定渠道还在候选里（未被水位/健康/样本过滤掉）→ 粘住
	if ok && contains(candidates, bound) {
		s.mu.Lock()
		if s.last[rc.Group] != 0 && s.last[rc.Group] != bound {
			s.switches++
		}
		s.last[rc.Group] = bound
		s.mu.Unlock()
		return bound
	}

	// 绑定失效或没有绑定 → 内部策略重新选
	chosen := s.inner.Pick(rc, kb, candidates)
	if chosen == 0 {
		return 0
	}
	s.mu.Lock()
	prev := s.last[rc.Group]
	if prev != 0 && prev != chosen {
		s.switches++
	}
	s.bind[rc.Group] = chosen
	s.last[rc.Group] = chosen
	s.mu.Unlock()
	return chosen
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
