// internal/health/health_test.go
// P3-1 健康探测模块单测。

package health

import (
	"testing"
	"time"
)

func TestProbeDefaultHealthy(t *testing.T) {
	// 未探测时默认健康（不阻塞选路）
	p := NewProber([]int{1, 2, 3}, time.Minute, nil)
	if len(p.HealthyChannels()) != 3 {
		t.Fatalf("默认应全部健康，实际 %v", p.HealthyChannels())
	}
	st := p.Get(1)
	if !st.Up || st.LatencyMs <= 0 {
		t.Fatalf("默认状态应为健康，实际 %+v", st)
	}
}

func TestProbeOnceUpdatesState(t *testing.T) {
	// 探测一轮后缓存更新
	var calls int
	p := NewProber([]int{1, 2}, time.Minute, func(id int) State {
		calls++
		if id == 2 {
			return State{ChannelID: 2, Up: false, LatencyMs: 0, ErrorRate: 1.0}
		}
		return State{ChannelID: 1, Up: true, LatencyMs: 800, ErrorRate: 0.01}
	})
	p.ProbeOnce()
	if calls != 2 {
		t.Fatalf("应探测 2 个渠道，实际 %d", calls)
	}
	if !p.Get(1).Up || p.Get(1).LatencyMs != 800 {
		t.Fatalf("渠道1 应健康且延迟800ms，实际 %+v", p.Get(1))
	}
	if p.Get(2).Up {
		t.Fatalf("渠道2 应标记不可用，实际 %+v", p.Get(2))
	}
	// HealthyChannels 应排除渠道2
	hc := p.HealthyChannels()
	if len(hc) != 1 || hc[0] != 1 {
		t.Fatalf("可用渠道应为 [1]，实际 %v", hc)
	}
}

func TestSkipExcludes(t *testing.T) {
	// 免打扰模式：跳过指定渠道（不探测）
	var calls int
	p := NewProber([]int{1, 2}, time.Minute, func(id int) State {
		calls++
		return State{ChannelID: id, Up: true, LatencyMs: 100}
	})
	p.Skip(2)
	p.ProbeOnce()
	if calls != 1 {
		t.Fatalf("跳过渠道2后应只探测1个，实际 %d", calls)
	}
	// 跳过的渠道不进 HealthyChannels
	if hc := p.HealthyChannels(); len(hc) != 1 || hc[0] != 1 {
		t.Fatalf("可用渠道应为 [1]（渠道2被跳过），实际 %v", hc)
	}
}
