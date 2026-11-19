// internal/eval/eval_test.go
// P2 评估与选优单测：聚合、归一化评分、晋升建议，以及 v6 置信区间/显著性。

package eval

import (
	"math"
	"testing"

	"routing-arena-demo/internal/replay"
)

// mkRecords 构造测试记录：
//   - S0 走贵渠道（成本高、延迟高）40 条
//   - S1 走便宜渠道（成本低、延迟低）40 条
// 两策略成功率都 100%。样本量 ≥ MinSampleForCI，可参与晋升。
const (
	nS0 = 40
	nS1 = 40
)

func mkRecords() []replay.Record {
	var recs []replay.Record
	// S0: 40 条，贵渠道（每条 1.2 元，2000ms）
	for i := 0; i < nS0; i++ {
		recs = append(recs, replay.Record{
			StrategyID: "S0_Champion", Model: "gpt-4o", ChannelID: 2,
			PromptTokens: 1000, CompletionTokens: 1000,
			Cost: 1.2, LatencyMs: 2000, Success: true,
		})
	}
	// S1: 40 条，便宜渠道（每条 0.5 元，1000ms）
	for i := 0; i < nS1; i++ {
		recs = append(recs, replay.Record{
			StrategyID: "S1_MinCost", Model: "gpt-4o", ChannelID: 1,
			PromptTokens: 1000, CompletionTokens: 1000,
			Cost: 0.5, LatencyMs: 1000, Success: true,
		})
	}
	return recs
}

func TestAggregateGrouping(t *testing.T) {
	// 聚合应分成 (gpt-4o, S0) 和 (gpt-4o, S1) 两组
	a := NewAggregator()
	scores := a.Aggregate(mkRecords())
	if len(scores) != 2 {
		t.Fatalf("应聚合出 2 组，实际 %d", len(scores))
	}
	byID := map[string]StrategyScore{}
	for _, s := range scores {
		byID[s.StrategyID] = s
	}
	s0, ok := byID["S0_Champion"]
	if !ok {
		t.Fatal("缺 S0_Champion 组")
	}
	if s0.Requests != nS0 || math.Abs(s0.TotalCost-1.2*nS0) > 1e-9 || s0.AvgLatency != 2000 {
		t.Errorf("S0 聚合不对: requests=%d cost=%.1f lat=%d", s0.Requests, s0.TotalCost, s0.AvgLatency)
	}
	s1, ok := byID["S1_MinCost"]
	if !ok {
		t.Fatal("缺 S1_MinCost 组")
	}
	if s1.Requests != nS1 || math.Abs(s1.TotalCost-0.5*nS1) > 1e-9 || s1.AvgLatency != 1000 {
		t.Errorf("S1 聚合不对: requests=%d cost=%.1f lat=%d", s1.Requests, s1.TotalCost, s1.AvgLatency)
	}
	if s0.SuccessRate != 1.0 || s1.SuccessRate != 1.0 {
		t.Errorf("成功率应为 1.0")
	}
}

func TestAggregateSuccessRate(t *testing.T) {
	// 成功率统计：S1 里 2 条失败 → 38/40 = 0.95
	// mkRecords() 顺序：前 nS0 条 S0，之后 nS1 条 S1
	recs := mkRecords()
	recs[nS0].Success = false   // S1 第一条
	recs[nS0+1].Success = false // S1 第二条
	a := NewAggregator()
	scores := a.Aggregate(recs)
	for _, s := range scores {
		if s.StrategyID == "S1_MinCost" {
			want := float64(nS1-2) / float64(nS1)
			if math.Abs(s.SuccessRate-want) > 1e-9 {
				t.Errorf("S1 成功率应为 %.3f，实际 %.3f", want, s.SuccessRate)
			}
		}
	}
}

func TestScoreS1BeatsS0(t *testing.T) {
	// 归一化评分：S1 成本低 + 延迟低 → 综合分应高于 S0
	a := NewAggregator()
	scores := a.Aggregate(mkRecords())
	byID := map[string]StrategyScore{}
	for _, s := range scores {
		byID[s.StrategyID] = s
	}
	if byID["S1_MinCost"].Score <= byID["S0_Champion"].Score {
		t.Errorf("S1 应高于 S0: S1=%.1f S0=%.1f", byID["S1_MinCost"].Score, byID["S0_Champion"].Score)
	}
	if byID["S1_MinCost"].CostScore <= byID["S0_Champion"].CostScore {
		t.Errorf("S1 成本分应更高: S1=%.1f S0=%.1f", byID["S1_MinCost"].CostScore, byID["S0_Champion"].CostScore)
	}
}

func TestRecommendS1(t *testing.T) {
	// S1 综合分更高 + 成本显著更低 → 建议晋升 S1
	a := NewAggregator()
	scores := a.Aggregate(mkRecords())
	recs := Recommend(scores)
	if len(recs) != 1 {
		t.Fatalf("应有 1 条建议，实际 %d", len(recs))
	}
	r := recs[0]
	if r.Model != "gpt-4o" || r.Candidate != "S1_MinCost" {
		t.Errorf("应建议晋升 S1_MinCost (gpt-4o)，实际 %s (%s)", r.Candidate, r.Model)
	}
	if r.CandidateWin <= 0 {
		t.Errorf("分差应为正（S1 更优），实际 %.1f", r.CandidateWin)
	}
	if !r.Significant {
		t.Error("成本 1.2 → 0.5 且方差为 0，应判定为显著")
	}
	if !r.ShouldPromote() {
		t.Errorf("应建议晋升，理由: %s", r.Reason)
	}
	// 节省率 = (1.2-0.5)/1.2 = 58.3%
	if math.Abs(r.SavedPct-58.3) > 0.15 {
		t.Errorf("节省率应约 58.3%%，实际 %.1f%%", r.SavedPct)
	}
	if r.SavedRange() == "" {
		t.Error("节省率区间文字不应为空")
	}
	t.Logf("建议晋升 %s：省 %s（%s）", r.Candidate, r.SavedRange(), r.Reason)
}

func TestRecommendNoUpgradeWhenS0Wins(t *testing.T) {
	// S0 便宜且快，S1 贵且慢 → 不推荐晋升
	recs := []replay.Record{
		{StrategyID: "S0_Champion", Model: "gpt-4o", ChannelID: 1, Cost: 0.5, LatencyMs: 500, Success: true},
		{StrategyID: "S0_Champion", Model: "gpt-4o", ChannelID: 1, Cost: 0.5, LatencyMs: 500, Success: true},
		{StrategyID: "S1_MinCost", Model: "gpt-4o", ChannelID: 2, Cost: 2.0, LatencyMs: 3000, Success: true},
		{StrategyID: "S1_MinCost", Model: "gpt-4o", ChannelID: 2, Cost: 2.0, LatencyMs: 3000, Success: true},
	}
	a := NewAggregator()
	recsOut := Recommend(a.Aggregate(recs))
	for _, r := range recsOut {
		if r.ShouldPromote() {
			t.Errorf("S0 更优时不应建议晋升，实际 %+v", r)
		}
	}
}

func TestRecommendEmpty(t *testing.T) {
	a := NewAggregator()
	if recs := Recommend(a.Aggregate(nil)); len(recs) != 0 {
		t.Fatalf("空记录应无建议，实际 %d", len(recs))
	}
}

func TestRecommendBelowCISampleExcluded(t *testing.T) {
	// 样本门槛（v6 改为置信区间可用下限）：候选仅 10 条 → 不参与晋升
	var recs []replay.Record
	for i := 0; i < 100; i++ {
		recs = append(recs, replay.Record{StrategyID: "S0_Champion", Model: "gpt-4o", ChannelID: 1, Cost: 1.0, LatencyMs: 1000, Success: true})
	}
	for i := 0; i < 10; i++ {
		recs = append(recs, replay.Record{StrategyID: "S1_MinCost", Model: "gpt-4o", ChannelID: 2, Cost: 0.1, LatencyMs: 100, Success: true})
	}
	a := NewAggregator()
	recsOut := Recommend(a.Aggregate(recs))
	if len(recsOut) != 0 {
		t.Fatalf("候选 %d 条 < MinSampleForCI(%d) 不应参与晋升，实际建议: %+v",
			10, MinSampleForCI, recsOut)
	}
}

func TestRecommendSufficientSample(t *testing.T) {
	// 样本充足且成本显著更低 → 参与晋升
	var recs []replay.Record
	for i := 0; i < 50; i++ {
		recs = append(recs, replay.Record{StrategyID: "S0_Champion", Model: "gpt-4o", ChannelID: 1, Cost: 1.0, LatencyMs: 1000, Success: true})
	}
	for i := 0; i < 50; i++ {
		recs = append(recs, replay.Record{StrategyID: "S1_MinCost", Model: "gpt-4o", ChannelID: 2, Cost: 0.1, LatencyMs: 100, Success: true})
	}
	a := NewAggregator()
	recsOut := Recommend(a.Aggregate(recs))
	found := false
	for _, r := range recsOut {
		if r.Candidate == "S1_MinCost" && r.ShouldPromote() {
			found = true
		}
	}
	if !found {
		t.Fatalf("样本充足且更优，应建议晋升 S1，实际: %+v", recsOut)
	}
}

// ---------------------------------------------------------------------------
// v6 新增：置信区间与显著性
// ---------------------------------------------------------------------------

func TestMeanCIKnownVariance(t *testing.T) {
	// 样本 [1,2,3,4,5]：均值 3，样本方差 2.5，SE=sqrt(0.5)≈0.7071，
	// n=5 → t(4)=2.776 → 半宽 ≈ 1.963
	ci := MeanCI([]float64{1, 2, 3, 4, 5})
	if math.Abs(ci.Mean-3) > 1e-9 {
		t.Errorf("均值应为 3，实际 %.4f", ci.Mean)
	}
	if math.Abs(ci.HalfWidth-1.963) > 0.01 {
		t.Errorf("半宽应约 1.963，实际 %.4f", ci.HalfWidth)
	}
	if ci.N != 5 {
		t.Errorf("样本量应为 5，实际 %d", ci.N)
	}
	if math.Abs(ci.Low()-1.037) > 0.02 || math.Abs(ci.High()-4.963) > 0.02 {
		t.Errorf("区间应为约 [1.04, 4.96]，实际 [%.3f, %.3f]", ci.Low(), ci.High())
	}
}

func TestMeanCIEdgeCases(t *testing.T) {
	if ci := MeanCI(nil); ci.Mean != 0 || ci.HalfWidth != 0 {
		t.Errorf("空样本应返回零值，实际 %+v", ci)
	}
	if ci := MeanCI([]float64{7}); ci.Mean != 7 || ci.HalfWidth != 0 {
		t.Errorf("单样本半宽应为 0，实际 %+v", ci)
	}
	// 恒定值：方差 0 → 半宽 0（无限确定）
	ci := MeanCI([]float64{2, 2, 2, 2})
	if ci.HalfWidth != 0 {
		t.Errorf("恒定样本半宽应为 0，实际 %.4f", ci.HalfWidth)
	}
	if ci.RelHalf() != 0 {
		t.Errorf("恒定样本相对半宽应为 0，实际 %.4f", ci.RelHalf())
	}
}

func TestRecommendNotSignificantWhenOverlapping(t *testing.T) {
	// 候选均值更低但波动大 → 区间重叠 → 不显著，不应建议晋升
	var recs []replay.Record
	for i := 0; i < 40; i++ {
		c := 0.8
		if i%2 == 0 {
			c = 1.2 // 均值 1.0，标准差 0.2
		}
		recs = append(recs, replay.Record{StrategyID: "S0_Champion", Model: "gpt-4o", ChannelID: 1, Cost: c, LatencyMs: 1000, Success: true})
	}
	for i := 0; i < 40; i++ {
		c := 0.7
		if i%2 == 0 {
			c = 1.1 // 均值 0.9，标准差 0.2 → 区间与冠军重叠
		}
		recs = append(recs, replay.Record{StrategyID: "S1_MinCost", Model: "gpt-4o", ChannelID: 2, Cost: c, LatencyMs: 900, Success: true})
	}
	a := NewAggregator()
	recsOut := Recommend(a.Aggregate(recs))
	if len(recsOut) != 1 {
		t.Fatalf("应有 1 条评估，实际 %d", len(recsOut))
	}
	r := recsOut[0]
	if r.Significant {
		t.Errorf("区间重叠时不应判定显著: 候选[%.3f,%.3f] 冠军[%.3f,%.3f]",
			r.CandidateCI.Low(), r.CandidateCI.High(), r.ChampionCI.Low(), r.ChampionCI.High())
	}
	if r.ShouldPromote() {
		t.Error("不显著时不应建议晋升")
	}
	if r.Reason == "" {
		t.Error("应给出「为什么不建议」的理由")
	}
	t.Logf("未晋升理由: %s（候选省 %.1f%%）", r.Reason, r.SavedPct)
}

func TestTCritMonotonic(t *testing.T) {
	// t 临界值应随样本量增加而下降，且不小于 1.96
	prev := math.Inf(1)
	for n := 2; n <= 200; n++ {
		c := TCrit(n)
		if c < Z95-1e-9 {
			t.Fatalf("n=%d 临界值 %.3f 小于正态近似 %.2f", n, c, Z95)
		}
		if c > prev+1e-9 {
			t.Fatalf("n=%d 临界值 %.3f 未随样本量下降（上一个 %.3f）", n, c, prev)
		}
		prev = c
	}
	if TCrit(1) != Z95 {
		t.Errorf("n=1（df=0）应回退正态近似")
	}
}

func TestSavedRangeFormat(t *testing.T) {
	r := Recommendation{SavedPct: 16.04, SavedPctHalf: 2.35}
	if got := r.SavedRange(); got != "16.0% ± 2.4%" {
		t.Errorf("节省率区间格式不对: %s", got)
	}
	r2 := Recommendation{SavedPct: -3.2, SavedPctHalf: 1.05}
	if got := r2.SavedRange(); got != "-3.2% ± 1.1%" {
		t.Errorf("负节省率格式不对: %s", got)
	}
}
