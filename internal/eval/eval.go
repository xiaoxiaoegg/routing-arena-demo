// internal/eval/eval.go
// P2 评估与选优：把 P1 评估数据管道的原始记录，聚合 → 评分 → 建议晋升。
//
// 聚合维度（P2-1）：模型 × 策略
// 评分维度（P2-3）：成本 40% / 延迟 30% / 成功率 30%（归一化后加权）
// 晋升判定：综合分最高 + 且成本优势「统计显著」→ 输出"建议晋升"
//
// 统计严谨性（v6）：
//   - 每个策略的成本/延迟都带 95% 置信区间（小样本用 t 分布，大样本用正态近似）
//   - 晋升不再只看"样本条数 ≥ 10"这种硬门槛，而是看候选的置信区间是否
//     整体低于冠军（不重叠才算"省得显著"），避免被小样本的漂亮数字骗
//   - 输出带误差的结论：省 16.0% ± 2.3%
package eval

import (
	"math"
	"sort"

	"routing-arena-demo/internal/replay"
)

// CI 均值置信区间（默认 95%）
type CI struct {
	Mean      float64 // 均值
	HalfWidth float64 // 半宽（Mean ± HalfWidth）
	N         int     // 样本量
}

// Low 下界
func (c CI) Low() float64 { return c.Mean - c.HalfWidth }

// High 上界
func (c CI) High() float64 { return c.Mean + c.HalfWidth }

// RelHalf 相对半宽（半宽 / 均值），均值 <= 0 时返回 0
func (c CI) RelHalf() float64 {
	if c.Mean <= 0 {
		return 0
	}
	return c.HalfWidth / c.Mean
}

// StrategyScore 单个策略在某个模型上的聚合评分
type StrategyScore struct {
	StrategyID    string  // 策略 ID
	Model         string  // 模型名
	Requests      int     // 请求数（样本量）
	TotalCost     float64 // 总成本（元）
	AvgCostPerReq float64 // 平均每请求成本（元）——归一化用这个，跨请求数可比
	AvgLatency    int     // 平均延迟（ms）
	SuccessRate   float64 // 成功率 [0,1]
	Score         float64 // 综合评分 [0,100]
	CostScore     float64 // 成本维度分（越低越好，归一化到 [0,100]）
	LatScore      float64 // 延迟维度分（越低越好）
	SuccScore     float64 // 成功率维度分（越高越好）

	// v6 统计显著性：成本 / 延迟的 95% 置信区间
	CostCI CI // 平均每请求成本（元）
	LatCI  CI // 平均延迟（ms）
}

// Aggregator 聚合器：把评估记录按 (模型, 策略) 分组统计
type Aggregator struct {
	// 评分权重（P2-3 默认：成本 40% / 延迟 30% / 成功率 30%）
	WeightCost float64
	WeightLat  float64
	WeightSucc float64
}

// NewAggregator 默认权重聚合器
func NewAggregator() *Aggregator {
	return &Aggregator{WeightCost: 0.4, WeightLat: 0.3, WeightSucc: 0.3}
}

// Aggregate 聚合记录：返回按 (模型, 策略) 的评分列表
func (a *Aggregator) Aggregate(records []replay.Record) []StrategyScore {
	// 1. 分组统计（同时保留每条的样本，用于算置信区间）
	type acc struct {
		costs    []float64
		lats     []float64
		success  int
	}
	groups := map[string]*acc{} // key: model|strategy
	keyOf := func(model, sid string) string { return model + "|" + sid }

	for _, r := range records {
		k := keyOf(r.Model, r.StrategyID)
		g, ok := groups[k]
		if !ok {
			g = &acc{}
			groups[k] = g
		}
		g.costs = append(g.costs, r.Cost)
		g.lats = append(g.lats, float64(r.LatencyMs))
		if r.Success {
			g.success++
		}
	}

	// 2. 算出每个分组的成本/延迟/成功率 + 置信区间
	raw := map[string]StrategyScore{}
	for k, g := range groups {
		model, sid := splitKey(k)
		n := len(g.costs)
		if n == 0 {
			continue
		}
		costCI := MeanCI(g.costs)
		latCI := MeanCI(g.lats)
		raw[k] = StrategyScore{
			StrategyID:    sid,
			Model:         model,
			Requests:      n,
			TotalCost:     costCI.Mean * float64(n),
			AvgCostPerReq: costCI.Mean,
			AvgLatency:    int(math.Round(latCI.Mean)),
			SuccessRate:   float64(g.success) / float64(n),
			CostCI:        costCI,
			LatCI:         latCI,
		}
	}

	// 3. 按模型分组，组内归一化评分
	byModel := map[string][]string{}
	for k := range raw {
		model, _ := splitKey(k)
		byModel[model] = append(byModel[model], k)
	}

	var out []StrategyScore
	for _, keys := range byModel {
		// 组内极值（归一化基准，用平均每请求成本，跨请求数可比）
		minCost, maxCost := 1e18, 0.0
		minLat := int(^uint(0) >> 1)
		for _, k := range keys {
			s := raw[k]
			if s.AvgCostPerReq < minCost {
				minCost = s.AvgCostPerReq
			}
			if s.AvgCostPerReq > maxCost {
				maxCost = s.AvgCostPerReq
			}
			if s.AvgLatency < minLat {
				minLat = s.AvgLatency
			}
		}
		// 归一化：成本/延迟越低越好 → 分越高；成功率越高越好
		for _, k := range keys {
			s := raw[k]
			rangeCost := maxCost - minCost
			if rangeCost <= 0 {
				s.CostScore = 100
			} else {
				s.CostScore = (1 - (s.AvgCostPerReq-minCost)/rangeCost) * 100
			}
			// 延迟分：相对最慢的差距
			if s.AvgLatency > 0 {
				s.LatScore = float64(minLat) / float64(s.AvgLatency) * 100
			}
			s.SuccScore = s.SuccessRate * 100
			s.Score = a.WeightCost*s.CostScore + a.WeightLat*s.LatScore + a.WeightSucc*s.SuccScore
			out = append(out, s)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Score > out[j].Score
	})
	return out
}

// MinSampleForCI 置信区间的可用样本下限。
// 低于这个条数时正态/t 近似都不可靠，区间会宽得没有意义——
// 但它不再直接决定"能不能晋升"，晋升由区间是否重叠决定（见 Recommend）。
const MinSampleForCI = 30

// Recommendation 晋升建议
type Recommendation struct {
	Model         string  // 模型名
	Candidate     string  // 建议晋升的策略
	CandidateWin  float64 // 相对冠军的综合分差（>0 = 分更高）
	ChampionScore float64 // 当前冠军分

	// v6 统计严谨性
	Significant  bool          // 成本优势是否统计显著（候选成本区间整体低于冠军）
	CandidateCI  CI            // 候选平均每请求成本区间
	ChampionCI   CI            // 冠军平均每请求成本区间
	SavedPct     float64       // 成本节省率（%，相对冠军）
	SavedPctHalf float64       // 节省率的 95% 半宽（%）
	Reason       string        // 结论理由（建议晋升 / 为什么不建议）
	CandidateScr StrategyScore // 候选完整评分（便于调用方展示）
	ChampionScr  StrategyScore // 冠军完整评分
}

// SavedRange 节省率区间文字，如 "16.0% ± 2.3%"
func (r Recommendation) SavedRange() string {
	return sprintfPct(r.SavedPct) + " ± " + sprintfPct(r.SavedPctHalf)
}

// Recommend 晋升建议：对每个模型，找综合分最高且非 S0 的候选策略，
// 并用置信区间判断"省得是否显著"。
//
// 判定规则（v6）：
//  1. 候选样本 < MinSampleForCI → 区间不可靠，不参与晋升
//  2. 候选成本区间上界 < 冠军成本区间下界 → 成本优势显著
//  3. 综合分更高 且 成本优势显著 → 建议晋升；否则维持现状并说明原因
func Recommend(scores []StrategyScore) []Recommendation {
	best := map[string]*StrategyScore{} // model → 最高分候选
	s0 := map[string]*StrategyScore{}   // model → S0 冠军
	for i := range scores {
		s := &scores[i]
		if s.StrategyID == "S0_Champion" {
			s0[s.Model] = s
			continue
		}
		// 样本门槛：低于置信区间可用下限 → 不参与晋升
		if s.Requests < MinSampleForCI {
			continue
		}
		if cur, ok := best[s.Model]; !ok || s.Score > cur.Score {
			best[s.Model] = s
		}
	}

	var recs []Recommendation
	for model, cand := range best {
		cur, ok := s0[model]
		if !ok {
			continue // 该模型没有 S0 基线，跳过
		}

		// 成本节省率：以冠军均值为基准
		saved := 0.0
		if cur.CostCI.Mean > 0 {
			saved = (cur.CostCI.Mean - cand.CostCI.Mean) / cur.CostCI.Mean * 100
		}
		// 节省率半宽（近似：合并两边的标准误；分母用冠军均值，不做二次放大 → 保守）
		seChamp := cur.CostCI.HalfWidth / Z95
		seCand := cand.CostCI.HalfWidth / Z95
		seDiff := math.Sqrt(seChamp*seChamp + seCand*seCand)
		halfPct := 0.0
		if cur.CostCI.Mean > 0 {
			halfPct = seDiff * Z95 / cur.CostCI.Mean * 100
		}

		// 显著性：候选成本区间整体低于冠军（不重叠）
		sig := cand.CostCI.High() < cur.CostCI.Low()

		win := cand.Score - cur.Score
		reason := ""
		switch {
		case !sig:
			reason = "成本优势不显著（区间重叠），继续观察"
		case win <= 0:
			reason = "虽然更省，但综合分（含延迟/成功率）未超过冠军，继续观察"
		default:
			reason = "成本显著更低且综合分更高"
		}

		recs = append(recs, Recommendation{
			Model:         model,
			Candidate:     cand.StrategyID,
			CandidateWin:  win,
			ChampionScore: cur.Score,
			Significant:   sig,
			CandidateCI:   cand.CostCI,
			ChampionCI:    cur.CostCI,
			SavedPct:      saved,
			SavedPctHalf:  halfPct,
			Reason:        reason,
			CandidateScr:  *cand,
			ChampionScr:   *cur,
		})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Model < recs[j].Model })
	return recs
}

// ShouldPromote 是否建议晋升（成本显著更低 + 综合分更高）
func (r Recommendation) ShouldPromote() bool { return r.Significant && r.CandidateWin > 0 }

// Z95 95% 置信水平对应的正态分位数
const Z95 = 1.96

// MeanCI 计算均值与 95% 置信区间半宽。
// n < 2 → 半宽 0（无信息）；n ≤ 30 用 t 分布临界值（小样本更保守），否则正态近似。
// MeanCI 计算均值与 95% 置信区间（对外暴露，供其它包做同样的统计判断）
func MeanCI(xs []float64) CI {
	n := len(xs)
	if n == 0 {
		return CI{}
	}
	mean := 0.0
	for _, x := range xs {
		mean += x
	}
	mean /= float64(n)
	if n < 2 {
		return CI{Mean: mean, HalfWidth: 0, N: n}
	}

	varSum := 0.0
	for _, x := range xs {
		d := x - mean
		varSum += d * d
	}
	variance := varSum / float64(n-1) // 样本方差（无偏）
	se := math.Sqrt(variance / float64(n))

	return CI{Mean: mean, HalfWidth: TCrit(n) * se, N: n}
}

// TCrit 95% 双尾 t 分布临界值（df = n-1）；df > 30 用正态近似 1.96。
// 只取一张小表，够 demo 用（小样本置信区间保守一些没坏处）。
func TCrit(n int) float64 {
	df := n - 1
	table := [...]float64{
		12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
		2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
		2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
	}
	if df < 1 {
		return Z95
	}
	if df <= len(table) {
		return table[df-1]
	}
	return Z95
}

// splitKey 拆分 "model|strategy"
func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// sprintfPct 百分比格式化（保留 1 位小数 + %）
func sprintfPct(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	intPart := int(v)
	frac := int(math.Round((v-float64(intPart))*10))
	s := itoa(intPart) + "." + itoa(frac) + "%"
	if neg {
		return "-" + s
	}
	return s
}

// itoa 小整数转字符串（避免为了格式化再引入依赖）
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
