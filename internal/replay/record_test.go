// internal/replay/record_test.go
// P1-5 评估数据管道单测：逐条记录的策略/渠道/回退标志/反事实成本。

package replay

import (
	"testing"

	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

// mkEvalKB 知识库：渠道1 便宜(0.25)，渠道2 快(100ms)，样本充足
func mkEvalKB() *stats.Knowledge {
	var recs []logreader.LogRecord
	for i := 0; i < 20; i++ {
		recs = append(recs,
			logreader.LogRecord{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
			logreader.LogRecord{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 1.2, UseTimeMs: 100},
		)
	}
	return stats.Aggregate(recs)
}

// mkEvalRecs 回放记录：实际都走了渠道1（现状）
func mkEvalRecs() []logreader.LogRecord {
	return []logreader.LogRecord{
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
	}
}

func TestRunEngineDefaultS0Records(t *testing.T) {
	// 默认（无分桶）：每条记录都是 S0 决策、走实际渠道、无回退
	kb := mkEvalKB()
	recs := mkEvalRecs()
	eng := strategy.NewEngine()

	records, res := RunEngine(recs, kb, eng, nil, nil)
	if len(records) != 2 {
		t.Fatalf("应产生 2 条记录，实际 %d", len(records))
	}
	for i, r := range records {
		if r.StrategyID != "S0_Champion" || r.IntendedID != "S0_Champion" {
			t.Errorf("记录%d 应为 S0 决策，实际 %s (intended %s)", i, r.StrategyID, r.IntendedID)
		}
		if r.Fallback {
			t.Errorf("记录%d 不应有回退（S0 是现状基线）", i)
		}
		if r.ChannelID != 1 {
			t.Errorf("记录%d 应走实际渠道1，实际 %d", i, r.ChannelID)
		}
	}
	if res.Requests != 2 || res.TotalCost <= 0 {
		t.Errorf("汇总异常: requests=%d cost=%v", res.Requests, res.TotalCost)
	}
}

func TestRunEngineS1BucketRecords(t *testing.T) {
	// 100% 分桶给 S1：每条记录都是 S1 决策、选最便宜渠道1、无回退
	kb := mkEvalKB()
	recs := mkEvalRecs()
	eng := strategy.NewEngine()
	bucket := strategy.NewBucket(1.0, "S1_MinCost")

	records, res := RunEngine(recs, kb, eng, bucket, nil)
	if len(records) != 2 {
		t.Fatalf("应产生 2 条记录，实际 %d", len(records))
	}
	for i, r := range records {
		if r.StrategyID != "S1_MinCost" || r.IntendedID != "S1_MinCost" {
			t.Errorf("记录%d 应为 S1 决策，实际 %s (intended %s)", i, r.StrategyID, r.IntendedID)
		}
		if r.Fallback {
			t.Errorf("记录%d 不应有回退（S1 样本充足）", i)
		}
		if r.ChannelID != 1 {
			t.Errorf("记录%d S1 应选最便宜渠道1，实际 %d", i, r.ChannelID)
		}
	}
	if res.Requests != 2 {
		t.Errorf("汇总 requests 应为 2，实际 %d", res.Requests)
	}
}

func TestRunEngineFallbackRecorded(t *testing.T) {
	// 候选策略样本不足选不出 → 回退 S0，记录 Fallback=true
	// 知识库：渠道1/2 都只有 3 条样本（<10），S1 拒绝参与
	var recs []logreader.LogRecord
	for i := 0; i < 3; i++ {
		recs = append(recs,
			logreader.LogRecord{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
			logreader.LogRecord{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 1.2, UseTimeMs: 100},
		)
	}
	kb := stats.Aggregate(recs)

	// 回放 2 条（实际走渠道1），100% 分桶 S1
	playRecs := []logreader.LogRecord{
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
	}
	eng := strategy.NewEngine()
	bucket := strategy.NewBucket(1.0, "S1_MinCost")

	records, _ := RunEngine(playRecs, kb, eng, bucket, nil)
	if len(records) != 2 {
		t.Fatalf("应产生 2 条记录，实际 %d", len(records))
	}
	for i, r := range records {
		if r.StrategyID != "S0_Champion" {
			t.Errorf("记录%d 回退后应生效 S0，实际 %s", i, r.StrategyID)
		}
		if !r.Fallback {
			t.Errorf("记录%d 应标记 Fallback=true", i)
		}
		if r.ChannelID == 0 {
			t.Errorf("记录%d 回退后必须选到渠道，不能丢请求", i)
		}
	}
}

func TestRunEngineCostMatchesKnowledge(t *testing.T) {
	// 反事实成本验证：S1 选渠道1，知识库渠道1 千token成本 X，
	// 本条 2000 token → cost = X/1000*2000
	kb := mkEvalKB()
	cs := kb.ByChannelModel["1:gpt-4o"]
	recs := mkEvalRecs()
	eng := strategy.NewEngine()
	bucket := strategy.NewBucket(1.0, "S1_MinCost")

	records, _ := RunEngine(recs, kb, eng, bucket, nil)
	for i, r := range records {
		expect := cs.AvgCostPer1K / 1000 * float64(r.PromptTokens+r.CompletionTokens)
		if r.Cost != expect {
			t.Errorf("记录%d 成本应为 %.4f，实际 %.4f", i, expect, r.Cost)
		}
	}
}
