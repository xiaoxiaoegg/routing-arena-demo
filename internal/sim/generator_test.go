// internal/sim/generator_test.go
// 模拟数据生成器测试：可复现、画像完整、故障窗口生效。

package sim

import (
	"testing"
)

// TestGenerateDeterministic 同 seed 两次生成必须完全一致（结果可复现）
func TestGenerateDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Records = 500

	a, fa := Generate(cfg)
	b, fb := Generate(cfg)

	if len(a) != len(b) {
		t.Fatalf("两次生成条数不一致: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 条记录不一致:\n%+v\n%+v", i, a[i], b[i])
		}
	}
	if len(fa) != len(fb) {
		t.Fatalf("故障窗口数量不一致: %d vs %d", len(fa), len(fb))
	}
}

// TestGenerateShape 生成结果的画像完整性：条数/渠道/模型/时间跨度/高峰分布
func TestGenerateShape(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Records = 3000
	recs, _ := Generate(cfg)

	if len(recs) != cfg.Records {
		t.Fatalf("条数不符: 期望 %d, 实际 %d", cfg.Records, len(recs))
	}

	chans := map[int]int{}
	models := map[string]int{}
	hours := map[int]int{}
	var minT, maxT int64
	for i, r := range recs {
		chans[r.ChannelID]++
		models[r.Model]++
		hours[(int((r.Time-cfg.StartUnix)/3600))%24]++
		if i == 0 || r.Time < minT {
			minT = r.Time
		}
		if i == 0 || r.Time > maxT {
			maxT = r.Time
		}
		if r.PromptTokens <= 0 || r.CompletionTokens <= 0 || r.Quota <= 0 || r.UseTimeMs <= 0 {
			t.Fatalf("出现非法记录字段: %+v", r)
		}
	}

	if len(chans) != len(cfg.Channels) {
		t.Errorf("渠道覆盖不全: 期望 %d 个, 实际 %d 个 (%v)", len(cfg.Channels), len(chans), chans)
	}
	if len(models) != len(cfg.Models) {
		t.Errorf("模型覆盖不全: 期望 %d 个, 实际 %d 个 (%v)", len(cfg.Models), len(models), models)
	}

	// 时间跨度不超过 Days 天
	span := float64(maxT-minT) / 86400.0
	if span > float64(cfg.Days)+0.1 {
		t.Errorf("时间跨度超界: %.2f 天 > %d 天", span, cfg.Days)
	}

	// 业务高峰（9-17 点）应当明显多于凌晨（0-6 点）
	peak := 0
	for h := 9; h <= 17; h++ {
		peak += hours[h]
	}
	night := 0
	for h := 0; h <= 6; h++ {
		night += hours[h]
	}
	if peak <= night {
		t.Errorf("高峰时段记录数(%d)未多于凌晨(%d)，流量曲线未生效: %v", peak, night, hours)
	}
}

// TestFaultWindowAndChecker 故障窗口必须落在预期渠道/时间上，且判定函数按错误率失败
func TestFaultWindowAndChecker(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Records = 5000
	recs, fws := Generate(cfg)

	if len(fws) != 1 {
		t.Fatalf("默认画像应有 1 个故障窗口，实际 %d", len(fws))
	}
	w := fws[0]
	if w.ChannelID != 5 {
		t.Errorf("故障窗口渠道应为 5，实际 %d", w.ChannelID)
	}
	if w.EndUnix <= w.StartUnix {
		t.Fatalf("故障窗口时间非法: %+v", w)
	}

	check := FaultChecker(fws, cfg.Seed)
	if check == nil {
		t.Fatal("有故障窗口时 FaultChecker 不应返回 nil")
	}

	inWin, inFail, outWin, outFail := 0, 0, 0, 0
	for _, r := range recs {
		inside := r.Time >= w.StartUnix && r.Time < w.EndUnix
		ok := check(r, w.ChannelID)
		if inside {
			inWin++
			if !ok {
				inFail++
			}
		} else {
			outWin++
			if !ok {
				outFail++
			}
		}
	}

	if inWin == 0 {
		t.Fatal("故障窗口内没有样本，窗口换算有误")
	}
	if outFail != 0 {
		t.Errorf("故障窗口外不应有失败，实际 %d 条", outFail)
	}
	rate := float64(inFail) / float64(inWin)
	if rate < 0.45 || rate > 0.75 {
		t.Errorf("故障期失败率应接近 60%%，实际 %.1f%% (样本 %d)", rate*100, inWin)
	}
	if outWin == 0 {
		t.Error("故障窗口外样本为 0，测试无意义")
	}
}

// TestFaultCheckerNil 无故障窗口时返回 nil（等价于全部成功）
func TestFaultCheckerNil(t *testing.T) {
	if FaultChecker(nil, 1) != nil {
		t.Error("无故障窗口时应返回 nil")
	}
	cfg := DefaultConfig()
	cfg.Channels = []ChannelProfile{{ID: 1, Weight: 1, CostPer1K: 0.3, LatencyMs: 1000}}
	if FaultChecker(FaultWindows(cfg), 1) != nil {
		t.Error("画像无故障窗口时应返回 nil")
	}
}
