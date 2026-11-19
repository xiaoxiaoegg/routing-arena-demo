// internal/stats/split_test.go
// 训练/回放分离测试：保证样本外评估的数据切分正确。

package stats

import (
	"testing"

	"routing-arena-demo/internal/logreader"
)

func mkRecs(n int) []logreader.LogRecord {
	out := make([]logreader.LogRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, logreader.LogRecord{
			Time: int64(1700000000 + i*60), Model: "gpt-4o", ChannelID: 1,
			PromptTokens: 100, CompletionTokens: 50, Quota: 0.1, UseTimeMs: 100,
		})
	}
	return out
}

// TestSplitRatioAndOrder 切分比例正确、时间有序、两段不重叠
func TestSplitRatioAndOrder(t *testing.T) {
	recs := mkRecs(100)
	train, test := Split(recs, 0.7)
	if len(train) != 70 || len(test) != 30 {
		t.Fatalf("切分比例不对: train=%d test=%d", len(train), len(test))
	}
	if train[len(train)-1].Time >= test[0].Time {
		t.Errorf("训练集应完全早于回放集: trainMax=%d testMin=%d",
			train[len(train)-1].Time, test[0].Time)
	}
	// 两段加起来等于全集（无丢失、无重复）
	if len(train)+len(test) != len(recs) {
		t.Errorf("切分后总数变了: %d + %d != %d", len(train), len(test), len(recs))
	}
	seen := map[int64]bool{}
	for _, r := range append(append([]logreader.LogRecord{}, train...), test...) {
		if seen[r.Time] {
			t.Fatalf("记录重复: %d", r.Time)
		}
		seen[r.Time] = true
	}
}

// TestSplitSortsUnsortedInput 乱序输入也应按时间切分，且不改动入参
func TestSplitSortsUnsortedInput(t *testing.T) {
	recs := mkRecs(10)
	// 反转成逆序
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	firstBefore := recs[0].Time

	train, test := Split(recs, 0.5)
	if len(train) != 5 || len(test) != 5 {
		t.Fatalf("切分比例不对: %d/%d", len(train), len(test))
	}
	for i := 1; i < len(train); i++ {
		if train[i].Time < train[i-1].Time {
			t.Error("训练集未按时间排序")
		}
	}
	if train[0].Time > test[0].Time {
		t.Error("训练集应早于回放集")
	}
	if recs[0].Time != firstBefore {
		t.Error("Split 不应改动入参切片顺序")
	}
}

// TestSplitEdgeCases 边界：比例越界钳制、数据过少
func TestSplitEdgeCases(t *testing.T) {
	// 比例越界 → 钳制到 [0.1, 0.9]
	train, test := Split(mkRecs(100), 0.0)
	if len(train) != 10 || len(test) != 90 {
		t.Errorf("比例 0 应钳制到 0.1: train=%d test=%d", len(train), len(test))
	}
	train, test = Split(mkRecs(100), 1.0)
	if len(train) != 90 || len(test) != 10 {
		t.Errorf("比例 1 应钳制到 0.9: train=%d test=%d", len(train), len(test))
	}
	// 数据不足 2 条 → 全部给训练集
	train, test = Split(mkRecs(1), 0.7)
	if len(train) != 1 || test != nil {
		t.Errorf("单条数据应全给训练集: train=%d test=%v", len(train), test)
	}
	train, test = Split(nil, 0.7)
	if train != nil || test != nil {
		t.Error("空输入应返回空")
	}
	// 极小样本也必须保证两段非空（否则样本外评估无意义）
	train, test = Split(mkRecs(3), 0.9)
	if len(train) == 0 || len(test) == 0 {
		t.Errorf("3 条数据也应两段非空: train=%d test=%d", len(train), len(test))
	}
}
