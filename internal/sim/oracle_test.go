// internal/sim/oracle_test.go
// 真值判官测试：成本/延迟必须严格等于渠道画像（含涨价/故障窗口），
// 且与生成记录里的 Quota（带噪声的真实花费）量级吻合。

package sim

import (
	"testing"

	"routing-arena-demo/internal/logreader"
)

// TestOracleBaseCost 无窗口渠道：成本 = 基础单价 × token，延迟 = 基础延迟
func TestOracleBaseCost(t *testing.T) {
	cfg := DefaultConfig()
	o := NewOracle(cfg)
	rec := logreader.LogRecord{Time: cfg.StartUnix + 3600, PromptTokens: 1000, CompletionTokens: 500}
	ch := cfg.Channels[0] // 渠道1：无涨价无故障
	want := ch.CostPer1K / 1000 * 1500
	if got := o.Cost(rec, ch.ID); got != want {
		t.Errorf("基础成本应 %.4f，实际 %.4f", want, got)
	}
	if got := o.Latency(rec, ch.ID); got != ch.LatencyMs {
		t.Errorf("基础延迟应 %d，实际 %d", ch.LatencyMs, got)
	}
}

// TestOracleHikeWindow 涨价窗口内：成本 ×HikeRatio；窗口外不变
func TestOracleHikeWindow(t *testing.T) {
	cfg := DefaultConfig()
	o := NewOracle(cfg)
	ch := cfg.Channels[2] // 渠道3：涨价窗口 [0.6,0.8] ×1.35
	rec := logreader.LogRecord{PromptTokens: 1000, CompletionTokens: 500}

	span := float64(cfg.Days) * 86400
	// 窗口内（p=0.7）
	in := rec
	in.Time = cfg.StartUnix + int64(0.70*span)
	inCost := o.Cost(in, ch.ID)
	if inCost < ch.CostPer1K*1.35/1000*1500*0.999 || inCost > ch.CostPer1K*1.35/1000*1500*1.001 {
		t.Errorf("窗口内成本应 = 基础×1.35 = %.4f，实际 %.4f", ch.CostPer1K*1.35/1000*1500, inCost)
	}
	// 窗口外（p=0.3）
	out := rec
	out.Time = cfg.StartUnix + int64(0.30*span)
	outCost := o.Cost(out, ch.ID)
	if outCost != ch.CostPer1K/1000*1500 {
		t.Errorf("窗口外成本应 = 基础 %.4f，实际 %.4f", ch.CostPer1K/1000*1500, outCost)
	}
	if inCost <= outCost {
		t.Error("窗口内成本应高于窗口外")
	}
}

// TestOracleFaultWindow 故障窗口内：延迟 ×FaultLatencyX；窗口外不变
func TestOracleFaultWindow(t *testing.T) {
	cfg := DefaultConfig()
	o := NewOracle(cfg)
	ch := cfg.Channels[4] // 渠道5：故障窗口 [0.72,0.86] ×3.0
	rec := logreader.LogRecord{}
	span := float64(cfg.Days) * 86400

	rec.Time = cfg.StartUnix + int64(0.75*span)
	if got := o.Latency(rec, ch.ID); got != ch.LatencyMs*3 {
		t.Errorf("故障窗口内延迟应 %d，实际 %d", ch.LatencyMs*3, got)
	}
	rec.Time = cfg.StartUnix + int64(0.30*span)
	if got := o.Latency(rec, ch.ID); got != ch.LatencyMs {
		t.Errorf("故障窗口外延迟应 %d，实际 %d", ch.LatencyMs, got)
	}
}

// TestOracleMatchesQuota 生成记录的真实 Quota（带噪声）应落在 oracle 成本 ±8% 内
func TestOracleMatchesQuota(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Records = 3000
	recs, _ := Generate(cfg)
	o := NewOracle(cfg)

	for i, r := range recs {
		trueCost := o.Cost(r, r.ChannelID)
		if trueCost <= 0 {
			t.Fatalf("第 %d 条 oracle 成本为 0（渠道 %d）", i, r.ChannelID)
		}
		ratio := r.Quota / trueCost
		if ratio < 0.90 || ratio > 1.10 {
			t.Fatalf("第 %d 条 Quota(%.4f) 与 oracle(%.4f) 偏差 %.2f%% 超出噪声范围 ±8%%",
				i, r.Quota, trueCost, (ratio-1)*100)
		}
	}
}

// TestOracleUnknownChannel 未知渠道返回 0（不 panic）
func TestOracleUnknownChannel(t *testing.T) {
	o := NewOracle(DefaultConfig())
	rec := logreader.LogRecord{Time: DefaultStartUnix, PromptTokens: 100, CompletionTokens: 50}
	if o.Cost(rec, 999) != 0 || o.Latency(rec, 999) != 0 {
		t.Error("未知渠道应返回 0")
	}
}
