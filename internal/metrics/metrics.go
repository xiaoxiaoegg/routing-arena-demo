// internal/metrics/metrics.go
// 极简 Prometheus 指标：直方图（Histogram）+ Exemplar 标签。
//
// 目的（v6 可观测性，无第三方依赖）：
//   - 网关要能回答"延迟分布长什么样、哪个策略慢"——用延迟直方图（bucket 计数）
//   - Exemplar：P99 毛刺出现时，把 trace_id 挂在对应 bucket 上，看板能"点毛刺
//     下钻到具体那条请求"（v5 方案里 OpenTelemetry Exemplar 的 demo 版）
//
// 输出为 Prometheus 文本协议 0.0.4 / OpenMetrics 兼容格式：
//   name_bucket{le="300",strategy_id="S1_MinCost"} 42  # {trace_id="xxx"} 1
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
)

// Histogram 直方图：固定 bucket 上界（毫秒）+ 固定标签 + 可选 Exemplar
type Histogram struct {
	name    string
	help    string
	buckets []float64
	labels  map[string]string // 固定标签（如 strategy_id）
	mu      sync.Mutex
	counts  []uint64
	sum     float64
	count   uint64
	// exemplar：每个 bucket 最近一次观察的 trace_id（OpenMetrics 行内注释）
	exemplar map[int]map[string]string
}

// NewHistogram 创建直方图；buckets 为升序上界（ms），自动补 +Inf
func NewHistogram(name, help string, buckets []float64, labels map[string]string) *Histogram {
	sorted := append([]float64{}, buckets...)
	sort.Float64s(sorted)
	return &Histogram{
		name:     name,
		help:     help,
		buckets:  sorted,
		labels:   labels,
		counts:   make([]uint64, len(sorted)+1),
		exemplar: map[int]map[string]string{},
	}
}

// Observe 记录一次观测（ms）；exemplar 可为 nil，或带 trace_id 等关联标签
func (h *Histogram) Observe(v float64, exemplar map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.count++
	idx := len(h.buckets) // +Inf
	for i, b := range h.buckets {
		if v <= b {
			idx = i
			break
		}
	}
	h.counts[idx]++
	if len(exemplar) > 0 {
		h.exemplar[idx] = exemplar
	}
}

// WriteText 输出 Prometheus 文本格式
func (h *Histogram) WriteText(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()

	labelStr := labelString(h.labels)

	fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
	cum := uint64(0)
	for i, b := range h.buckets {
		cum += h.counts[i] // Prometheus bucket 是累计计数（le 表示 ≤ 上界）
		le := strconv.FormatFloat(b, 'g', -1, 64)
		fmt.Fprintf(w, "%s_bucket{le=%q%s} %d", h.name, le, commaLabels(labelStr), cum)
		if ex, ok := h.exemplar[i]; ok && len(ex) > 0 {
			fmt.Fprintf(w, " # {%s} %d", labelString(ex), cum)
		}
		fmt.Fprintln(w)
	}
	// +Inf bucket = 全部样本
	cum += h.counts[len(h.buckets)]
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"%s} %d", h.name, commaLabels(labelStr), cum)
	if ex, ok := h.exemplar[len(h.buckets)]; ok && len(ex) > 0 {
		fmt.Fprintf(w, " # {%s} %d", labelString(ex), cum)
	}
	fmt.Fprintln(w)
	if labelStr == "" {
		fmt.Fprintf(w, "%s_sum %s\n", h.name, formatFloat(h.sum))
		fmt.Fprintf(w, "%s_count %d\n", h.name, h.count)
	} else {
		fmt.Fprintf(w, "%s_sum{%s} %s\n", h.name, labelStr, formatFloat(h.sum))
		fmt.Fprintf(w, "%s_count{%s} %d\n", h.name, labelStr, h.count)
	}
}

// Count 总观测数（测试/展示用）
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// labelString 渲染 `k="v",k2="v2"` 形式的标签段（空 map 返回空串，无前导逗号）
func labelString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for _, k := range keys {
		if out != "" {
			out += ","
		}
		out += fmt.Sprintf(`%s="%s"`, k, labels[k])
	}
	return out
}

// commaLabels 给非空标签段加前导逗号（bucket 行在 le= 之后用）
func commaLabels(labels string) string {
	if labels == "" {
		return ""
	}
	return "," + labels
}

// formatFloat 输出紧凑浮点（去掉无意义的小数尾巴）
func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}
