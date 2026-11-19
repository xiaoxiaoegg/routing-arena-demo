// cmd/p2/main.go
// P2 竞技场看板 + 自动选优（手动模式，v6 升级：样本外 + 真值判官 + 置信区间）：
//   读/生成日志 → 训练建库 → 样本外评估管道 → 聚合评分（带 95%CI）→ 晋升建议 → 模拟切换 → 回滚保障
//
// 跑法：
//   go run ./cmd/p2                 # 完整流程（默认 30% 切流）
//   go run ./cmd/p2 -ratio 0.05     # 5% 小流量（候选样本不足时看"无建议"保护）
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
	bucketRatio := flag.Float64("ratio", 0.3, "切给候选策略的流量比例（默认 30%，确保样本过门槛）")
	candidate := flag.String("strategy", "S1_MinCost", "候选策略 ID")
	seed := flag.Int64("seed", 42, "随机种子")
	trainRatio := flag.Float64("train", 0.7, "训练集占比（建知识库）")
	flag.Parse()

	// 1. 数据准备（样本外：训练建库，回放集评估）
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

	// 2. 评估管道：模拟真实小流量（分桶切流 + 熔断器，结果自动回告）
	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})
	bucket := strategy.NewBucket(*bucketRatio, *candidate).WithSeed(*seed)
	records_, res := replay.RunEngineWith(test, kb, eng, bucket, check, oracle.Cost, oracle.Latency)
	fmt.Printf("数据: %s | 共 %d 条 | 训练 %d 条建库 → 回放 %d 条 | 混流 %d%% 切候选 %s → %d 条评估记录（回退 %d 次）\n\n",
		src.Describe(), len(src.Records), len(train), len(test),
		int(*bucketRatio*100), *candidate, len(records_), res.Fallbacks)

	// 3. P2-1 聚合 + P2-3 评分（成本/延迟带 95% 置信区间）
	agg := eval.NewAggregator()
	scores := agg.Aggregate(records_)
	fmt.Println("==== 综合评分（成本40% / 延迟30% / 成功率30%，成本/延迟带 95%CI）====")
	fmt.Printf("%-10s %-14s %7s %10s %-16s %-14s %8s %8s\n",
		"模型", "策略", "请求数", "总成本", "均成本/请求", "均延迟", "成功率", "评分")
	fmt.Println("---------------------------------------------------------------------------")
	for _, s := range scores {
		fmt.Printf("%-10s %-14s %7d %10.2f %-16s %-14s %7.0f%% %8.1f\n",
			s.Model, s.StrategyID, s.Requests, s.TotalCost,
			ciStr(s.CostCI), ciStrMs(s.LatCI), s.SuccessRate*100, s.Score)
	}

	// 4. 晋升建议（统计严谨：显著性 + 综合分）
	fmt.Println("\n==== 晋升建议（95% 置信区间判定）====")
	recommendations := eval.Recommend(scores)
	if len(recommendations) == 0 {
		fmt.Println("  无候选策略数据（样本不足），无法建议")
	} else {
		for _, r := range recommendations {
			act := "维持现状"
			if r.ShouldPromote() {
				act = "建议晋升"
			}
			fmt.Printf("  【%s】模型 %s：%s 综合分差 %+.1f（冠军 %.1f）| 成本省 %s | 显著=%v | %s\n",
				act, r.Model, r.Candidate, r.CandidateWin, r.ChampionScore,
				r.SavedRange(), r.Significant, r.Reason)
		}
	}

	// 5. P2-4 模拟手动切换：把冠军切到建议策略
	fmt.Println("\n==== 手动模式：模拟切换 ====")
	switched := false
	for _, r := range recommendations {
		if r.ShouldPromote() {
			if err := eng.SetDefault(r.Candidate); err == nil {
				fmt.Printf("  [切换] 默认策略 S0_Champion → %s（模型 %s）\n", r.Candidate, r.Model)
				switched = true
			}
		}
	}
	if !switched {
		fmt.Println("  [保持] 无值得切换的候选，维持 S0_Champion")
	}

	// 6. P2-5 回滚保障：切换后模拟新冠军劣化 → 熔断 → 自动回退
	//    （用独立熔断器注入故障，避免回放积累的健康样本把失败率稀释掉）
	fmt.Println("\n==== 回滚保障（模拟新冠军劣化）====")
	if switched {
		fmt.Println("  [故障注入] 候选策略错误率飙升（模拟渠道故障）...")
		demoEng := strategy.NewEngine()
		demoEng.WithBreaker(strategy.BreakerConfig{})
		for i := 0; i < 10; i++ {
			demoEng.ReportOutcome(*candidate, false, 9000) // 全失败 + 高延迟
		}
		if !demoEng.Breaker().CanUse(*candidate) {
			fmt.Printf("  [熔断] %s 错误率超阈值 → 熔断触发\n", *candidate)
			if err := eng.SetDefault("S0_Champion"); err == nil {
				fmt.Println("  [回退] 已自动回退默认策略 → S0_Champion（冠军保护生效）")
			}
		} else {
			fmt.Println("  [未熔断] 候选策略仍健康，继续观察")
		}
	} else {
		fmt.Println("  [跳过] 未发生切换，无需回滚")
	}

	fmt.Println("\n==== 汇总 ====")
	fmt.Printf("混流总成本 %.2f 元 / 平均延迟 %dms / 回退 %d 次\n", res.TotalCost, res.AvgLatency, res.Fallbacks)
	fmt.Println("注：P2 为 demo 演示（模拟数据 + 模拟故障），接入生产网关后接入真实指标与真实切换。")
}

// ciStr 成本置信区间 "均值±半宽"
func ciStr(c eval.CI) string {
	if c.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f±%.3f", c.Mean, c.HalfWidth)
}

// ciStrMs 延迟置信区间（ms）
func ciStrMs(c eval.CI) string {
	if c.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f±%.0fms", c.Mean, c.HalfWidth)
}
