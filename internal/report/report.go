// internal/report/report.go
// 对比报告：输出各策略回放结果的对比表 + 结论。

package report

import (
	"fmt"

	"routing-arena-demo/internal/replay"
)

// Print 打印对比表，S0 为基线计算各策略偏差
func Print(results []*replay.Result) {
	if len(results) == 0 {
		fmt.Println("无回放结果")
		return
	}

	// 找 S0 基线
	var s0 *replay.Result
	for _, r := range results {
		if r.StrategyID == "S0_Champion" {
			s0 = r
			break
		}
	}

	fmt.Println("策略          总成本(元)   vs S0      平均延迟   请求数")
	fmt.Println("------------------------------------------------------")
	for _, r := range results {
		if s0 != nil && r.StrategyID != "S0_Champion" && s0.TotalCost > 0 {
			delta := (r.TotalCost - s0.TotalCost) / s0.TotalCost * 100
			fmt.Printf("%-14s %10.2f  %+6.1f%%   %8dms  %7d\n",
				r.StrategyID, r.TotalCost, delta, r.AvgLatency, r.Requests)
		} else {
			fmt.Printf("%-14s %10.2f  %7s   %8dms  %7d\n",
				r.StrategyID, r.TotalCost, "-", r.AvgLatency, r.Requests)
		}
	}
	fmt.Println("------------------------------------------------------")

	// P0-5 结论判定：值不值得继续做
	fmt.Println()
	fmt.Println("==== 结论（P0 影子验证）====")
	fmt.Println(Conclusion(results))
}

// Conclusion 返回 P0 结论文本（纯函数，可单测）。
// 规则：候选策略比 S0 省 ≥10% → 值得进 P1；省 5%~10% → 勉强，需更多数据；
//       <5% → 方案价值存疑，建议降级或换方向。
func Conclusion(results []*replay.Result) string {
	var s0 *replay.Result
	for _, r := range results {
		if r.StrategyID == "S0_Champion" {
			s0 = r
			break
		}
	}
	if s0 == nil || s0.TotalCost <= 0 {
		return "无 S0 基线或基线成本为 0，无法下结论。"
	}

	var best *replay.Result
	var bestDelta float64
	for _, r := range results {
		if r.StrategyID == "S0_Champion" {
			continue
		}
		delta := (s0.TotalCost - r.TotalCost) / s0.TotalCost * 100 // 正 = 省钱
		if best == nil || delta > bestDelta {
			best = r
			bestDelta = delta
		}
	}
	if best == nil {
		return "无可比策略，无法下结论。"
	}

	verdict := "【价值存疑】"
	switch {
	case bestDelta >= 10:
		verdict = "【值得继续】"
	case bestDelta >= 5:
		verdict = "【勉强可行】"
	}
	if bestDelta >= 10 || bestDelta >= 5 {
		return fmt.Sprintf("%s%s 较 S0 省 %.1f%%，在 %s 阈值区间 → 建议进入 P1 真实小流量验证。",
			verdict, best.StrategyID, bestDelta, thresholdLabel(bestDelta))
	}
	return fmt.Sprintf("%s最优策略 %s 仅省 %.1f%%，低于 5%% → 建议降级处理或换优化方向。",
		verdict, best.StrategyID, bestDelta)
}

func thresholdLabel(delta float64) string {
	if delta >= 10 {
		return "≥10%"
	}
	return "5%~10%"
}
