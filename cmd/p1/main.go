// cmd/p1/main.go
// P1-7 模拟真实小流量（v6 升级：样本外 + 真值判官）：
//   引擎默认 S0 + 5% 分桶给候选策略 + 熔断器 + 评估管道。
//   目标：验证"真实流量下候选策略数据与 P0 估算方向一致"（P1 验收标准）。
//
// 跑法：go run ./cmd/p1 [-ratio 0.05] [-strategy S1_MinCost] [-records 5000]
package main

import (
	"flag"
	"fmt"
	"os"

	"routing-arena-demo/internal/eval"
	"routing-arena-demo/internal/replay"
	"routing-arena-demo/internal/sim"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

func main() {
	csvPath := flag.String("csv", "", "消费日志 CSV 路径（空 = 用模拟数据）")
	records := flag.Int("records", 5000, "模拟数据条数")
	bucketRatio := flag.Float64("ratio", 0.05, "切给候选策略的流量比例（默认 5%）")
	candidate := flag.String("strategy", "S1_MinCost", "候选策略 ID")
	seed := flag.Int64("seed", 42, "随机种子（固定=结果可复现）")
	trainRatio := flag.Float64("train", 0.7, "训练集占比（建知识库）")
	flag.Parse()

	// 1. 数据：模拟生成或 CSV；训练集建库，回放集做样本外评估
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
	fmt.Printf("数据: %s | 共 %d 条 | 训练 %d 条建库 → 回放 %d 条（样本外），模拟真实小流量（%d%% 走候选策略 %s）\n\n",
		src.Describe(), len(src.Records), len(train), len(test), int(*bucketRatio*100), *candidate)

	// 2. 构建引擎：默认 S0 + 熔断器（v6：回放循环会把每次结果回告熔断器，无需预暖）
	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})

	// 3. 分桶：默认全 S0，5% 切给候选（固定 seed 保证可复现）
	bucket := strategy.NewBucket(*bucketRatio, *candidate).WithSeed(*seed)

	// 4. 评估管道：逐条请求走引擎选路，产出记录（真值成本）
	mixRecs, mixRes := replay.RunEngineWith(test, kb, eng, bucket, check, oracle.Cost, oracle.Latency)

	// 5. 按策略聚合评估记录
	byStrategy := map[string]*agg{}
	for _, rec := range mixRecs {
		a, ok := byStrategy[rec.StrategyID]
		if !ok {
			a = &agg{}
			byStrategy[rec.StrategyID] = a
		}
		a.Requests++
		a.TotalCost += rec.Cost
		a.TotalLatency += rec.LatencyMs
		if rec.Fallback {
			a.Fallbacks++
		}
	}

	fmt.Println("==== P1 模拟小流量结果（按实际生效策略聚合，真值成本）====")
	fmt.Printf("%-14s %8s %12s %12s %8s\n", "策略", "请求数", "总成本(元)", "平均延迟", "回退数")
	fmt.Println("----------------------------------------------------------")
	for id, a := range byStrategy {
		fmt.Printf("%-14s %8d %12.2f %10dms %8d\n",
			id, a.Requests, a.TotalCost, a.TotalLatency/a.Requests, a.Fallbacks)
	}
	fmt.Println("----------------------------------------------------------")
	fmt.Printf("合计: %d 请求, 总成本 %.2f 元, 平均延迟 %dms, 回退 %d 次\n\n",
		mixRes.Requests, mixRes.TotalCost, mixRes.AvgLatency, mixRes.Fallbacks)

	// 6. S0 基线（现状）同口径回放 → 真实对照
	s0Recs, s0Res := replay.RunEngineWith(test, kb, eng, nil, check, oracle.Cost, oracle.Latency)
	s0Cost := s0Res.TotalCost
	delta := (s0Cost - mixRes.TotalCost) / s0Cost * 100
	fmt.Printf("==== 对照 ====\n")
	fmt.Printf("全走 S0（现状，同口径真值）: %.2f 元\n", s0Cost)
	fmt.Printf("混流（%d%% 切候选）: %.2f 元 → %s%.2f%%\n",
		int(*bucketRatio*100), mixRes.TotalCost, signOf(delta), abs(delta))

	// 7. 方向一致性判断：理论预期 = 候选全量节省率 × 切流比例
	soloRecs, soloRes := replay.RunEngineWith(test, kb, eng,
		strategy.NewBucket(1.0, *candidate).WithSeed(*seed), check, oracle.Cost, oracle.Latency)
	_ = soloRecs
	soloDelta := (s0Cost - soloRes.TotalCost) / s0Cost * 100
	expectDelta := soloDelta * (*bucketRatio)
	fmt.Printf("\n对照基准: 候选策略 %s 全量节省 %.2f%% → 理论预期（×%d%% 流量）= %.2f%%\n",
		*candidate, soloDelta, int(*bucketRatio*100), expectDelta)

	// 8. 候选全量的节省率置信区间（配对差值）
	ci := pairedDiffCI(s0Recs, soloRecs, dominantModelOf(s0Recs, soloRecs))
	if ci.N > 0 && s0Cost > 0 {
		per := ci.Mean / (s0Cost / float64(s0Res.Requests)) * 100
		half := ci.HalfWidth / (s0Cost / float64(s0Res.Requests)) * 100
		fmt.Printf("候选全量节省（带误差）: %.1f%% ± %.1f%%（95%% 置信，配对 %d 条）\n", per, half, ci.N)
	}

	if delta > 0 {
		// 实际节省与理论预期同向且量级吻合（实际 ≥ 理论的 50% 即视为通过，容忍小样本波动）
		passed := expectDelta > 0 && delta >= expectDelta*0.5
		if passed {
			fmt.Printf("\n【结论】混流实际省 %.2f%%，与理论预期（%.2f%%）同向且量级吻合 → P1 验证通过。\n", delta, expectDelta)
		} else {
			fmt.Printf("\n【警告】混流实际省 %.2f%%，但明显低于理论预期（%.2f%%），需检查样本或策略。\n", delta, expectDelta)
		}
	} else {
		fmt.Printf("\n【警告】混流较 S0 %s%.2f%%，候选策略未带来节省，与 P0 估算方向不一致，需检查。\n", signOf(delta), abs(delta))
	}
	fmt.Printf("注：demo 用模拟数据（样本外 %d 条），真实收益需接入生产网关后跑真实流量验证。\n", len(test))
}

type agg struct {
	Requests     int
	TotalCost    float64
	TotalLatency int
	Fallbacks    int
}

// dominantModelOf 取回放记录里样本最多的模型
func dominantModelOf(sets ...[]replay.Record) string {
	count := map[string]int{}
	for _, set := range sets {
		for _, r := range set {
			count[r.Model]++
		}
	}
	best, bestN := "gpt-4o", 0
	for m, n := range count {
		if n > bestN {
			best, bestN = m, n
		}
	}
	return best
}

// pairedDiffCI 两遍回放同序配对（同为样本外 test 记录），算每请求节省额置信区间
func pairedDiffCI(s0Recs, candRecs []replay.Record, model string) eval.CI {
	var a, b []replay.Record
	for _, r := range s0Recs {
		if r.Model == model {
			a = append(a, r)
		}
	}
	for _, r := range candRecs {
		if r.Model == model {
			b = append(b, r)
		}
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return eval.CI{}
	}
	diffs := make([]float64, n)
	for i := 0; i < n; i++ {
		diffs[i] = a[i].Cost - b[i].Cost
	}
	return eval.MeanCI(diffs)
}

func signOf(v float64) string {
	if v >= 0 {
		return "+"
	}
	return "-"
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
