// internal/replay/attribution_test.go
// v5 新增：严格归因测试。
//
// 核心语义：策略选了渠道 A 失败重试切到 B 成功，
//   A(失败) + B(重试) 的总耗时和总 Token 消耗全部算在策略头上。
// 这逼迫策略"选得准"——乱选的代价（重试成本）必须自己扛。

package replay

import (
	"testing"

	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

// mkAttrKB 知识库：渠道1 便宜(0.25)但总失败，渠道2 贵(1.2)但成功
func mkAttrKB() *stats.Knowledge {
	var recs []logreader.LogRecord
	for i := 0; i < 20; i++ {
		recs = append(recs,
			logreader.LogRecord{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
			logreader.LogRecord{ChannelID: 2, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 2.4, UseTimeMs: 2000},
		)
	}
	return stats.Aggregate(recs)
}

func mkAttrRecs() []logreader.LogRecord {
	return []logreader.LogRecord{
		{ChannelID: 1, Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 1000, Quota: 0.5, UseTimeMs: 2000},
	}
}

// TestStrictAttributionOnRetry：S1 选渠道1（便宜）→ 渠道1 失败 → 重试渠道2。
// 严格归因：成本 = 渠道1 + 渠道2 都要算在 S1 头上。
func TestStrictAttributionOnRetry(t *testing.T) {
	kb := mkAttrKB()
	recs := mkAttrRecs()
	eng := strategy.NewEngine()
	bucket := strategy.NewBucket(1.0, "S1_MinCost")

	// 渠道1 总是失败（successFn：chosen==1 → false），渠道2 成功
	records, res := RunEngine(recs, kb, eng, bucket, func(_ logreader.LogRecord, chosen int) bool {
		return chosen != 1
	})

	if len(records) != 1 {
		t.Fatalf("应产生 1 条记录，实际 %d", len(records))
	}
	r := records[0]

	// 1. 发生了重试（Retried=true）
	if !r.Retried {
		t.Fatal("渠道1 失败应触发重试（Retried=true）")
	}
	// 2. 成本 = 渠道1(失败) + 渠道2(重试)，全算在策略头上
	cs1 := kb.ByChannelModel["1:gpt-4o"]
	cs2 := kb.ByChannelModel["2:gpt-4o"]
	tokens := 2000
	cost1 := cs1.AvgCostPer1K / 1000 * float64(tokens)
	cost2 := cs2.AvgCostPer1K / 1000 * float64(tokens)
	want := cost1 + cost2
	if r.Cost < want-1e-9 || r.Cost > want+1e-9 {
		t.Errorf("严格归因成本应为 渠道1+渠道2 = %.4f，实际 %.4f", want, r.Cost)
	}
	// 3. 重试额外成本单独记录（展示用）
	if r.RetryCost < cost2-1e-9 || r.RetryCost > cost2+1e-9 {
		t.Errorf("RetryCost 应为渠道2成本 %.4f，实际 %.4f", cost2, r.RetryCost)
	}
	// 4. 最终成功（重试后用户侧成功）
	if !r.Success {
		t.Error("重试后最终应成功")
	}
	// 5. 汇总也把重试代价算进去了
	if res.TotalCost < want-1e-9 || res.TotalCost > want+1e-9 {
		t.Errorf("汇总成本应含重试代价 %.4f，实际 %.4f", want, res.TotalCost)
	}
	t.Logf("严格归因: S1 选渠道1失败(%v) → 重试渠道2(%v) → 总成本 %.4f 全算 S1 头上",
		cost1, cost2, r.Cost)
}

// TestNoRetryWhenSuccess：渠道不失败 → 无重试，成本不变（无副作用）
func TestNoRetryWhenSuccess(t *testing.T) {
	kb := mkAttrKB()
	recs := mkAttrRecs()
	eng := strategy.NewEngine()
	bucket := strategy.NewBucket(1.0, "S1_MinCost")

	// 全成功（successFn 返回 true）
	records, _ := RunEngine(recs, kb, eng, bucket, func(_ logreader.LogRecord, _ int) bool { return true })
	if len(records) != 1 {
		t.Fatalf("应产生 1 条记录，实际 %d", len(records))
	}
	if records[0].Retried {
		t.Fatal("成功请求不应触发重试")
	}
	// 成本 = 渠道1 单次成本（无重试）
	cs1 := kb.ByChannelModel["1:gpt-4o"]
	want := cs1.AvgCostPer1K / 1000 * 2000
	if records[0].Cost != want {
		t.Errorf("无重试成本应为 %.4f，实际 %.4f", want, records[0].Cost)
	}
}

// TestRetryCountsLatencyToo：重试延迟也要累加（A+B 耗时全算）
func TestRetryCountsLatencyToo(t *testing.T) {
	kb := mkAttrKB()
	recs := mkAttrRecs()
	eng := strategy.NewEngine()
	bucket := strategy.NewBucket(1.0, "S1_MinCost")

	records, _ := RunEngine(recs, kb, eng, bucket, func(_ logreader.LogRecord, chosen int) bool { return chosen != 1 })
	if len(records) != 1 {
		t.Fatalf("应产生 1 条记录，实际 %d", len(records))
	}
	r := records[0]
	if !r.Retried {
		t.Fatal("应触发重试")
	}
	wantLat := kb.ByChannelModel["1:gpt-4o"].AvgLatencyMs + kb.ByChannelModel["2:gpt-4o"].AvgLatencyMs
	if r.LatencyMs != wantLat {
		t.Errorf("重试延迟应累加为 %d，实际 %d", wantLat, r.LatencyMs)
	}
	t.Logf("严格归因延迟: %dms（失败+重试累计）", r.LatencyMs)
}
