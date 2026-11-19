// cmd/p4/main.go
// 第三波能力演示（全部基于模拟数据——"伪造的真实数据"）：
//
//	P4-1 时段自适应权重：同一策略白天重延迟、夜间重成本
//	P4-2 会话粘性 sticky：同用户固定渠道，渠道切换次数大减
//	P4-3 约束式选路 S5：延迟 ≤1.5s 里挑最便宜（业务口径）
//	P4-4 Prometheus 指标 + 毛刺下钻（Exemplar：指标上挂 trace_id，点毛刺找请求）
//
// 跑法：go run ./cmd/p4 [-records 6000]
package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/metrics"
	"routing-arena-demo/internal/replay"
	"routing-arena-demo/internal/sim"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

// successFn 与 replay.RunEngineWith 的成功判定函数签名一致
type successFn = func(logreader.LogRecord, int) bool

func main() {
	records := flag.Int("records", 6000, "模拟数据条数")
	seed := flag.Int64("seed", 42, "随机种子")
	flag.Parse()

	cfg := sim.DefaultConfig()
	cfg.Records = *records
	cfg.Seed = *seed
	recs, _ := sim.Generate(cfg)
	train, test := stats.Split(recs, 0.7)
	kb := stats.Aggregate(train)
	oracle := sim.NewOracle(cfg)
	check := sim.FaultChecker(sim.FaultWindows(cfg), *seed)

	fmt.Printf("数据: 模拟 %d 条 | 训练 %d → 回放 %d（样本外，真值评估）\n\n", len(recs), len(train), len(test))

	part1(test, kb, check, oracle, *seed)
	part2(test, kb, check, oracle, *seed)
	part3(test, kb, check, oracle, *seed)
	part4(test, kb, check, oracle, *seed)
}

// ---------------------------------------------------------------------------
// P4-1 时段自适应权重（机制微演示：快而贵 vs 慢而便宜，两选一）
// ---------------------------------------------------------------------------
func part1(test []logreader.LogRecord, kb *stats.Knowledge, check successFn, oracle *sim.Oracle, seed int64) {
	fmt.Println("==== P4-1 时段自适应权重（同一策略，白天重延迟 / 夜间重成本）====")
	fmt.Printf("  白天 %s\n", strategy.AdaptiveProfile(10))
	fmt.Printf("  夜间 %s\n", strategy.AdaptiveProfile(22))
	fmt.Printf("  凌晨 %s\n", strategy.AdaptiveProfile(3))

	// 构造两渠道知识库：渠道4 快而贵(520ms/0.315)、渠道2 慢而便宜(3200ms/0.245)
	kbm := &stats.Knowledge{ByChannelModel: map[string]*stats.ChannelStats{
		"4:gpt-4o": {ChannelID: 4, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.315, AvgLatencyMs: 520},
		"2:gpt-4o": {ChannelID: 2, Model: "gpt-4o", Requests: 100, AvgCostPer1K: 0.245, AvgLatencyMs: 3200},
	}}
	s := &strategy.S3Hybrid{Scorer: strategy.Scorer{Weights: strategy.DefaultHybridWeights()}}
	s.SetAdaptive(strategy.AdaptiveWeights)
	s.WithSeed(7)

	rc := &strategy.RouteContext{Model: "gpt-4o"}
	rc.Hour = 10
	day := s.Pick(rc, kbm, []int{4, 2})
	rc.Hour = 22
	night := s.Pick(rc, kbm, []int{4, 2})
	rc.Hour = 3
	midnight := s.Pick(rc, kbm, []int{4, 2})

	names := map[int]string{4: "渠道4(快而贵 520ms)", 2: "渠道2(慢而便宜 3200ms)"}
	fmt.Printf("\n  在「快而贵 vs 慢而便宜」里选:\n")
	fmt.Printf("    白天 10 点 → %s（重延迟，要快）\n", names[day])
	fmt.Printf("    夜间 22 点 → %s（重成本，要省）\n", names[night])
	fmt.Printf("    凌晨 03 点 → %s（极致成本）\n", names[midnight])

	// 诚实备注：真实 7 天回放里，"又快又便宜"的渠道5 常年占优 → 所有权重都选它，
	// 总体指标几乎不变。权重只在「快/便宜二选一」时才有意义。
	fmt.Printf("\n  备注: 真实回放中总体指标变化极小——因为存在「又快又便宜」的渠道5，\n  所有时段都会选它。自适应权重只在「快 or 便宜」冲突时才起作用（如上）。\n\n")
}

// ---------------------------------------------------------------------------
// P4-2 会话粘性
// ---------------------------------------------------------------------------
func part2(test []logreader.LogRecord, kb *stats.Knowledge, check successFn, oracle *sim.Oracle, seed int64) {
	// 非粘性：标准 S3
	engA := strategy.NewEngine()
	engA.WithBreaker(strategy.BreakerConfig{})
	recsA, _ := replay.RunEngineWith(test, kb, engA,
		strategy.NewBucket(1.0, "S3_Hybrid").WithSeed(seed), check, oracle.Cost, oracle.Latency)
	swA := countSwitches(recsA)

	// 粘性：StickyRouter 包 S3 并顶替注册（ID 仍是 S3_Hybrid）
	stickyS3 := strategy.NewSticky(&strategy.S3Hybrid{Scorer: strategy.Scorer{Weights: strategy.DefaultHybridWeights()}})
	engB := strategy.NewEngine()
	engB.WithBreaker(strategy.BreakerConfig{})
	engB.Register(stickyS3)
	recsB, _ := replay.RunEngineWith(test, kb, engB,
		strategy.NewBucket(1.0, "S3_Hybrid").WithSeed(seed), check, oracle.Cost, oracle.Latency)

	costA := sumCost(recsA)
	costB := sumCost(recsB)

	fmt.Println("==== P4-2 会话粘性 sticky（同一用户固定渠道）====")
	fmt.Printf("  无粘性: 同用户渠道切换 %d 次 | 总成本 %.2f 元\n", swA, costA)
	fmt.Printf("  带粘性: 同用户渠道切换 %d 次 | 总成本 %.2f 元（绑定 %d 个会话）\n",
		stickyS3.Switches(), costB, stickyS3.Bound())
	if swA > 0 {
		fmt.Printf("  → 切换次数降 %.0f%%（前缀缓存命中率↑、用户感知稳定）；成本变化 %+.2f%%\n\n",
			float64(swA-stickyS3.Switches())/float64(swA)*100, (costB/costA-1)*100)
	} else {
		fmt.Println("  → 本批数据无切换（数据太稀疏），效果不明显")
	}
}

// ---------------------------------------------------------------------------
// P4-3 约束式选路 S5
// ---------------------------------------------------------------------------
func part3(test []logreader.LogRecord, kb *stats.Knowledge, check successFn, oracle *sim.Oracle, seed int64) {
	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})

	// 关键：约束选路判断延迟必须用「实时探测」，不能只看历史平均——
	// 历史平均看不到"渠道5 正在故障"（真值延迟被拉高），会把请求发给正在故障的渠道。
	// 这里用 Oracle 对回放时段算出的「最坏情况延迟」当实时探测结果
	// （故障窗口内渠道5 真值延迟 ×3 ≈ 2100ms > 1.5s 上限 → 被约束排除）。
	realLat := map[int]int{}
	for _, rec := range test {
		for _, id := range []int{1, 2, 3, 4, 5, 6} {
			if lat := oracle.Latency(rec, id); lat > realLat[id] {
				realLat[id] = lat
			}
		}
	}
	prober := health.NewProber([]int{1, 2, 3, 4, 5, 6}, time.Minute, func(id int) health.State {
		return health.State{ChannelID: id, Up: true, LatencyMs: realLat[id], ErrorRate: 0.01}
	})
	prober.ProbeOnce()
	eng.WithHealthProber(prober)

	_, s0 := replay.RunEngineWith(test, kb, eng, nil, check, oracle.Cost, oracle.Latency)
	_, s1 := replay.RunEngineWith(test, kb, eng, strategy.NewBucket(1.0, "S1_MinCost").WithSeed(seed), check, oracle.Cost, oracle.Latency)
	_, s2 := replay.RunEngineWith(test, kb, eng, strategy.NewBucket(1.0, "S2_MinLat").WithSeed(seed), check, oracle.Cost, oracle.Latency)
	_, s5 := replay.RunEngineWith(test, kb, eng, strategy.NewBucket(1.0, "S5_Constrained").WithSeed(seed), check, oracle.Cost, oracle.Latency)

	fmt.Println("==== P4-3 约束式选路 S5（实时延迟 ≤1.5s 里挑最便宜）====")
	fmt.Printf("  实时延迟参考(最坏情况): 渠道1=%dms 渠道2=%dms 渠道3=%dms 渠道4=%dms 渠道5=%dms 渠道6=%dms\n",
		realLat[1], realLat[2], realLat[3], realLat[4], realLat[5], realLat[6])
	fmt.Printf("  %-18s %10s %12s %12s\n", "策略", "平均延迟", "每请求成本", "相对 S0 成本")
	s0avg := s0.TotalCost / float64(max1(s0.Requests))
	for _, r := range []struct {
		id string
		re *replay.EngineResult
	}{{"S0_现状", s0}, {"S1_无脑最便宜", s1}, {"S2_无脑最快", s2}, {"S5_约束选路", s5}} {
		avg := r.re.TotalCost / float64(max1(r.re.Requests))
		fmt.Printf("  %-18s %8dms %12.3f %+11.1f%%\n", r.id, r.re.AvgLatency, avg, (avg/s0avg-1)*100)
	}
	fmt.Printf("  → S5 用实时延迟做约束，避开正在故障的渠道5，得到「1.5s 内最便宜」\n\n")
}

// ---------------------------------------------------------------------------
// P4-4 Prometheus 指标 + 毛刺下钻
// ---------------------------------------------------------------------------
func part4(test []logreader.LogRecord, kb *stats.Knowledge, check successFn, oracle *sim.Oracle, seed int64) {
	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})
	recs_, _ := replay.RunEngineWith(test, kb, eng,
		strategy.NewBucket(1.0, "S3_Hybrid").WithSeed(seed), check, oracle.Cost, oracle.Latency)

	h := metrics.NewHistogram("req_latency_ms", "请求延迟分布（毛刺带 trace_id 可下钻）",
		[]float64{100, 300, 800, 1500, 3000, 6000}, nil)
	type spike struct {
		traceID, strategy, at, model string
		channel                      int
		latency                      int
		cost                         float64
		retried                      bool
	}
	var spikes []spike
	for i, r := range recs_ {
		tid := fmt.Sprintf("trace-%05d", i)
		h.Observe(float64(r.LatencyMs), map[string]string{"trace_id": tid, "strategy_id": r.StrategyID})
		if r.LatencyMs > 3000 {
			spikes = append(spikes, spike{
				traceID: tid, strategy: r.StrategyID,
				at:      time.Unix(r.Time, 0).Format("01-02 15:04"),
				channel: r.ChannelID, model: r.Model,
				latency: r.LatencyMs, cost: r.Cost, retried: r.Retried,
			})
		}
	}

	fmt.Println("==== P4-4 Prometheus 指标 + 毛刺下钻（Exemplar demo 版）====")
	fmt.Println("  /api/metrics 输出（节选）:")
	text := textOf(h)
	fmt.Println("  " + joinLines(text, 0, 3))
	fmt.Println("  …")
	fmt.Println("  " + joinLines(text, -3, 0))

	fmt.Printf("\n  毛刺（延迟 > 3s）共 %d 条，点第一条下钻:\n", len(spikes))
	if len(spikes) > 0 {
		s := spikes[0]
		fmt.Printf("    trace_id=%s\n", s.traceID)
		fmt.Printf("      时间 %s | 策略 %s | 渠道 %d | 模型 %s | 延迟 %dms | 成本 %.3f 元 | 重试=%v\n",
			s.at, s.strategy, s.channel, s.model, s.latency, s.cost, s.retried)
		fmt.Printf("    → Exemplar：指标行上挂着 %s，点毛刺即可跳到这条请求的完整决策链\n\n", s.traceID)
	} else {
		fmt.Println("    本批数据无毛刺")
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

type agg struct {
	name string
	n    int
	lat  int
	cost float64
}

func (a *agg) add(r replay.Record) {
	a.n++
	a.lat += r.LatencyMs
	a.cost += r.Cost
}

func (a *agg) avgLat() float64 {
	if a.n <= 0 {
		return 0
	}
	return float64(a.lat) / float64(a.n)
}

func (a *agg) avgCost() float64 {
	if a.n <= 0 {
		return 0
	}
	return a.cost / float64(a.n)
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

func sumCost(recs []replay.Record) float64 {
	s := 0.0
	for _, r := range recs {
		s += r.Cost
	}
	return s
}

// countSwitches 统计同一会话相邻请求换了渠道的次数（回放记录已是时间序）
func countSwitches(recs []replay.Record) int {
	prev := map[string]int{}
	sw := 0
	for _, r := range recs {
		if p, ok := prev[r.Group]; ok && p != r.ChannelID {
			sw++
		}
		prev[r.Group] = r.ChannelID
	}
	return sw
}

func textOf(h *metrics.Histogram) string {
	var sb strings.Builder
	h.WriteText(&sb)
	return sb.String()
}

// joinLines 截取多行文本（from<0 表示从末尾倒数）
func joinLines(s string, from, to int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if from < 0 {
		from = len(lines) + from
	}
	if to <= 0 {
		to = len(lines) + to
	}
	if from < 0 {
		from = 0
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from >= to {
		return ""
	}
	return strings.Join(lines[from:to], "\n")
}
