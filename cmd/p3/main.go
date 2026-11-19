// cmd/p3/main.go
// P3 健康探测 + 自动模式（v6 升级：模拟数据大样本，保证晋升演示可触发）：
//   健康探测（P3-1）→ 自动切换（P3-2，需二次确认 P3-3）→ 全程留痕
//
// 跑法：go run ./cmd/p3 [-records 3000] [-seed 42]
package main

import (
	"flag"
	"fmt"
	"time"

	"routing-arena-demo/internal/arena"
	"routing-arena-demo/internal/eval"
	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/replay"
	"routing-arena-demo/internal/sim"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

func main() {
	records := flag.Int("records", 3000, "模拟数据条数")
	seed := flag.Int64("seed", 42, "随机种子")
	flag.Parse()

	cfg := sim.DefaultConfig()
	cfg.Records = *records
	cfg.Seed = *seed
	recs, faults := sim.Generate(cfg)
	train, test := stats.Split(recs, 0.7)
	kb := stats.Aggregate(train)
	oracle := sim.NewOracle(cfg)
	check := sim.FaultChecker(faults, *seed)
	_ = oracle

	// 2. 健康探测（P3-1）：渠道1 故障，渠道2 变慢，其余健康
	//    真实场景用 /models 免费接口；这里模拟
	probeCall := 0
	prober := health.NewProber(allChannelIDs(recs), time.Minute, func(id int) health.State {
		probeCall++
		switch id {
		case 1:
			return health.State{ChannelID: 1, Up: false, LatencyMs: 0, ErrorRate: 1.0} // 故障
		case 2:
			return health.State{ChannelID: 2, Up: true, LatencyMs: 3000, ErrorRate: 0.02} // 变慢
		default:
			return health.State{ChannelID: id, Up: true, LatencyMs: 600, ErrorRate: 0.005} // 健康
		}
	})
	prober.ProbeOnce()
	fmt.Println("==== P3-1 健康探测 ====")
	for _, st := range prober.All() {
		fmt.Printf("  渠道%d: up=%v 延迟=%dms 错误率=%.1f%%\n", st.ChannelID, st.Up, st.LatencyMs, st.ErrorRate*100)
	}
	fmt.Printf("  可用渠道: %v（零成本探测 %d 次，未产生 token 计费）\n\n", prober.HealthyChannels(), probeCall)

	// 3. 引擎接入健康探测（S2 选路排除故障渠道）；熔断器由回放结果自动维护
	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})
	eng.WithHealthProber(prober)

	// 4. 模拟自动模式前的一轮评估（70% 切给 S1，样本过置信区间门槛）
	bucket := strategy.NewBucket(0.7, "S1_MinCost").WithSeed(*seed)
	records_, _ := replay.RunEngineWith(test, kb, eng, bucket, check, oracle.Cost, oracle.Latency)
	agg := eval.NewAggregator()
	scores := agg.Aggregate(records_)

	// 5. 竞技场管理（P3-2/P3-3）
	fmt.Println("==== P3-2 自动模式 ====")
	mgr := arena.NewManager(eng.DefaultID())
	fmt.Printf("  初始模式: %s，冠军: %s\n", mgr.Mode(), mgr.Champion())

	// P3-3 二次确认防护：未确认时切自动被拒
	fmt.Println("  [P3-3 二次确认] 尝试直接切自动模式...")
	if err := mgr.SetMode(arena.ModeAuto); err != nil {
		fmt.Printf("  → 被拒绝: %v\n", err)
		fmt.Println("  → 管理员二次确认...")
		mgr.ConfirmAuto()
		if err := mgr.SetMode(arena.ModeAuto); err != nil {
			fmt.Printf("  → 确认后仍失败: %v\n", err)
		} else {
			fmt.Println("  → 确认成功，已切换自动模式（误开防护生效）")
		}
	}

	// 自动模式：评估建议 → 系统自动切换
	fmt.Println("\n  [自动评估] 基于评估记录计算晋升建议...")
	recommendations := eval.Recommend(scores)
	autoSwitched := false
	for _, r := range recommendations {
		if r.ShouldPromote() {
			switched := mgr.AutoSwitch(r.Candidate, r.CandidateWin, time.Now())
			fmt.Printf("  → 候选 %s 综合分差 %+.1f 且成本显著更省: %v\n", r.Candidate, r.CandidateWin, switched)
			if switched {
				autoSwitched = true
			}
		}
	}
	if !autoSwitched {
		fmt.Println("  → 无稳定优势候选，维持现状（自动模式也避免无谓切换）")
	}
	fmt.Printf("  当前冠军: %s\n", mgr.Champion())

	// 6. 切换历史留痕
	fmt.Println("\n==== 切换历史（全程留痕）====")
	fmt.Print(mgr.HistoryString())

	// 7. 故障回退演示：新冠军劣化 → 熔断 → 自动回退
	//    （独立熔断器注入故障，避免回放积累的健康样本稀释失败率）
	fmt.Println("\n==== 故障回退（新冠军劣化）====")
	fmt.Println("  [故障注入] 当前冠军策略错误率飙升...")
	demoEng := strategy.NewEngine()
	demoEng.WithBreaker(strategy.BreakerConfig{})
	for i := 0; i < 10; i++ {
		demoEng.ReportOutcome("S1_MinCost", false, 9000)
	}
	if !demoEng.Breaker().CanUse("S1_MinCost") {
		fmt.Println("  → 熔断触发")
		mgr.Fallback("S0_Champion")
		fmt.Printf("  → 自动回退冠军到: %s\n", mgr.Champion())
		fmt.Print("\n  更新后的切换历史:\n", mgr.HistoryString())
	} else {
		fmt.Println("  → 未熔断，继续观察")
	}

	fmt.Println("\n==== 汇总 ====")
	fmt.Printf("数据 %d 条（训练 %d → 回放 %d 样本外）| 健康探测 %d 次（零 token 计费）| 切换历史 %d 条可审计\n",
		len(recs), len(train), len(test), probeCall, len(mgr.History()))
}

// allChannelIDs 数据里出现的渠道 ID
func allChannelIDs(recs []logreader.LogRecord) []int {
	seen := map[int]bool{}
	var out []int
	for _, r := range recs {
		if !seen[r.ChannelID] {
			seen[r.ChannelID] = true
			out = append(out, r.ChannelID)
		}
	}
	return out
}
