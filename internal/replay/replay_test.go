// internal/replay/replay_test.go
// 回放引擎测试：验证反事实成本计算正确。

package replay

import (
	"testing"

	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

func TestRunS1CounterfactualCost(t *testing.T) {
	// 知识库：渠道1 便宜（千token 0.5），渠道2 贵（千token 2.0），各 10 条样本
	var recs []logreader.LogRecord
	for i := 0; i < 10; i++ {
		recs = append(recs, logreader.LogRecord{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 1.0, UseTimeMs: 2000})
		recs = append(recs, logreader.LogRecord{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 4.0, UseTimeMs: 2000})
	}
	kb := stats.Aggregate(recs)

	// 回放 4 条记录（实际渠道都是贵的渠道2）
	replayRecs := []logreader.LogRecord{
		{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 4.0, UseTimeMs: 2000},
		{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 4.0, UseTimeMs: 2000},
		{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 4.0, UseTimeMs: 2000},
		{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 4.0, UseTimeMs: 2000},
	}

	s1 := &strategy.S1MinCost{}
	res := Run(replayRecs, kb, s1, func(rc *strategy.RouteContext, kb *stats.Knowledge, candidates []int) int {
		return s1.Pick(rc, kb, candidates)
	})

	// S1 会选渠道1，反事实成本 = 0.5/1000 * 2000 = 1.0 元/条 × 4 条 = 4.0
	if res.TotalCost != 4.0 {
		t.Errorf("S1 反事实总成本应为 4.0，实际 %v", res.TotalCost)
	}
	if res.Requests != 4 {
		t.Errorf("应处理 4 条，实际 %d", res.Requests)
	}
	t.Logf("S1 反事实总成本: %v (4条, 每条1.0)", res.TotalCost)
}
