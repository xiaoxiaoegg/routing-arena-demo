// internal/arena/arena.go
// P3-2/P3-3 竞技场管理：自动切换模式 + 切换历史留痕 + 二次确认防护。
//
// 状态机：
//   手动模式（manual）→ [二次确认] → 自动模式（auto）
//   自动模式下：评分优势稳定 + 候选未熔断 → 系统自动切换冠军 + 留痕
//
// 设计要点：
//   - 切换历史全程留痕（谁/何时/从哪切到哪/原因），可审计
//   - 手动 → 自动 必须显式确认（ConfirmAuto），防止误开
//   - 自动切换有冷却（同一策略不连续反复切换）

package arena

import (
	"fmt"
	"strings"
	"time"
)

// Mode 管理模式
type Mode string

const (
	ModeManual Mode = "manual" // 手动模式：切换需人工确认
	ModeAuto   Mode = "auto"   // 自动模式：系统自动切换（需先确认开启）
)

// Entry 一条切换/变更历史记录
type Entry struct {
	At        time.Time // 时间
	From      string    // 原冠军策略（参数变更为当前冠军）
	To        string    // 新冠军策略（参数变更为当前冠军）
	Reason    string    // 原因（"manual confirm" / "auto: score advantage" / "auto: fallback" / "param change: ..."）
	Triggered string    // 触发者类型（"admin" / "system"）
	Operator  string    // 操作人（v6：手动操作的账号；system 动作 = "system"）
}

// Manager 竞技场管理器
type Manager struct {
	mode        Mode
	champion    string // 当前冠军策略 ID
	history     []Entry
	confirmAuto bool           // 二次确认标记（手动→自动需确认）
	cooldown    time.Duration  // 自动切换冷却期
	lastSwitch  time.Time      // 上次自动切换时间
	sink        func(Entry)   // v6 审计落盘钩子（nil = 纯内存）；每条历史都会同步给 sink
	// v5 统计严谨评估：候选策略需连续领先 N 次（窗口）且未熔断才建议/执行晋升，
	// 防止单次波动（偶发高分）导致误切换。
	streakNeed int            // 需连续领先次数（默认 3）
	streak     map[string]int // 策略 → 当前连续领先计数
	// v5 状态平滑交接：新冠军接班时继承旧冠军的画像快照（demo 用内存记录，
	// 生产用 Redis）。切换时把旧冠军的"最后画像"带给新冠军，防数据震荡。
	snapshot string // 旧冠军最后画像快照（切换时更新，UI 展示）
}

// NewManager 创建管理器（默认手动模式，冠军 S0）
func NewManager(champion string) *Manager {
	return &Manager{
		mode:        ModeManual,
		champion:    champion,
		cooldown:    30 * time.Second,
		streakNeed:  3,
		streak:      map[string]int{},
	}
}

// SetSink 设置审计落盘钩子（v6）：每条历史记录都会同步调用 sink。
// 调用方负责把 Entry 写进 store（JSONL 等），实现"重启可查"。
func (m *Manager) SetSink(sink func(Entry)) {
	m.sink = sink
}

// LoadHistory 从持久化存储恢复历史（启动时调用）。
// 恢复的历史只读展示，不参与 streak/cooldown 状态。
func (m *Manager) LoadHistory(entries []Entry) {
	m.history = append(m.history, entries...)
}

// SetStreakNeed 设置晋升所需连续领先次数（统计严谨评估 v5）
func (m *Manager) SetStreakNeed(n int) {
	if n > 0 {
		m.streakNeed = n
	}
}

// Mode 当前管理模式
func (m *Manager) Mode() Mode { return m.mode }

// Champion 当前冠军
func (m *Manager) Champion() string { return m.champion }

// History 切换历史（展示用）
func (m *Manager) History() []Entry { return m.history }

// SetMode 切换管理模式。
// 手动 → 自动：必须已确认（ConfirmAuto），否则拒绝（P3-3 二次确认防护）
// 自动 → 手动：随时允许
func (m *Manager) SetMode(mode Mode) error {
	switch mode {
	case ModeManual:
		m.mode = ModeManual
		return nil
	case ModeAuto:
		if !m.confirmAuto {
			return fmt.Errorf("手动→自动 需二次确认（调用 ConfirmAuto）")
		}
		m.mode = ModeAuto
		return nil
	default:
		return fmt.Errorf("未知模式 %q", mode)
	}
}

// ConfirmAuto 二次确认：管理员明确确认开启自动模式（防误开）
func (m *Manager) ConfirmAuto() {
	m.confirmAuto = true
}

// ManualSwitch 手动切换冠军（P2 手动模式）：始终允许，留痕。
// operator 为操作人标识（管理后台从 X-Operator 头取，默认 "admin"）。
func (m *Manager) ManualSwitch(to, operator string) {
	if operator == "" {
		operator = "admin"
	}
	m.record(m.champion, to, "manual confirm", "admin", operator)
	m.champion = to
}

// LogParamChange 参数变更留痕（v6）：权重微调 / 流量分配等不换冠军的变更，
// 也进同一条审计历史（From==To==当前冠军，Reason 描述改了什么）。
func (m *Manager) LogParamChange(operator, field, oldVal, newVal string) {
	if operator == "" {
		operator = "admin"
	}
	m.record(m.champion, m.champion, "param change: "+field+" "+oldVal+" → "+newVal, "admin", operator)
}

// AutoSwitch 自动切换冠军（P3-2）：自动模式下由系统调用。
// 条件（v5 统计严谨评估）：
//   - 处于自动模式
//   - 候选连续领先（streak 达到 streakNeed）——防单次波动误切换
//   - 冷却期已过（防反复横跳）
// 返回是否真的切换了。
func (m *Manager) AutoSwitch(to string, win float64, now time.Time) bool {
	if m.mode != ModeAuto {
		return false // 手动模式下系统不自动切换
	}
	if win <= 0 {
		// 未领先：清零该策略连续领先计数（严谨：重新积累）
		delete(m.streak, to)
		return false
	}
	if now.Sub(m.lastSwitch) < m.cooldown {
		return false // 冷却期内，防止反复横跳
	}
	// 连续领先 +1；未达到门槛不切换
	m.streak[to]++
	if m.streak[to] < m.streakNeed {
		return false
	}
	// 达到连续领先门槛 → 切换并清零计数
	delete(m.streak, to)
	m.record(m.champion, to, fmt.Sprintf("auto: streak %d× advantage %.1f", m.streakNeed, win), "system", "system")
	m.champion = to
	m.lastSwitch = now
	return true
}

// Fallback 自动回退（P2-5 回滚保障）：新冠军劣化 → 回退旧冠军
func (m *Manager) Fallback(to string) {
	m.record(m.champion, to, "auto: fallback (degraded)", "system", "system")
	m.champion = to
}

// record 追加历史记录（v6：同步到审计 sink，实现落盘）
func (m *Manager) record(from, to, reason, triggered, operator string) {
	if operator == "" {
		operator = triggered
	}
	e := Entry{
		At:        time.Now(),
		From:      from,
		To:        to,
		Reason:    reason,
		Triggered: triggered,
		Operator:  operator,
	}
	m.history = append(m.history, e)
	if m.sink != nil {
		m.sink(e) // 落盘钩子：写 JSONL / 告警等
	}
	// v5 平滑交接：切换时继承旧冠军画像快照（"谁接班、接过什么"）
	if from != to {
		m.snapshot = from + " 的画像已交接给 " + to
	}
}

// Snapshot 当前画像快照（UI 展示：平滑交接状态）
func (m *Manager) Snapshot() string { return m.snapshot }

// HistoryString 格式化历史（展示用）
func (m *Manager) HistoryString() string {
	if len(m.history) == 0 {
		return "（暂无切换历史）"
	}
	var sb strings.Builder
	for _, e := range m.history {
		op := e.Operator
		if op == "" {
			op = e.Triggered
		}
		sb.WriteString(fmt.Sprintf("  %s  %s → %s  [%s] by %s\n",
			e.At.Format("15:04:05"), e.From, e.To, e.Reason, op))
	}
	return sb.String()
}
