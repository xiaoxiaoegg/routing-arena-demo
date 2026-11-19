// cmd/scenarios/main.go
// 场景矩阵：同一套「训练集建知识库 → 回放集样本外评估（真值判官）」流程，
// 跑 5 类业务场景，出一张对照表：
//
//	① 正常运营    ② 单点故障    ③ 限流    ④ 涨价    ⑤ 高峰并发
//
// 与旧回放的区别（v6 方法论升级）：
//   - 样本外：前 70% 日志建知识库（策略"见过"的历史），后 30% 做评估（没见过）
//   - 真值判官：成本/延迟用 sim.Oracle（生成器画像），不是用知识库均价自证
//   - 统计显著：节省率带 95% 置信区间，区间不跨零才算"省得显著"
//
// 跑法：go run ./cmd/scenarios [-records 6000]
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

// scenario 一个业务场景
type scenario struct {
	name    string
	cfg     sim.Config
	quota   map[int]int // 并发配额（nil = 不启用水位）
	active  map[int]int // 活跃并发注入（配合 quota 模拟限流/高峰）
	cand    string      // 候选策略
	note    string
	rolling bool // 知识库滚动更新（用全量日志建库，看到涨价/故障后的最新价格）
}

func main() {
	records := flag.Int("records", 3000, "每场景模拟日志条数（默认 3000）")
	flag.Parse()

	base := sim.DefaultConfig()
	base.Records = *records

	// ① 正常运营：无故障、无涨价
	normal := base
	normal.Channels = cleanProfiles(base.Channels)

	// ② 单点故障：S1 最爱的便宜渠道2 从训练期末段一路故障到回放期（错误率 80%）
	fault := base
	ch := fault.Channels
	ch[1].Fault = sim.Window{From: 0.45, To: 0.95}
	ch[1].FaultErrorRate = 0.80
	ch[1].FaultLatencyX = 3.0
	fault.Channels = ch

	// ③ 限流：最便宜的渠道2 与 又快又便宜的渠道5 并发到顶（96% 配额）
	throttle := base
	throttle.Channels = cleanProfiles(base.Channels) // 无故障干扰，纯限流对照

	// ④ 涨价：S1 最爱的便宜渠道2 在回放期（后 30%）涨价 60%——训练知识库看不到
	hike := base
	hike.Channels = cleanProfiles(base.Channels)
	hike.Channels[1].Hike = sim.Window{From: 0.70, To: 1.0}
	hike.Channels[1].HikeRatio = 1.60

	// ⑤ 高峰并发：3 倍流量 + 全部渠道水位顶到 95%（只剩个别渠道能接）
	peak := base
	peak.Records = *records * 3
	peak.Channels = cleanProfiles(base.Channels)

	scen := []scenario{
		{name: "① 正常运营", cfg: normal, cand: "S1_MinCost", note: "无故障无涨价，基线对照"},
		{name: "② 单点故障", cfg: fault, cand: "S1_MinCost", note: "便宜渠道2 故障期错误率80%，严格归因把重试代价算在策略头上"},
		{name: "③ 限流", cfg: throttle, quota: map[int]int{2: 50, 5: 60}, active: map[int]int{2: 48, 5: 58}, cand: "S1_MinCost", note: "便宜渠道并发到顶(96%)被水位踢出 → S1 被迫绕开"},
		{name: "④ 涨价", cfg: hike, cand: "S1_MinCost", note: "便宜渠道2 回放期涨价60%，训练库看不到 → 对比知识库滚动更新"},
		{name: "④b 涨价+滚动库", cfg: hike, cand: "S1_MinCost", rolling: true, note: "知识库含涨价后的价格（上限参考）"},
		{name: "⑤ 高峰并发", cfg: peak, quota: map[int]int{1: 40, 2: 40, 3: 40, 4: 40, 5: 40, 6: 40}, active: map[int]int{1: 38, 2: 38, 3: 38, 4: 38, 5: 38}, cand: "S1_MinCost", note: "3倍流量+5/6渠道水位到顶 → 看保稳代价"},
	}

	fmt.Printf("场景矩阵（模拟数据 %d 条/场景，训练 70%% 建库 → 回放 30%% 真值评估，候选 %s）\n\n",
		*records, "S1_MinCost")
	fmt.Printf("%-14s %10s %10s %-18s %5s %18s %8s %6s\n",
		"场景", "S0成本", "候选成本", "节省(均值±95%CI)", "显著", "延迟 S0→候选", "成功率", "回退")
	fmt.Println("----------------------------------------------------------------------------------------------------")

	for _, sc := range scen {
		row, err := runScenario(sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", sc.name, err)
			continue
		}
		fmt.Println(row)
	}
	fmt.Println("----------------------------------------------------------------------------------------------------")
	fmt.Println("口径：成本/延迟为生成器真值（Oracle）；节省 = (S0−候选)/S0；显著 = 候选成本区间整体低于 S0。")
	fmt.Println()
	fmt.Println("场景说明：")
	for _, sc := range scen {
		fmt.Printf("  %-14s %s\n", sc.name, sc.note)
	}
}

func runScenario(sc scenario) (string, error) {
	recs, _ := sim.Generate(sc.cfg)
	train, test := stats.Split(recs, 0.7)

	// 知识库：默认只用训练集；rolling 用全量（模拟滚动更新的上限）
	kbSrc := train
	if sc.rolling {
		kbSrc = recs
	}
	kb := stats.Aggregate(kbSrc)

	oracle := sim.NewOracle(sc.cfg)
	check := sim.FaultChecker(sim.FaultWindows(sc.cfg), sc.cfg.Seed)

	eng := strategy.NewEngine()
	eng.WithBreaker(strategy.BreakerConfig{})
	if sc.quota != nil {
		wm := strategy.NewWatermark(sc.quota)
		for chID, n := range sc.active {
			wm.SetActive(chID, n)
		}
		eng.WithWatermark(wm)
	}

	// S0 基线（现状）与候选策略，在同一把尺子（真值判官）下回放
	s0Recs, _ := replay.RunEngineWith(test, kb, eng, nil, check, oracle.Cost, oracle.Latency)
	candRecs, candRes := replay.RunEngineWith(test, kb, eng,
		strategy.NewBucket(1.0, sc.cand).WithSeed(sc.cfg.Seed), check, oracle.Cost, oracle.Latency)

	s0Cost, candCost := 0.0, 0.0
	for _, r := range s0Recs {
		s0Cost += r.Cost
	}
	for _, r := range candRecs {
		candCost += r.Cost
	}

	// 节省率 + 95% 置信区间（配对差值，与 arena 同口径；不依赖 Recommend 的样本门槛）
	dom := dominantModelOf(s0Recs, candRecs)
	var s0m, candm []replay.Record
	for _, r := range s0Recs {
		if r.Model == dom {
			s0m = append(s0m, r)
		}
	}
	for _, r := range candRecs {
		if r.Model == dom {
			candm = append(candm, r)
		}
	}
	n := len(s0m)
	if len(candm) < n {
		n = len(candm)
	}
	if n == 0 {
		return fmt.Sprintf("%-14s 无 %s 样本，无法评估", sc.name, dom), nil
	}
	diffs := make([]float64, n)
	for i := 0; i < n; i++ {
		diffs[i] = s0m[i].Cost - candm[i].Cost
	}
	ci := eval.MeanCI(diffs)
	s0Mean := s0Cost / float64(len(s0Recs))
	savedPct := ci.Mean / s0Mean * 100
	halfPct := ci.HalfWidth / s0Mean * 100
	sig := ci.Low() > 0

	var s0Lat, candLat, s0N, candN int
	for _, r := range s0m {
		s0Lat += r.LatencyMs
		s0N++
	}
	for _, r := range candm {
		candLat += r.LatencyMs
		candN++
	}
	succ := "-"
	if candN > 0 {
		ok := 0
		for _, r := range candm {
			if r.Success {
				ok++
			}
		}
		succ = fmt.Sprintf("%.0f%%", float64(ok)/float64(candN)*100)
	}

	verdict := "维持现状"
	switch {
	case savedPct < -0.5:
		verdict = "⚠ 更贵，别切"
	case sig && savedPct > 0:
		verdict = "✅ 建议晋升"
	case sig:
		verdict = "成本显著省，但综合分未超"
	default:
		verdict = "省得不显著，继续观察"
	}

	rolling := ""
	if sc.rolling {
		rolling = "（滚动库）"
	}
	return fmt.Sprintf("%-14s %10.2f %10.2f %-18s %5s %8d→%-6d %8s %6d  %s %s",
		sc.name, s0Cost, candCost, fmt.Sprintf("%.1f%% ± %.1f%%", savedPct, halfPct),
		yesNo(sig), s0Lat/max1(s0N), candLat/max1(candN), succ, candRes.Fallbacks, verdict, rolling), nil
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
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
	for m, c := range count {
		if c > bestN {
			best, bestN = m, c
		}
	}
	return best
}

// cleanProfiles 去掉所有渠道的涨价/故障窗口（构造"正常"基线）
func cleanProfiles(chs []sim.ChannelProfile) []sim.ChannelProfile {
	out := make([]sim.ChannelProfile, len(chs))
	copy(out, chs)
	for i := range out {
		out[i].Hike = sim.Window{}
		out[i].Fault = sim.Window{}
	}
	return out
}
