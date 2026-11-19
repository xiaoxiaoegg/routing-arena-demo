// internal/strategy/adaptive.go
// v6 时段自适应权重：同一把策略（S3/S4），不同时段用不同评分权重。
//
// 业务直觉：
//   - 白天业务高峰（9-18 点）：用户在等结果，体验优先 → 延迟权重大
//   - 晚间（19-23 点）：批处理/离线任务多 → 省钱优先
//   - 凌晨（0-6 点）：流量低谷，能省就省 → 极致成本
//
// 权重归一化到总和 1（成本+延迟+健康）。

package strategy

// AdaptiveWeights 按小时返回 S3/S4 综合策略的评分权重
func AdaptiveWeights(hour int) HybridWeights {
	switch {
	case hour >= 9 && hour <= 18:
		// 业务高峰：重延迟（用户在线等结果，TTFT 优先）
		return HybridWeights{Cost: 0.25, Latency: 0.45, Health: 0.30}
	case hour >= 19 || hour <= 6:
		// 夜间/凌晨：重成本（离线批处理多，省钱优先）
		return HybridWeights{Cost: 0.50, Latency: 0.20, Health: 0.30}
	default: // 7-8 点：早高峰过渡
		return HybridWeights{Cost: 0.35, Latency: 0.35, Health: 0.30}
	}
}

// AdaptiveProfile 自适应权重的说明文字（展示用）
func AdaptiveProfile(hour int) string {
	w := AdaptiveWeights(hour)
	kind := "夜间省钱优先"
	if hour >= 9 && hour <= 18 {
		kind = "白天体验优先（重延迟）"
	} else if hour >= 7 && hour <= 8 {
		kind = "早高峰过渡"
	}
	return kind + "（成本 " + pct1(w.Cost) + "/延迟 " + pct1(w.Latency) + "/健康 " + pct1(w.Health) + "）"
}

func pct1(v float64) string {
	n := int(v*100 + 0.5)
	if n >= 100 {
		return "100%"
	}
	if n <= 0 {
		return "0%"
	}
	return itoa(n) + "%"
}

// itoa 小整数转字符串
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
