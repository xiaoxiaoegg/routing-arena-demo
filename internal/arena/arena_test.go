// internal/arena/arena_test.go
// P3-2/P3-3 竞技场管理单测：自动切换、二次确认、冷却、回退留痕。

package arena

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultManualMode(t *testing.T) {
	// 默认手动模式，冠军 S0
	m := NewManager("S0_Champion")
	if m.Mode() != ModeManual {
		t.Fatalf("默认应为手动模式，实际 %s", m.Mode())
	}
	if m.Champion() != "S0_Champion" {
		t.Fatalf("默认冠军应为 S0，实际 %s", m.Champion())
	}
}

func TestAutoRequiresConfirm(t *testing.T) {
	// P3-3 二次确认：未确认时手动→自动被拒
	m := NewManager("S0_Champion")
	if err := m.SetMode(ModeAuto); err == nil {
		t.Fatal("未二次确认时切自动模式应被拒绝")
	}
	// 确认后可切
	m.ConfirmAuto()
	if err := m.SetMode(ModeAuto); err != nil {
		t.Fatalf("确认后切自动模式应成功: %v", err)
	}
	if m.Mode() != ModeAuto {
		t.Fatalf("应处于自动模式")
	}
}

func TestAutoSwitchRequiresAutoMode(t *testing.T) {
	// 手动模式下 AutoSwitch 不应生效
	m := NewManager("S0_Champion")
	switched := m.AutoSwitch("S1_MinCost", 10, time.Now())
	if switched {
		t.Fatal("手动模式下不应自动切换")
	}
	if m.Champion() != "S0_Champion" {
		t.Fatalf("手动模式下冠军不应变，实际 %s", m.Champion())
	}
}

func TestAutoSwitchSuccess(t *testing.T) {
	// v5 统计严谨评估：自动模式 + 有优势 + 过冷却 + 连续领先 3 次 → 自动切换 + 留痕
	m := NewManager("S0_Champion")
	m.ConfirmAuto()
	if err := m.SetMode(ModeAuto); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 前 2 次领先：未达连续门槛，不切换
	for i := 0; i < 2; i++ {
		if m.AutoSwitch("S1_MinCost", 12.5, now) {
			t.Fatalf("第 %d 次领先不应切换（需连续 %d 次）", i+1, m.streakNeed)
		}
	}
	// 第 3 次连续领先：达到门槛 → 切换成功
	if !m.AutoSwitch("S1_MinCost", 12.5, now) {
		t.Fatal("连续领先 3 次应切换成功")
	}
	if m.Champion() != "S1_MinCost" {
		t.Fatalf("冠军应变为 S1，实际 %s", m.Champion())
	}
	h := m.History()
	if len(h) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(h))
	}
	if h[0].Triggered != "system" || !strings.Contains(h[0].Reason, "12.5") {
		t.Fatalf("历史应记录 system 触发 + 优势分，实际 %+v", h[0])
	}
}

func TestAutoSwitchNoAdvantage(t *testing.T) {
	// 无优势（win<=0）不切换，且清零连续领先计数
	m := NewManager("S0_Champion")
	m.ConfirmAuto()
	_ = m.SetMode(ModeAuto)
	now := time.Now()
	// 先积累 2 次领先
	for i := 0; i < 2; i++ {
		m.AutoSwitch("S1_MinCost", 10, now)
	}
	// 一次无优势 → 清零
	if m.AutoSwitch("S1_MinCost", -3, now) {
		t.Fatal("无优势不应切换")
	}
	if m.Champion() != "S0_Champion" {
		t.Fatalf("冠军不应变，实际 %s", m.Champion())
	}
	// 清零后需重新积累 3 次：再领先 2 次仍不切换
	for i := 0; i < 2; i++ {
		if m.AutoSwitch("S1_MinCost", 10, now) {
			t.Fatalf("清零后第 %d 次领先不应切换", i+1)
		}
	}
	if m.Champion() != "S0_Champion" {
		t.Fatalf("清零后冠军仍不应变，实际 %s", m.Champion())
	}
}

func TestAutoSwitchCooldown(t *testing.T) {
	// 冷却期内不反复切换（v5：还需连续领先 3 次）
	now := time.Now()
	m := NewManager("S0_Champion")
	m.ConfirmAuto()
	_ = m.SetMode(ModeAuto)
	// 连续领先 3 次 → 第一次切换成功
	for i := 0; i < 3; i++ {
		m.AutoSwitch("S1_MinCost", 10, now)
	}
	if m.Champion() != "S1_MinCost" {
		t.Fatalf("第一次切换应成功，实际冠军 %s", m.Champion())
	}
	// 冷却期内（+10s < 30s）：切回 S0 应被冷却拦截
	if m.AutoSwitch("S0_Champion", 15, now.Add(10*time.Second)) {
		t.Fatal("冷却期内不应再次切换")
	}
	if m.Champion() != "S1_MinCost" {
		t.Fatalf("冷却期内冠军不应变，实际 %s", m.Champion())
	}
	// 冷却期后（+40s > 30s）：需重新连续领先 3 次才可切换
	for i := 0; i < 2; i++ {
		if m.AutoSwitch("S0_Champion", 15, now.Add(40*time.Second)) {
			t.Fatalf("冷却期后第 %d 次领先不应切换（需连续 3 次）", i+1)
		}
	}
	if !m.AutoSwitch("S0_Champion", 15, now.Add(40*time.Second)) {
		t.Fatal("冷却期后连续领先 3 次应可切换")
	}
	if m.Champion() != "S0_Champion" {
		t.Fatalf("冷却期后冠军应变回 S0，实际 %s", m.Champion())
	}
}

func TestManualSwitchAlwaysAllowed(t *testing.T) {
	// 手动切换始终允许 + 留痕
	m := NewManager("S0_Champion")
	m.ManualSwitch("S1_MinCost", "admin")
	if m.Champion() != "S1_MinCost" {
		t.Fatalf("手动切换后冠军应变为 S1，实际 %s", m.Champion())
	}
	h := m.History()
	if len(h) != 1 || h[0].Triggered != "admin" {
		t.Fatalf("手动切换应留 admin 记录，实际 %+v", h)
	}
}

func TestFallbackRecords(t *testing.T) {
	// 回退留痕（P2-5 回滚保障）
	m := NewManager("S1_MinCost")
	m.Fallback("S0_Champion")
	if m.Champion() != "S0_Champion" {
		t.Fatalf("回退后冠军应为 S0，实际 %s", m.Champion())
	}
	h := m.History()
	if len(h) != 1 || !strings.Contains(h[0].Reason, "fallback") {
		t.Fatalf("回退应留 fallback 记录，实际 %+v", h)
	}
}

// ---------------------------------------------------------------------------
// v6 新增：审计落盘钩子 / 操作人 / 参数变更留痕
// ---------------------------------------------------------------------------

// TestSinkCalledOnRecord 每条历史都会同步给 sink（落盘钩子）
func TestSinkCalledOnRecord(t *testing.T) {
	m := NewManager("S0_Champion")
	var got []Entry
	m.SetSink(func(e Entry) { got = append(got, e) })

	m.ManualSwitch("S1_MinCost", "zhangsan")
	m.Fallback("S0_Champion")
	m.LogParamChange("zhangsan", "weights.S3", "0.4/0.3/0.3", "0.5/0.2/0.3")

	if len(got) != 3 {
		t.Fatalf("sink 应收到 3 条，实际 %d", len(got))
	}
	if got[0].Operator != "zhangsan" || got[0].Triggered != "admin" {
		t.Errorf("手动切换应记录操作人 zhangsan: %+v", got[0])
	}
	if got[1].Operator != "system" {
		t.Errorf("自动回退操作人应为 system: %+v", got[1])
	}
	if !strings.Contains(got[2].Reason, "param change: weights.S3") {
		t.Errorf("参数变更应进历史: %+v", got[2])
	}
	if len(m.History()) != 3 {
		t.Errorf("内存历史也应累计 3 条，实际 %d", len(m.History()))
	}
}

// TestLogParamChangeKeepsChampion 参数变更不改冠军
func TestLogParamChangeKeepsChampion(t *testing.T) {
	m := NewManager("S0_Champion")
	m.LogParamChange("lisi", "traffic.S1", "10", "30")
	if m.Champion() != "S0_Champion" {
		t.Errorf("参数变更不应改冠军，实际 %s", m.Champion())
	}
	h := m.History()
	if len(h) != 1 || h[0].From != "S0_Champion" || h[0].To != "S0_Champion" {
		t.Errorf("参数变更历史 From==To==冠军: %+v", h)
	}
	if h[0].Operator != "lisi" {
		t.Errorf("应记录操作人: %+v", h[0])
	}
}

// TestManualSwitchOperatorDefault 未传操作人时默认 admin
func TestManualSwitchOperatorDefault(t *testing.T) {
	m := NewManager("S0_Champion")
	m.ManualSwitch("S1_MinCost", "")
	if m.History()[0].Operator != "admin" {
		t.Errorf("默认操作人应为 admin，实际 %s", m.History()[0].Operator)
	}
}

// TestLoadHistoryRestore 持久化恢复的历史可展示且不干扰状态机
func TestLoadHistoryRestore(t *testing.T) {
	m := NewManager("S0_Champion")
	m.LoadHistory([]Entry{
		{From: "S0_Champion", To: "S1_MinCost", Reason: "manual confirm", Triggered: "admin", Operator: "admin"},
	})
	if len(m.History()) != 1 {
		t.Fatalf("恢复历史应可见: %d", len(m.History()))
	}
	// 恢复的历史不应影响模式/冠军
	if m.Mode() != ModeManual || m.Champion() != "S0_Champion" {
		t.Errorf("恢复历史不应改变运行态: mode=%s champion=%s", m.Mode(), m.Champion())
	}
	if !strings.Contains(m.HistoryString(), "S1_MinCost") {
		t.Errorf("HistoryString 应包含恢复的历史: %s", m.HistoryString())
	}
}
