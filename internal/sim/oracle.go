// internal/sim/oracle.go
// 真值判官（Oracle）：给定一条记录 + "如果选了某个渠道"，算出该渠道在
// 这条记录上真实的成本与延迟。
//
// 为什么需要它：
//   旧回放用「知识库均价」给策略算反事实成本——而知识库是从同一批日志里
//   聚合出来的。S1_MinCost 必然选到"自己定价最低"的渠道，节省率必然好看，
//   这是样本内自证。生成器知道每个渠道的真实画像（价格/延迟/涨价/故障），
//   所以它能当"真值判官"：样本外评估时，用真值给 S0 和候选策略都打分，
//   S0 和候选在同一把尺子下比，谁省多少才可信。
//
//   接入生产网关后，对应物是"真实账单核对"：候选策略的账面成本要用上游账单
//   真值核对，而不是用自己聚合的均价。
package sim

import (
	"routing-arena-demo/internal/logreader"
)

// Oracle 真值判官：持有渠道画像，按记录时间落在的窗口（涨价/故障）算真值
type Oracle struct {
	start int64
	span  float64 // 总时长（秒）
	byID  map[int]ChannelProfile
}

// NewOracle 根据生成配置创建判官。cfg 与生成数据时用的配置一致即可。
func NewOracle(cfg Config) *Oracle {
	if cfg.Days <= 0 {
		cfg.Days = 7
	}
	if cfg.StartUnix == 0 {
		cfg.StartUnix = DefaultStartUnix
	}
	if len(cfg.Channels) == 0 {
		cfg.Channels = DefaultChannels()
	}
	byID := make(map[int]ChannelProfile, len(cfg.Channels))
	for _, c := range cfg.Channels {
		byID[c.ID] = c
	}
	return &Oracle{
		start: cfg.StartUnix,
		span:  float64(cfg.Days) * 24 * 3600,
		byID:  byID,
	}
}

// ratio 记录在整段日志中的时间比例 [0,1)
func (o *Oracle) ratio(t int64) float64 {
	if o.span <= 0 {
		return 0
	}
	p := float64(t-o.start) / o.span
	if p < 0 {
		return 0
	}
	if p >= 1 {
		return 0.999999
	}
	return p
}

// Cost 真值成本：基础单价 ×（涨价窗口倍率）× token 数（元）
func (o *Oracle) Cost(rec logreader.LogRecord, chID int) float64 {
	ch, ok := o.byID[chID]
	if !ok {
		return 0
	}
	per1K := ch.CostPer1K
	if ch.Hike.Hit(o.ratio(rec.Time)) {
		per1K *= ch.HikeRatio
	}
	return per1K / 1000 * float64(rec.PromptTokens+rec.CompletionTokens)
}

// Latency 真值延迟：基础延迟 × 故障窗口放大倍数（ms）
func (o *Oracle) Latency(rec logreader.LogRecord, chID int) int {
	ch, ok := o.byID[chID]
	if !ok {
		return 0
	}
	lat := float64(ch.LatencyMs)
	if ch.Fault.Hit(o.ratio(rec.Time)) && ch.FaultLatencyX > 0 {
		lat *= ch.FaultLatencyX
	}
	return int(lat)
}
