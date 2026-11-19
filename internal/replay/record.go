// internal/replay/record.go
// P1-5 评估数据管道：逐条请求记录"哪个策略决策的、选了谁、结果如何"。
// 这是 P2 聚合/看板的数据基础——每条记录 = 一个可审计的决策事件。

package replay

import (
	"time"

	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/strategy"
)

// Record 单条请求的评估记录（评估数据管道的原始数据）
type Record struct {
	Time             int64   // v6 请求时间（unix 秒）——按小时分桶/审计用
	Group            string  // v6 会话/用户标识——粘性/会话统计用
	StrategyID       string  // 实际生效的策略（可能因回退而不同于期望策略）
	IntendedID       string  // 期望策略（分桶决策/默认策略）
	Fallback         bool    // 是否发生了回退（期望策略选不出 → 回退默认）
	ChannelID        int     // 选中的渠道
	Model            string  // 模型名
	PromptTokens     int     // 输入 token
	CompletionTokens int     // 输出 token
	Cost             float64 // 反事实成本（元）：知识库均价 × 本条 token 数
	LatencyMs        int     // 反事实延迟（ms）：知识库该渠道平均延迟
	Success          bool    // 请求是否成功（P2 评分用：成功率维度）
	// v5 严格归因：请求失败重试时，A(失败) + B(重试) 的总耗时/Token 全算在策略头上。
	Retried  bool    // 是否发生了失败重试
	RetryCost float64 // 重试带来的额外成本（元）
	RetryLatency int // 重试带来的额外延迟（ms）
}

// EngineResult 引擎回放的混合汇总（跨策略，区别于单策略回放的 Result）
type EngineResult struct {
	TotalCost  float64 // 总成本（元）
	AvgLatency int     // 平均延迟（ms）
	Requests   int     // 处理的请求数
	Fallbacks  int     // 回退次数（评估兜底是否频繁）
}

// RunEngine 基于路由引擎的回放：逐条请求走引擎选路（含分桶 + 兜底回退），
// 产出评估记录列表 + 混合汇总。
//   - 期望策略为 S0（默认/未命中分桶）：候选只有实际渠道 → 复现现状
//   - 期望策略为 S1/S2（命中分桶）：候选为全部可用渠道 → 策略自由选
//   - 策略选不出（chosen==0）：引擎回退默认策略，记录 Fallback=true
//
// picker 决定每条请求走哪个策略：*strategy.Bucket（单候选分桶）
// 或 *strategy.TrafficSplitter（多策略流量分配）。
//
// successFn 决定该请求是否成功（P2 评分需要成功率维度）：
//   - nil → 全部成功（历史日志无失败标志时的默认）
//   - 非 nil → 按「本条记录 + 选中渠道」判定（真实场景由上游响应决定，
//     demo 用 sim.FaultChecker 按时间窗口模拟故障期）
// hourOf 取 unix 秒对应的小时 [0,23]（本地时区）
func hourOf(t int64) int {
	return time.Unix(t, 0).Local().Hour()
}

// CostFn 成本函数（真值判官）；nil = 用知识库均价（旧的反事实估算路径）。
// 用生成器画像（sim.Oracle）当真值时，S0 与候选策略在同一把尺子下比，
// 避免"用知识库给自己定价"的样本内自证。
type CostFn func(rec logreader.LogRecord, chosen int) float64

// LatencyFn 延迟函数（真值判官）；nil = 用知识库平均延迟。
type LatencyFn func(rec logreader.LogRecord, chosen int) int

// RunEngine 基于路由引擎的回放（兼容旧接口，成本/延迟用知识库估算）
func RunEngine(recs []logreader.LogRecord, kb *stats.Knowledge, eng *strategy.RoutingEngine, picker strategy.Picker, successFn func(logreader.LogRecord, int) bool) ([]Record, *EngineResult) {
	return RunEngineWith(recs, kb, eng, picker, successFn, nil, nil)
}

// RunEngineWith 与 RunEngine 相同，但可注入成本/延迟真值函数（见 sim.Oracle）。
func RunEngineWith(recs []logreader.LogRecord, kb *stats.Knowledge, eng *strategy.RoutingEngine, picker strategy.Picker, successFn func(logreader.LogRecord, int) bool, costFn CostFn, latFn LatencyFn) ([]Record, *EngineResult) {
	records := make([]Record, 0, len(recs))
	res := &EngineResult{}

	for _, rec := range recs {
		rc := &strategy.RouteContext{Model: rec.Model, Group: rec.Group, Hour: hourOf(rec.Time)}

		// 期望策略：选择器决策（未命中 → 默认策略）
		intended := eng.DefaultID()
		if picker != nil {
			if picked := picker.PickStrategy(); picked != "" {
				intended = picked
			}
		}

		// 候选渠道：S0 复现现状（只有实际渠道）；其他策略自由选
		var candidates []int
		if intended == "S0_Champion" {
			candidates = []int{rec.ChannelID}
		} else {
			candidates = strategy.CandidatesOf(kb, rec.Model)
		}

		// 引擎选路（内部含兜底回退）
		actualID, chosen := eng.SelectChannel(rc, kb, candidates, intended)
		if actualID == "NONE" || chosen == 0 {
			continue // 无渠道可用，跳过（不产生记录）
		}

		// 回退判定：期望非默认策略，但实际生效的是别的策略
		fallback := actualID != intended && intended != eng.DefaultID()
		if fallback {
			res.Fallbacks++
		}

		// 成本/延迟取值：优先真值函数，否则知识库均价（旧路径）
		costOf := func(ch int) float64 {
			if costFn != nil {
				return costFn(rec, ch)
			}
			if cs, ok := kb.ByChannelModel[channelModelKey(ch, rec.Model)]; ok {
				return cs.AvgCostPer1K / 1000 * float64(rec.PromptTokens+rec.CompletionTokens)
			}
			return 0
		}
		latOf := func(ch int) int {
			if latFn != nil {
				return latFn(rec, ch)
			}
			if cs, ok := kb.ByChannelModel[channelModelKey(ch, rec.Model)]; ok {
				return cs.AvgLatencyMs
			}
			return 0
		}

		// 反事实成本/延迟：真值函数或知识库
		success := true
		if successFn != nil {
			success = successFn(rec, chosen)
		}
		rec_ := Record{
			Time:             rec.Time,
			Group:            rec.Group,
			StrategyID:       actualID,
			IntendedID:       intended,
			Fallback:         fallback,
			ChannelID:        chosen,
			Model:            rec.Model,
			PromptTokens:     rec.PromptTokens,
			CompletionTokens: rec.CompletionTokens,
			Success:          success,
			Cost:             costOf(chosen),
			LatencyMs:        latOf(chosen),
		}

		// v5 严格归因：策略选错渠道 → 请求失败 → 重试切到下一可用渠道。
		// A(失败) + B(重试) 的总耗时/Token 全算在策略头上，逼迫策略选得准。
		if !success {
			for _, alt := range candidates {
				if alt == chosen {
					continue // 跳过已失败的渠道
				}
				_, ok := kb.ByChannelModel[channelModelKey(alt, rec.Model)]
				if ok || costFn != nil {
					extraCost := costOf(alt)
					if costFn == nil {
						// 知识库路径：没有该渠道样本时跳过
						if _, ok2 := kb.ByChannelModel[channelModelKey(alt, rec.Model)]; !ok2 {
							continue
						}
						extraCost = costOf(alt)
					}
					rec_.Retried = true
					rec_.RetryCost = extraCost
					rec_.RetryLatency = latOf(alt)
					rec_.Cost += extraCost // 全算在策略头上
					rec_.LatencyMs += rec_.RetryLatency
					rec_.Success = true // 重试后成功（最终用户侧成功）
				}
				break
			}
		}

		records = append(records, rec_)
		res.TotalCost += rec_.Cost
		res.AvgLatency += rec_.LatencyMs
		res.Requests++

		// v6：把本条请求的结果回告引擎（熔断器据此维护策略健康状态）。
		// 策略老选故障渠道 → 失败率超阈值 → 熔断 → 后续请求自动回退。
		eng.ReportOutcome(actualID, success, rec_.LatencyMs)
	}
	if res.Requests > 0 {
		res.AvgLatency /= res.Requests
	}
	return records, res
}
