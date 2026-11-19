// internal/strategy/sticky_test.go
// v6 会话粘性路由测试：同会话固定渠道、不可用时换渠道、切换计数。

package strategy

import (
	"testing"

	"routing-arena-demo/internal/stats"
)

// mkStickyKB 渠道1 最便宜（S1 会选它），渠道2 次之
func mkStickyKB() *stats.Knowledge {
	return &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"1:gpt-4o": {ChannelID: 1, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.10, AvgLatencyMs: 500},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.30, AvgLatencyMs: 900},
	}}
}

func TestStickySameSessionSameChannel(t *testing.T) {
	kb := mkStickyKB()
	s := NewSticky(&S1MinCost{})
	rc := &RouteContext{Model: "gpt-4o", Group: "user-001"}

	first := s.Pick(rc, kb, []int{1, 2})
	second := s.Pick(rc, kb, []int{1, 2})
	if first != second {
		t.Fatalf("同一会话应固定渠道: 第一次 %d 第二次 %d", first, second)
	}
	if s.Switches() != 0 {
		t.Errorf("连续同渠道不应计切换，实际 %d", s.Switches())
	}
	if s.Bound() != 1 {
		t.Errorf("应绑定 1 个会话，实际 %d", s.Bound())
	}
}

func TestStickySwitchesWhenUnavailable(t *testing.T) {
	kb := mkStickyKB()
	s := NewSticky(&S1MinCost{})
	rc := &RouteContext{Model: "gpt-4o", Group: "user-002"}

	s.Pick(rc, kb, []int{1, 2})      // 绑定渠道1
	s.Pick(rc, kb, []int{1, 2})      // 仍渠道1
	s.Pick(rc, kb, []int{2})         // 渠道1 不可用（被过滤）→ 换渠道2
	if got := s.Pick(rc, kb, []int{2}); got != 2 {
		t.Errorf("渠道1 不可用时应换渠道2，实际 %d", got)
	}
	if s.Switches() != 1 {
		t.Errorf("应恰好切换 1 次，实际 %d", s.Switches())
	}
}

func TestStickySessionsIndependent(t *testing.T) {
	kb := mkStickyKB()
	s := NewSticky(&S1MinCost{})
	ra := &RouteContext{Model: "gpt-4o", Group: "user-a"}
	rb := &RouteContext{Model: "gpt-4o", Group: "user-b"}

	a1 := s.Pick(ra, kb, []int{1, 2})
	b1 := s.Pick(rb, kb, []int{1, 2})
	a2 := s.Pick(ra, kb, []int{1, 2})
	if a1 != a2 || a1 != b1 {
		t.Errorf("各会话独立绑定: a1=%d a2=%d b1=%d", a1, a2, b1)
	}
	if s.Bound() != 2 {
		t.Errorf("应绑定 2 个会话，实际 %d", s.Bound())
	}
}

func TestStickyNoGroupFallsThrough(t *testing.T) {
	kb := mkStickyKB()
	s := NewSticky(&S1MinCost{})
	rc := &RouteContext{Model: "gpt-4o"} // 无 Group
	got := s.Pick(rc, kb, []int{1, 2})
	if got != 1 {
		t.Errorf("无 Group 应退化为内部策略（S1 选渠道1），实际 %d", got)
	}
	if s.Switches() != 0 || s.Bound() != 0 {
		t.Errorf("无 Group 不应产生绑定/切换: switches=%d bound=%d", s.Switches(), s.Bound())
	}
}
