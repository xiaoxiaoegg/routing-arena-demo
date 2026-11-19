// internal/stats/aggregate_test.go
// 聚合器单元测试：用已知数据手算验证成本/延迟统计正确。

package stats

import (
	"testing"

	"routing-arena-demo/internal/logreader"
)

func TestAggregate(t *testing.T) {
	recs := []logreader.LogRecord{
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 2.0, UseTimeMs: 2000},
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 2.0, UseTimeMs: 2000},
		{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 1.0, UseTimeMs: 1000},
	}
	k := Aggregate(recs)

	c1 := k.ByChannelModel["1:gpt-4o"]
	if c1 == nil {
		t.Fatal("渠道1统计缺失")
	}
	if c1.Requests != 2 {
		t.Errorf("渠道1请求数应为 2，实际 %d", c1.Requests)
	}
	// 千 token 成本 = 总花费 / 总token * 1000 = 4 / 4000 * 1000 = 1.0
	if c1.AvgCostPer1K != 1.0 {
		t.Errorf("渠道1千token成本应为 1.0，实际 %v", c1.AvgCostPer1K)
	}
	// 平均延迟 = 2000
	if c1.AvgLatencyMs != 2000 {
		t.Errorf("渠道1平均延迟应为 2000，实际 %d", c1.AvgLatencyMs)
	}

	c2 := k.ByChannelModel["2:gpt-4o"]
	if c2 == nil {
		t.Fatal("渠道2统计缺失")
	}
	// 千 token 成本 = 1 / 2000 * 1000 = 0.5
	if c2.AvgCostPer1K != 0.5 {
		t.Errorf("渠道2千token成本应为 0.5，实际 %v", c2.AvgCostPer1K)
	}
	t.Logf("渠道1: %+v", c1)
	t.Logf("渠道2: %+v", c2)
}
