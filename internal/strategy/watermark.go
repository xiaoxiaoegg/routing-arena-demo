// internal/strategy/watermark.go
// v5 新增：并发水位拦截（对齐 Higress 的 ActiveStreams 水位线概念）。
//
// 针对大模型长连接特性：S3/S4 算分前先检查各渠道当前活跃并发数，
// 并发水位超过配额 90% 的渠道直接踢出候选池，不给算分机会。
// 防止把请求发给已堵死的渠道导致首字延迟 (TTFT) 飙升。

package strategy

import (
	"sync"
)

// Watermark 并发水位拦截器
type Watermark struct {
	mu     sync.RWMutex
	active map[int]int // 渠道 ID → 当前活跃并发数
	quota  map[int]int // 渠道 ID → 并发配额（0 = 无限制）
}

// NewWatermark 创建水位拦截器；quota 渠道配额，未知渠道按无限制
func NewWatermark(quota map[int]int) *Watermark {
	return &Watermark{active: map[int]int{}, quota: quota}
}

// SetActive 直接设置某渠道的活跃并发数（场景注入/测试用：
// 真实环境由网关的 ActiveStreams 计数维护，demo 里由 Acquire/Release 或本方法维护）
func (w *Watermark) SetActive(chID, n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n <= 0 {
		delete(w.active, chID)
		return
	}
	w.active[chID] = n
}

// Acquire 请求发给某渠道前调用：活跃并发 +1
func (w *Watermark) Acquire(chID int) {
	w.mu.Lock()
	w.active[chID]++
	w.mu.Unlock()
}

// Release 请求结束调用：活跃并发 -1（不低于 0）
func (w *Watermark) Release(chID int) {
	w.mu.Lock()
	if w.active[chID] > 0 {
		w.active[chID]--
	}
	w.mu.Unlock()
}

// Allow 该渠道是否可用：活跃并发 < 配额*90% 才放行；无配额或未知渠道放行
func (w *Watermark) Allow(chID int) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	q, ok := w.quota[chID]
	if !ok || q <= 0 {
		return true // 无配额限制
	}
	return w.active[chID] < q*9/10 // 90% 水位线（整数运算，避免浮点）
}

// Active 当前各渠道活跃并发（展示用）
func (w *Watermark) Active() map[int]int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make(map[int]int, len(w.active))
	for id, n := range w.active {
		out[id] = n
	}
	return out
}
