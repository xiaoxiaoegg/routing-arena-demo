// internal/metrics/metrics_test.go
// 极简 Prometheus 指标测试：bucket 计数、exemplar 输出、文本格式。

package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestHistogramBucketing(t *testing.T) {
	h := NewHistogram("req_latency_ms", "请求延迟", []float64{100, 300, 800}, map[string]string{"strategy_id": "S1_MinCost"})
	h.Observe(50, nil)   // bucket 100
	h.Observe(150, nil)  // bucket 300
	h.Observe(250, nil)  // bucket 300
	h.Observe(1500, nil) // +Inf

	if h.Count() != 4 {
		t.Fatalf("count 应为 4，实际 %d", h.Count())
	}

	var buf bytes.Buffer
	h.WriteText(&buf)
	out := buf.String()

	// 各 bucket 计数正确
	checks := map[string]string{
		`req_latency_ms_bucket{le="100",strategy_id="S1_MinCost"} 1`: "",
		`req_latency_ms_bucket{le="300",strategy_id="S1_MinCost"} 3`: "",
		`req_latency_ms_bucket{le="800",strategy_id="S1_MinCost"} 3`: "",
		`req_latency_ms_bucket{le="+Inf",strategy_id="S1_MinCost"} 4`: "",
		`req_latency_ms_count{strategy_id="S1_MinCost"} 4`:            "",
	}
	for want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少: %s\n实际:\n%s", want, out)
		}
	}
}

func TestHistogramExemplar(t *testing.T) {
	h := NewHistogram("req_latency_ms", "请求延迟", []float64{100, 500}, nil)
	// 高延迟毛刺挂 trace_id
	h.Observe(4000, map[string]string{"trace_id": "trace-abc-123"})

	var buf bytes.Buffer
	h.WriteText(&buf)
	out := buf.String()

	if !strings.Contains(out, `# {trace_id="trace-abc-123"}`) {
		t.Errorf("高延迟 bucket 应带 exemplar 注释:\n%s", out)
	}
	// 毛刺应落在 +Inf bucket（>500）
	if !strings.Contains(out, `le="+Inf"}`+" 1") {
		t.Errorf("毛刺应落在 +Inf bucket:\n%s", out)
	}
}

func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram("empty_metric", "空指标", []float64{10, 20}, nil)
	var buf bytes.Buffer
	h.WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "# TYPE empty_metric histogram") {
		t.Errorf("空直方图也要输出 TYPE 行:\n%s", out)
	}
	if !strings.Contains(out, `le="+Inf"}`+" 0") {
		t.Errorf("空直方图 +Inf 应为 0:\n%s", out)
	}
}

func TestHistogramSumAndCount(t *testing.T) {
	h := NewHistogram("m", "x", []float64{10}, nil)
	h.Observe(5, nil)
	h.Observe(15, nil)
	var buf bytes.Buffer
	h.WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "m_sum 20") {
		t.Errorf("sum 应为 20:\n%s", out)
	}
	if !strings.Contains(out, "m_count 2") {
		t.Errorf("count 应为 2:\n%s", out)
	}
}
