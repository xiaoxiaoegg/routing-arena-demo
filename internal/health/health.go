// internal/health/health.go
// P3-1 健康探测模块：周期探测各渠道，产出实时延迟/可用性/错误率。
//
// 真实场景：零成本探测用上游 /models 免费接口（不产生 token 计费），
// 免打扰模式跳过指定渠道；demo 用可注入的 ProbeFunc 模拟。
//
// 产出喂给 S2_MinLat 打分——选路用"实时健康"而不是"历史平均"。

package health

import (
	"sync"
	"time"
)

// State 单个渠道的健康状态
type State struct {
	ChannelID  int     // 渠道 ID
	Up         bool    // 可用性
	LatencyMs  int     // 实时延迟（ms）
	ErrorRate  float64 // 近窗口错误率 [0,1]
	CheckedAt  time.Time // 探测时间
}

// ProbeFunc 探测函数：输入渠道 ID，返回该渠道健康状态。
// 真实场景由 HTTP 探测实现（/models 免费接口）；demo 注入模拟实现。
type ProbeFunc func(channelID int) State

// Prober 健康探测器：按周期探测一批渠道，缓存最新状态
type Prober struct {
	mu       sync.RWMutex
	interval time.Duration
	probe    ProbeFunc
	channels []int            // 要探测的渠道
	latest   map[int]State    // 渠道 ID → 最新状态
	skip     map[int]bool     // 免打扰模式：跳过指定渠道
}

// NewProber 创建探测器；probe 为 nil 时默认返回"健康"（不阻塞选路）
func NewProber(channels []int, interval time.Duration, probe ProbeFunc) *Prober {
	p := &Prober{
		interval: interval,
		probe:    probe,
		channels: channels,
		latest:   make(map[int]State),
		skip:     make(map[int]bool),
	}
	if p.probe == nil {
		p.probe = func(id int) State {
			return State{ChannelID: id, Up: true, LatencyMs: 500, ErrorRate: 0}
		}
	}
	return p
}

// Skip 免打扰模式：跳过指定渠道（不探测、不产生任何请求）
func (p *Prober) Skip(channelIDs ...int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range channelIDs {
		p.skip[id] = true
	}
}

// ProbeOnce 探测一轮所有渠道，更新缓存
func (p *Prober) ProbeOnce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.channels {
		if p.skip[id] {
			continue // 免打扰：跳过
		}
		p.latest[id] = p.probe(id)
	}
}

// Get 取某渠道最新状态（探测前返回默认健康态）
func (p *Prober) Get(channelID int) State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if s, ok := p.latest[channelID]; ok {
		return s
	}
	return State{ChannelID: channelID, Up: true, LatencyMs: 500, ErrorRate: 0}
}

// HealthyChannels 返回当前可用的渠道列表（Up=true）
func (p *Prober) HealthyChannels() []int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []int
	for _, id := range p.channels {
		if p.skip[id] {
			continue
		}
		if s, ok := p.latest[id]; ok {
			if s.Up {
				out = append(out, id)
			}
		} else {
			out = append(out, id) // 未探测默认健康
		}
	}
	return out
}

// All 返回全部渠道最新状态（看板展示用）
func (p *Prober) All() []State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []State
	for _, id := range p.channels {
		if s, ok := p.latest[id]; ok {
			out = append(out, s)
		} else {
			out = append(out, State{ChannelID: id, Up: true, LatencyMs: 500, ErrorRate: 0})
		}
	}
	return out
}
