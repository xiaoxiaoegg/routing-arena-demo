// internal/sim/generator.go
// 模拟消费日志生成器：在保持「模拟数据」的前提下，把数据做厚。
//
// 相比手写的 40 条 CSV，这里补上了真实流量本来就有的东西：
//   - 多渠道（默认 6 个，各有价格/延迟/错误率画像）
//   - 多模型（默认 3 个）
//   - 小时级流量曲线（业务高峰时段记录更密，凌晨稀疏）
//   - 渠道涨价窗口（同一渠道不同时段单价不同）
//   - 渠道故障窗口（窗口内延迟飙升 + 错误率飙升，供回放模拟真实故障期）
//
// 生成结果直接是 []logreader.LogRecord，与读 CSV 完全等价，可无缝替换。
// 全部随机都走 rand.New(rand.NewSource(Seed))，同 Seed 结果完全可复现。

package sim

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"routing-arena-demo/internal/logreader"
)

// Window 时间窗口，用「占整段日志的比例 [0,1]」表示，方便对任意天数复用。
// 零值窗口（From==To==0）视为不生效。
type Window struct {
	From float64
	To   float64
}

// Active 窗口是否生效，以及某时间比例 p 是否落在窗口内
func (w Window) Active() bool { return w.To > w.From }
func (w Window) Hit(p float64) bool {
	return w.Active() && p >= w.From && p < w.To
}

// ChannelProfile 一个上游渠道的画像
type ChannelProfile struct {
	ID             int
	Weight         float64 // 渠道分流权重（相对值，内部会归一化）
	CostPer1K      float64 // 基础千 token 成本（元）
	LatencyMs      int     // 基础延迟（ms）
	ErrorRate      float64 // 基础错误率
	Hike           Window  // 涨价窗口
	HikeRatio      float64 // 涨价倍率（1.3 = 涨 30%）
	Fault          Window  // 故障窗口
	FaultErrorRate float64 // 故障期错误率
	FaultLatencyX  float64 // 故障期延迟倍数
}

// FaultWindow 化为绝对时间的故障窗口（回放判"请求是否成功"用）
type FaultWindow struct {
	ChannelID int
	StartUnix int64
	EndUnix   int64
	ErrorRate float64
}

// Config 生成配置
type Config struct {
	Records   int              // 生成条数（默认 2000）
	Days      int              // 时间跨度（默认 7 天）
	Seed      int64            // 随机种子（默认 42，固定=可复现）
	StartUnix int64            // 起始时间（默认 2026-09-01 00:00 +08:00）
	Models    []string         // 模型列表（默认 3 个）
	Channels  []ChannelProfile // 渠道画像（nil = DefaultChannels()）
	Groups    int              // 用户/会话数（默认 50；每条记录随机归属一个用户，供粘性路由演示）
}

// DefaultStartUnix 2026-09-01 00:00:00 +08:00
const DefaultStartUnix = int64(1788192000)

// DefaultConfig 默认配置：2000 条 / 7 天 / 6 渠道 / 3 模型 / 50 用户
func DefaultConfig() Config {
	return Config{
		Records:   2000,
		Days:      7,
		Seed:      42,
		StartUnix: DefaultStartUnix,
		Models:    []string{"gpt-4o", "claude-sonnet-4", "deepseek-v3"},
		Channels:  DefaultChannels(),
		Groups:    50,
	}
}

// DefaultChannels 6 个性格分明的渠道：
//
//	1 老牌稳定（贵但稳，占比最大）
//	2 低价慢（最便宜但最慢）
//	3 均衡（中价中速，后段涨价）
//	4 快而贵（最贵但最快）
//	5 又快又便宜（新渠道，便宜且快，但中途故障一段时间）
//	6 中庸（价格延迟都居中，错误率最低）
func DefaultChannels() []ChannelProfile {
	return []ChannelProfile{
		{ID: 1, Weight: 0.30, CostPer1K: 0.340, LatencyMs: 2800, ErrorRate: 0.005, FaultLatencyX: 2.5},
		{ID: 2, Weight: 0.20, CostPer1K: 0.245, LatencyMs: 3200, ErrorRate: 0.012, FaultLatencyX: 2.5},
		{ID: 3, Weight: 0.15, CostPer1K: 0.270, LatencyMs: 950, ErrorRate: 0.006,
			Hike: Window{From: 0.60, To: 0.80}, HikeRatio: 1.35, FaultLatencyX: 2.5},
		{ID: 4, Weight: 0.12, CostPer1K: 0.315, LatencyMs: 520, ErrorRate: 0.008, FaultLatencyX: 2.5},
		{ID: 5, Weight: 0.15, CostPer1K: 0.255, LatencyMs: 700, ErrorRate: 0.015,
			Fault: Window{From: 0.72, To: 0.86}, FaultErrorRate: 0.60, FaultLatencyX: 3.0},
		{ID: 6, Weight: 0.08, CostPer1K: 0.290, LatencyMs: 1400, ErrorRate: 0.004, FaultLatencyX: 2.5},
	}
}

// hourWeights 一天 24 小时的流量权重（业务高峰 9-11 / 14-17，凌晨最低）
var hourWeights = [24]float64{
	2, 2, 2, 2, 2, 3, 4, 6, 9, 13, 15, 14,
	7, 8, 13, 14, 12, 10, 6, 5, 5, 4, 3, 2,
}

// Generate 生成模拟消费日志 + 对应的绝对时间故障窗口
func Generate(cfg Config) ([]logreader.LogRecord, []FaultWindow) {
	if cfg.Records <= 0 {
		cfg.Records = 2000
	}
	if cfg.Days <= 0 {
		cfg.Days = 7
	}
	if cfg.StartUnix == 0 {
		cfg.StartUnix = DefaultStartUnix
	}
	if len(cfg.Models) == 0 {
		cfg.Models = DefaultConfig().Models
	}
	if len(cfg.Channels) == 0 {
		cfg.Channels = DefaultChannels()
	}
	if cfg.Groups <= 0 {
		cfg.Groups = 50
	}

	rng := rand.New(rand.NewSource(cfg.Seed))
	totalHours := cfg.Days * 24

	// 小时累积分布：让记录按业务曲线落在不同时段
	cum := make([]float64, totalHours)
	sum := 0.0
	for h := 0; h < totalHours; h++ {
		sum += hourWeights[h%24]
		cum[h] = sum
	}

	chWeightSum := 0.0
	for _, c := range cfg.Channels {
		chWeightSum += c.Weight
	}
	modelWeight := make([]float64, len(cfg.Models))
	mSum := 0.0
	for i := range cfg.Models {
		// 第一个模型占大头（模拟主用模型），其余按序递减
		modelWeight[i] = 1.0 / float64(i+1)
		mSum += modelWeight[i]
	}
	for i := range modelWeight {
		modelWeight[i] /= mSum
	}

	recs := make([]logreader.LogRecord, 0, cfg.Records)
	span := float64(cfg.Days) * 24 * 3600
	for i := 0; i < cfg.Records; i++ {
		p := (float64(i) + 0.5) / float64(cfg.Records) // 时间比例 [0,1)，用于小时曲线采样
		hourIdx := sort.SearchFloat64s(cum, p*sum)
		if hourIdx >= totalHours {
			hourIdx = totalHours - 1
		}
		t := cfg.StartUnix + int64(hourIdx)*3600 + int64(rng.Intn(3600))
		// 窗口判定用「记录实际落点的时间比例」，与 Oracle 完全同口径
		// （不能用索引比例 p：小时曲线会让同索引比例对应的真实时间偏移，窗口边界会错位）
		pAt := float64(t-cfg.StartUnix) / span
		if pAt < 0 {
			pAt = 0
		}
		if pAt >= 1 {
			pAt = 0.999999
		}

		// 选渠道（按权重）
		ch := cfg.Channels[pickByWeight(rng, cfg.Channels, chWeightSum)]
		// 选模型（按权重）
		model := cfg.Models[pickByFloat(rng, modelWeight)]

		// token 数：按模型略有差异，加随机波动
		modelScale := 1.0 - 0.15*float64(indexOf(cfg.Models, model))
		prompt := int((400 + float64(rng.Intn(2600))) * modelScale)
		if prompt < 50 {
			prompt = 50
		}
		completion := int((150 + float64(rng.Intn(1300))) * modelScale)
		if completion < 20 {
			completion = 20
		}
		tokens := prompt + completion

		// 成本：基础单价 × token；命中涨价窗口则上浮；再加 ±8% 噪声
		costPer1K := ch.CostPer1K
		if ch.Hike.Hit(pAt) {
			costPer1K *= ch.HikeRatio
		}
		quota := costPer1K / 1000 * float64(tokens) * (0.92 + 0.16*rng.Float64())

		// 延迟：基础延迟 × 噪声；命中故障窗口则放大
		lat := float64(ch.LatencyMs) * (0.85 + 0.30*rng.Float64())
		if ch.Fault.Hit(pAt) {
			lat *= ch.FaultLatencyX
		}

		recs = append(recs, logreader.LogRecord{
			Time:             t,
			Group:            fmt.Sprintf("user-%03d", rng.Intn(cfg.Groups)),
			Model:            model,
			ChannelID:        ch.ID,
			PromptTokens:     prompt,
			CompletionTokens: completion,
			Quota:            round2(quota),
			UseTimeMs:        int(lat),
		})
	}

	return recs, FaultWindows(cfg)
}

// FaultWindows 把比例窗口换算成绝对时间窗口
func FaultWindows(cfg Config) []FaultWindow {
	if cfg.Days <= 0 {
		cfg.Days = 7
	}
	if cfg.StartUnix == 0 {
		cfg.StartUnix = DefaultStartUnix
	}
	if len(cfg.Channels) == 0 {
		cfg.Channels = DefaultChannels()
	}
	span := int64(cfg.Days) * 24 * 3600

	var out []FaultWindow
	for _, c := range cfg.Channels {
		if !c.Fault.Active() {
			continue
		}
		out = append(out, FaultWindow{
			ChannelID: c.ID,
			StartUnix: cfg.StartUnix + int64(c.Fault.From*float64(span)),
			EndUnix:   cfg.StartUnix + int64(c.Fault.To*float64(span)),
			ErrorRate: c.FaultErrorRate,
		})
	}
	return out
}

// FaultChecker 返回回放用的「请求是否成功」判定函数：
// 命中故障窗口 → 按该窗口错误率判定失败（用 时间+渠道+seed 做哈希，保证可复现且与调用顺序无关）。
// 不传 windows 时返回 nil（等价于全部成功）。
func FaultChecker(windows []FaultWindow, seed int64) func(logreader.LogRecord, int) bool {
	if len(windows) == 0 {
		return nil
	}
	return func(rec logreader.LogRecord, chosen int) bool {
		for _, w := range windows {
			if w.ChannelID != chosen || rec.Time < w.StartUnix || rec.Time >= w.EndUnix {
				continue
			}
			// 稳定哈希 → [0,1)，同一条记录判定结果固定
			h := uint64(rec.Time)*2654435761 + uint64(chosen)*40503 + uint64(seed)*2246822519
			h ^= h >> 33
			h *= 0xff51afd7ed558ccd
			h ^= h >> 33
			return float64(h%10000)/10000.0 >= w.ErrorRate
		}
		return true
	}
}

// pickByWeight 按渠道权重抽一个下标
func pickByWeight(rng *rand.Rand, chs []ChannelProfile, sum float64) int {
	r := rng.Float64() * sum
	acc := 0.0
	for i, c := range chs {
		acc += c.Weight
		if r < acc {
			return i
		}
	}
	return len(chs) - 1
}

// pickByFloat 按权重数组抽一个下标
func pickByFloat(rng *rand.Rand, w []float64) int {
	r := rng.Float64()
	acc := 0.0
	for i, x := range w {
		acc += x
		if r < acc {
			return i
		}
	}
	return len(w) - 1
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return 0
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
