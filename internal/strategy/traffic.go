// internal/strategy/traffic.go
// 流量分配器：多策略按比例分桶——管理员在后台设定各策略承接多少真实流量。
//
// 对比单候选 Bucket（S0 + 一个候选）：
//   Bucket 只能切一个候选；TrafficSplitter 支持 N 个策略任意比例，
//   总和必须 100%（可热更新，管理后台"流量分配器"面板）。
//
// 用法：
//   ts := NewTrafficSplitter(map[string]float64{"S0_Champion": 80, "S3_Hybrid": 15, "S1_MinCost": 5})
//   ts.SetWeights(map[string]float64{...})   // 热更新（保存即生效）
//   ts.PickStrategy()                        // 按比例随机选一个策略

package strategy

import (
	"fmt"
	"math/rand"
	"sort"
)

// TrafficSplitter 流量分配器
type TrafficSplitter struct {
	rng     *rand.Rand             // 独立随机源（WithSeed 可复现）
	weights map[string]float64     // 策略 ID → 流量比例（%）
	total   float64                // 权重总和（内部归一化用）
	order   []string               // 策略顺序（确定性遍历）
}

// NewTrafficSplitter 创建分配器；weights 为 策略→比例（总和应≈100）
func NewTrafficSplitter(weights map[string]float64) *TrafficSplitter {
	ts := &TrafficSplitter{weights: weights}
	ts.rebuild()
	return ts
}

// WithSeed 注入独立随机源（Go 1.20+ 全局 rand.Seed 已失效）
func (ts *TrafficSplitter) WithSeed(seed int64) *TrafficSplitter {
	ts.rng = rand.New(rand.NewSource(seed))
	return ts
}

// Weights 返回当前比例副本（展示用）
func (ts *TrafficSplitter) Weights() map[string]float64 {
	out := make(map[string]float64, len(ts.weights))
	for id, w := range ts.weights {
		out[id] = w
	}
	return out
}

// Total 返回比例总和（应≈100）
func (ts *TrafficSplitter) Total() float64 { return ts.total }

// SetWeights 热更新比例（管理后台保存即生效）
func (ts *TrafficSplitter) SetWeights(weights map[string]float64) {
	ts.weights = weights
	ts.rebuild()
}

// rebuild 重新计算总权重与策略顺序
func (ts *TrafficSplitter) rebuild() {
	ts.total = 0
	ts.order = nil
	for id, w := range ts.weights {
		ts.total += w
		ts.order = append(ts.order, id)
	}
	sort.Strings(ts.order) // 确定性顺序
}

// PickStrategy 按比例随机选一个策略
func (ts *TrafficSplitter) PickStrategy() string {
	if ts.total <= 0 {
		return ""
	}
	r := ts.randFloat() * ts.total
	acc := 0.0
	for _, id := range ts.order {
		acc += ts.weights[id]
		if r < acc {
			return id
		}
	}
	// 浮点误差兜底：返回最后一个
	if len(ts.order) > 0 {
		return ts.order[len(ts.order)-1]
	}
	return ""
}

// randFloat 生成 [0,1) 随机数（有 rng 用独立源，否则全局）
func (ts *TrafficSplitter) randFloat() float64 {
	if ts.rng != nil {
		return ts.rng.Float64()
	}
	return rand.Float64()
}

// Validate 校验比例是否合法（总和 100% ± 容差，且每项非负）
func Validate(weights map[string]float64) error {
	if len(weights) == 0 {
		return fmt.Errorf("分配器不能为空")
	}
	total := 0.0
	for id, w := range weights {
		if w < 0 {
			return fmt.Errorf("策略 %s 比例 %v 为负", id, w)
		}
		total += w
	}
	if total < 99 || total > 101 {
		return fmt.Errorf("比例总和应为 100%%，实际 %.1f%%", total)
	}
	return nil
}
