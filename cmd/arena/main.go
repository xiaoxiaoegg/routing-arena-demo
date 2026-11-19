// cmd/arena/main.go
// 影子竞技场入口（v6 方法论升级）：
//   读/生成日志 → 训练 70% 建知识库 → 回放 30% 样本外评估（真值判官）→ 策略对比报告
//
// 跑法：
//   go run ./cmd/arena                     # 模拟 2000 条
//   go run ./cmd/arena -records 10000      # 更大样本
//   go run ./cmd/arena -csv xxx.csv        # 用自己的 CSV（无故障窗口）
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"routing-arena-demo/internal/eval"
	"routing-arena-demo/internal/replay"
	"routing-arena-demo/internal/sim"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

func main() {
	csvPath := flag.String("csv", "", "消费日志 CSV 路径（空 = 用模拟数据）")
	records := flag.Int("records", 2000, "模拟数据条数")
	seed := flag.Int64("seed", 42, "随机种子（固定=可复现）")
	trainRatio := flag.Float64("train", 0.7, "训练集占比（建知识库用，其余样本外评估）")
	flag.Parse()

	cfg := sim.DefaultConfig()
	cfg.Records = *records
	cfg.Seed = *seed
	src, err := sim.LoadOrGenerate(*csvPath, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "取数失败: %v\n", err)
		os.Exit(1)
	}

	train, test := stats.Split(src.Records, *trainRatio)
	kb := stats.Aggregate(train)
	oracle := sim.NewOracle(cfg)
	check := sim.FaultChecker(src.Faults, *seed)

	fmt.Printf("数据: %s | 共 %d 条 | 训练 %d 条建知识库 → 回放 %d 条（样本外）\n\n",
		src.Describe(), len(src.Records), len(train), len(test))

	fmt.Println("渠道×模型知识库（训练集，策略「见过」的历史）:")
	for key, cs := range kb.ByChannelModel {
		fmt.Printf("  %-16s 请求=%-4d 千token成本=%.3f元 平均延迟=%dms\n",
			key, cs.Requests, cs.AvgCostPer1K, cs.AvgLatencyMs)
	}
	fmt.Println()

	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})
	for _, r := range test {
		eng.Breaker().Report("S1_MinCost", true, time.Duration(r.UseTimeMs)*time.Millisecond)
		eng.Breaker().Report("S2_MinLat", true, time.Duration(r.UseTimeMs)*time.Millisecond)
	}

	// S0 基线（现状：实际渠道）+ 候选策略（100% 切流），同一把尺子（真值判官）
	s0Recs, s0Res := replay.RunEngineWith(test, kb, eng, nil, check, oracle.Cost, oracle.Latency)
	s1Recs, s1Res := replay.RunEngineWith(test, kb, eng,
		strategy.NewBucket(1.0, "S1_MinCost").WithSeed(*seed), check, oracle.Cost, oracle.Latency)
	s2Recs, s2Res := replay.RunEngineWith(test, kb, eng,
		strategy.NewBucket(1.0, "S2_MinLat").WithSeed(*seed), check, oracle.Cost, oracle.Latency)

	// 评分 + 晋升建议（置信区间/显著性），取样本最多的模型
	all := append(append(append([]replay.Record{}, s0Recs...), s1Recs...), s2Recs...)
	agg := eval.NewAggregator()
	scoresAll := agg.Aggregate(all)
	domModel := dominantModel(scoresAll)

	// 对每个候选策略：与 S0 在 domModel 上逐条配对，算节省率均值 ± 95% CI
	savings := map[string]eval.CI{} // 策略 → 每请求节省（元）置信区间
	for _, id := range []string{"S1_MinCost", "S2_MinLat"} {
		savings[id] = pairedSavings(s0Recs, s1Recs, s2Recs, id, domModel)
	}

	rows := []struct {
		id  string
		res *replay.EngineResult
	}{
		{"S0_Champion", s0Res}, {"S1_MinCost", s1Res}, {"S2_MinLat", s2Res},
	}
	fmt.Println("==== 影子竞技场回放结果（样本外，真值成本）====")
	fmt.Printf("%-14s %12s %10s %8s %18s %8s\n", "策略", "总成本(元)", "平均延迟", "请求数", "节省(均值±95%CI)", "显著")
	fmt.Println("--------------------------------------------------------------------------------")
	s0Cost := s0Res.TotalCost
	for _, r := range rows {
		saved, sig := "-", "-"
		if r.id != "S0_Champion" {
			if ci, ok := savings[r.id]; ok && ci.N > 0 && s0Cost > 0 {
				s0Mean := s0Cost / float64(s0Res.Requests)
				pct := ci.Mean / s0Mean * 100
				half := ci.HalfWidth / s0Mean * 100
				saved = fmt.Sprintf("%.1f%% ± %.1f%%", pct, half)
				if ci.Low() > 0 {
					sig = "是"
				} else {
					sig = "否"
				}
			}
		}
		fmt.Printf("%-14s %12.2f %10dms %8d %18s %8s\n",
			r.id, r.res.TotalCost, r.res.AvgLatency, r.res.Requests, saved, sig)
	}
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("对照 S0 基线: S0 总成本 %.2f 元（%d 条样本外回放）\n", s0Cost, s0Res.Requests)
	fmt.Printf("口径: 成本/延迟为生成器真值（Oracle），非知识库自证；节省=(S0−候选)/S0，95%% 置信区间；%s 为样本最多的模型。\n", domModel)
}

// dominantModel 评分里样本量最大的模型
func dominantModel(scores []eval.StrategyScore) string {
	best, bestN := "gpt-4o", 0
	for _, s := range scores {
		if s.Requests > bestN {
			best, bestN = s.Model, s.Requests
		}
	}
	return best
}

// pairedSavings 在指定模型上，把候选记录与 S0 记录按下标配对（两遍回放
// 遍历的是同一批 test 记录、顺序一致），对每请求节省额算 95% 置信区间。
func pairedSavings(s0Recs, s1Recs, s2Recs []replay.Record, candID, model string) eval.CI {
	var cand []replay.Record
	switch candID {
	case "S1_MinCost":
		cand = s1Recs
	case "S2_MinLat":
		cand = s2Recs
	default:
		return eval.CI{}
	}
	var s0m, candm []replay.Record
	for _, r := range s0Recs {
		if r.Model == model {
			s0m = append(s0m, r)
		}
	}
	for _, r := range cand {
		if r.Model == model {
			candm = append(candm, r)
		}
	}
	n := len(s0m)
	if len(candm) < n {
		n = len(candm)
	}
	if n == 0 {
		return eval.CI{}
	}
	diffs := make([]float64, n)
	for i := 0; i < n; i++ {
		diffs[i] = s0m[i].Cost - candm[i].Cost
	}
	return eval.MeanCI(diffs)
}
