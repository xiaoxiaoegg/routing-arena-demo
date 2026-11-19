// cmd/admin/main.go
// 路由策略池竞技场 · 管理后台（参考 api-pool 模式：单服务 + JSON API + Tab 面板）
//
// 功能：
//   - 渠道健康状态总览（P3-1 健康探测）
//   - 策略竞技场看板（P2 评分 + 晋升建议）
//   - 冠军切换（手动 / 自动模式 + 二次确认 P3-3）
//   - 故障注入演示（模拟渠道故障 → 熔断 → 回退）
//   - 切换历史留痕（全程可审计）
//
// 跑法：go run ./cmd/admin   → 打开 http://localhost:8080

package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"routing-arena-demo/internal/arena"
	"routing-arena-demo/internal/eval"
	"routing-arena-demo/internal/health"
	"routing-arena-demo/internal/logreader"
	"routing-arena-demo/internal/metrics"
	"routing-arena-demo/internal/replay"
	"routing-arena-demo/internal/sim"
	"routing-arena-demo/internal/stats"
	"routing-arena-demo/internal/store"
	"routing-arena-demo/internal/strategy"
)

//go:embed web/admin.html
var webFS embed.FS

// App 应用状态（管理后台持有全局单例）
type App struct {
	kb       *stats.Knowledge
	recs     []logreader.LogRecord
	eng      *strategy.RoutingEngine
	prober   *health.Prober
	mgr      *arena.Manager
	faulted  map[int]bool             // 模拟故障中的渠道
	traffic  *strategy.TrafficSplitter // 流量分配器（多策略按比例分桶）
	s3Weight strategy.HybridWeights   // S3 权重（可微调）
	s4Weight strategy.HybridWeights   // S4 权重（可微调）
	watermark *strategy.Watermark     // v5 并发水位拦截器

	// v6 工程化：鉴权 / 审计落盘 / 告警
	token       string  // ADMIN_TOKEN（空 = 开发模式不鉴权）
	dataDir     string  // 持久化目录（空 = 纯内存）
	historyPath string  // 切换历史 JSONL
	alertPath   string  // 告警 JSONL
	alerts      []Alert // 内存告警环形缓冲（最近 20 条）

	latHist *metrics.Histogram // v6 请求延迟直方图（/api/metrics，Exemplar 挂 trace_id）
}

// Alert 一条告警
type Alert struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"` // "warn" / "info" / "error"
	Msg   string    `json:"msg"`
}

func main() {
	app := mustNewApp()

	mux := http.NewServeMux()
	// 静态页面
	mux.HandleFunc("/", app.handleIndex)
	// 只读 API：无需鉴权
	mux.HandleFunc("/api/state", app.handleState)
	mux.HandleFunc("/api/history", app.handleHistory)
	mux.HandleFunc("/api/scores", app.handleScores)
	mux.HandleFunc("/api/alerts", app.handleAlerts)
	mux.HandleFunc("/api/metrics", app.handleMetrics)
	mux.HandleFunc("/api/traffic", app.handleTraffic) // 内部按方法区分：GET 只读不鉴权，POST 变更需鉴权
	// 变更 API：需要鉴权（ADMIN_TOKEN）+ 记录操作人
	auth := app.requireAuth
	mux.HandleFunc("/api/probe", auth(app.handleProbe))
	mux.HandleFunc("/api/switch", auth(app.handleSwitch))
	mux.HandleFunc("/api/mode", auth(app.handleMode))
	mux.HandleFunc("/api/fault", auth(app.handleFault))
	mux.HandleFunc("/api/weights", auth(app.handleWeights))
	mux.HandleFunc("/api/watermark", auth(app.handleWatermark))

	addr := ":" + getenvDefault("ADMIN_PORT", "8080")
	if app.token == "" {
		fmt.Printf("⚠ 未设置 ADMIN_TOKEN，管理后台为开发模式（不做鉴权）——生产务必设置\n")
	}
	fmt.Printf("管理后台已启动: http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// getenvDefault 读环境变量，为空时用默认值
func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newApp 初始化应用状态：读/生成日志 → 训练集建库 → 建引擎 → 健康探测 → 竞技场管理
// src 统一取数结果（CSV 或模拟生成）；知识库只用训练集（前 70%），评估用全量。
// dataDir 非空时启用审计落盘：切换历史 / 告警写 JSONL，重启可查。
func newApp(src sim.Source, dataDir string) (*App, error) {
	train, _ := stats.Split(src.Records, 0.7)
	kb := stats.Aggregate(train)

	app := &App{
		recs:        src.Records,
		kb:          kb,
		eng:         strategy.NewEngine(),
		faulted:     make(map[int]bool),
		token:       os.Getenv("ADMIN_TOKEN"), // 空 = 开发模式不鉴权
		dataDir:     dataDir,
		historyPath: filepath.Join(dataDir, "history.jsonl"),
		alertPath:   filepath.Join(dataDir, "alerts.jsonl"),
	}
	if dataDir == "" {
		app.historyPath, app.alertPath = "", ""
	}
	app.eng.WithBreaker(strategy.BreakerConfig{
		// v6 告警钩子：策略熔断 → 写告警
		OnTrip: func(id, reason string) {
			app.alert("error", fmt.Sprintf("熔断触发: 策略 %s（%s）", id, reason))
		},
	})
	// 健康探测（默认全部健康，故障由 /api/fault 注入）；渠道列表从数据里取
	chIDs := uniqueChannels(src.Records)
	app.prober = health.NewProber(chIDs, time.Minute, app.probeFunc)
	app.prober.ProbeOnce()
	app.eng.WithHealthProber(app.prober)
	// v6 并发水位拦截：所有渠道配额 100（演示水位踢出）
	quota := map[int]int{}
	for _, id := range chIDs {
		quota[id] = 100
	}
	app.watermark = strategy.NewWatermark(quota)
	app.eng.WithWatermark(app.watermark)
	// 竞技场管理器（默认手动模式，冠军 S0）
	app.mgr = arena.NewManager("S0_Champion")
	// v6 审计落盘：每条历史同步写 JSONL
	if app.historyPath != "" {
		app.mgr.SetSink(func(e arena.Entry) {
			_ = store.Append(app.historyPath, e)
		})
		// 启动时恢复历史（重启可查）
		var entries []arena.Entry
		found, _, _, err := store.Load(app.historyPath, func(b []byte) error {
			var e arena.Entry
			if err := json.Unmarshal(b, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("恢复历史失败: %w", err)
		}
		if found && len(entries) > 0 {
			app.mgr.LoadHistory(entries)
			fmt.Printf("已恢复 %d 条切换历史（%s）\n", len(entries), app.historyPath)
		}
	}
	// 流量分配器：默认 S0 80% / S1 10% / S3 10%（管理员可在后台改）
	app.traffic = strategy.NewTrafficSplitter(map[string]float64{
		"S0_Champion": 80, "S1_MinCost": 10, "S3_Hybrid": 10,
	}).WithSeed(42)
	// 综合策略权重（可微调）
	app.s3Weight = strategy.DefaultHybridWeights()
	app.s4Weight = strategy.RobustWeights()
	// v6 延迟直方图（/api/metrics）
	app.latHist = metrics.NewHistogram("req_latency_ms", "请求延迟分布（Exemplar 挂 trace_id 可下钻）",
		[]float64{100, 300, 800, 1500, 3000, 6000}, nil)
	return app, nil
}

// uniqueChannels 取日志里出现的所有渠道 ID（去重、升序）
func uniqueChannels(recs []logreader.LogRecord) []int {
	seen := map[int]bool{}
	var out []int
	for _, r := range recs {
		if !seen[r.ChannelID] {
			seen[r.ChannelID] = true
			out = append(out, r.ChannelID)
		}
	}
	sort.Ints(out)
	return out
}

// mustNewApp 便捷包装：失败直接退出（main 用）
func mustNewApp() *App {
	csvPath := flag.String("csv", "", "消费日志 CSV 路径（空 = 用模拟数据）")
	records := flag.Int("records", 2000, "模拟数据条数")
	seed := flag.Int64("seed", 42, "随机种子")
	dataDir := flag.String("data", "", "审计落盘目录（空 = 纯内存；切换历史/告警写 JSONL）")
	flag.Parse()

	cfg := sim.DefaultConfig()
	cfg.Records = *records
	cfg.Seed = *seed
	src, err := sim.LoadOrGenerate(*csvPath, cfg)
	if err != nil {
		log.Fatalf("取数失败: %v", err)
	}
	fmt.Printf("数据: %s | %d 条（知识库=前70%%训练集，样本外口径）\n", src.Describe(), len(src.Records))
	if *dataDir != "" {
		if err := os.MkdirAll(*dataDir, 0o755); err != nil {
			log.Fatalf("创建数据目录失败: %v", err)
		}
		fmt.Printf("审计落盘: %s（切换历史/告警重启可查）\n", *dataDir)
	}
	app, err := newApp(src, *dataDir)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	return app
}

// probeFunc 健康探测实现：真实场景调上游 /models 免费接口；
// 这里按故障注入状态模拟（零成本，不产生 token 计费）
func (a *App) probeFunc(id int) health.State {
	if a.faulted[id] {
		return health.State{ChannelID: id, Up: false, LatencyMs: 0, ErrorRate: 1.0}
	}
	switch id {
	case 1:
		return health.State{ChannelID: id, Up: true, LatencyMs: 3000, ErrorRate: 0.005}
	case 2:
		return health.State{ChannelID: id, Up: true, LatencyMs: 1200, ErrorRate: 0.01}
	default:
		return health.State{ChannelID: id, Up: true, LatencyMs: 900, ErrorRate: 0.003}
	}
}

// ---- 页面 ----

// handleIndex 返回管理面板页面（embed 内嵌 HTML）
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(webFS, "web/admin.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ---- JSON API ----

// writeJSON 统一 JSON 响应
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// ---- v6 鉴权 / 操作人 / 告警 / 方法守卫 ----

// requireAuth 鉴权中间件：ADMIN_TOKEN 为空 = 开发模式放行；
// 非空则要求 Authorization: Bearer <token>，否则 401。
func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized) // 先写状态码，再写 body
			writeJSON(w, map[string]any{"ok": false, "error": "未授权（需要 Bearer ADMIN_TOKEN）"})
			return
		}
		next(w, r)
	}
}

// authorized 是否通过鉴权：未设 token = 开发模式恒真；否则校验 Bearer
func (a *App) authorized(r *http.Request) bool {
	if a.token == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	return strings.HasPrefix(got, "Bearer ") && strings.TrimPrefix(got, "Bearer ") == a.token
}

// operatorOf 当前操作人：X-Operator 头优先，否则默认 admin
func (a *App) operatorOf(r *http.Request) string {
	if op := r.Header.Get("X-Operator"); op != "" {
		return op
	}
	return "admin"
}

// alert 记录一条告警：内存环形缓冲（最近 20 条）+ 落盘 alerts.jsonl（best-effort）
func (a *App) alert(level, msg string) {
	al := Alert{At: time.Now(), Level: level, Msg: msg}
	a.alerts = append(a.alerts, al)
	if len(a.alerts) > 20 {
		a.alerts = a.alerts[len(a.alerts)-20:]
	}
	if a.alertPath != "" {
		_ = store.Append(a.alertPath, al)
	}
	fmt.Printf("[alert:%s] %s\n", level, msg)
}

// handleAlerts GET /api/alerts — 最近告警
func (a *App) handleAlerts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"alerts": a.alerts})
}

// handleMetrics GET /api/metrics — Prometheus 文本格式指标（只读，不鉴权）
func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	a.latHist.WriteText(w)
}

// stateResp 状态响应
type stateResp struct {
	Channels []health.State `json:"channels"`
	Healthy  []int          `json:"healthy"`
	Mode     arena.Mode     `json:"mode"`
	Champion string         `json:"champion"`
	Breakers map[string]bool `json:"breakers"` // 策略 → 是否熔断
	BreakerStates map[string]string `json:"breakerStates"` // v5 三态：closed/open/half_open
	Watermark map[int]int   `json:"watermark"` // v5 并发水位：渠道 → 活跃并发
	Snapshot string         `json:"snapshot"`  // v5 平滑交接：画像快照
	Probes   int            `json:"probes"`    // 探测次数
}

// handleState GET /api/state — 整体状态
func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	br := a.eng.Breaker()
	writeJSON(w, stateResp{
		Channels: a.prober.All(),
		Healthy:  a.prober.HealthyChannels(),
		Mode:     a.mgr.Mode(),
		Champion: a.mgr.Champion(),
		Breakers: map[string]bool{
			"S1_MinCost": br.IsTripped("S1_MinCost"),
			"S2_MinLat":  br.IsTripped("S2_MinLat"),
			"S3_Hybrid":  br.IsTripped("S3_Hybrid"),
		},
		BreakerStates: map[string]string{
			"S1_MinCost": br.StateName("S1_MinCost"),
			"S2_MinLat":  br.StateName("S2_MinLat"),
			"S3_Hybrid":  br.StateName("S3_Hybrid"),
		},
		Watermark: a.watermark.Active(),
		Snapshot:  a.mgr.Snapshot(),
		Probes:    len(a.prober.All()),
	})
}

// handleProbe POST /api/probe — 触发一轮健康探测
func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	a.prober.ProbeOnce()
	writeJSON(w, map[string]any{"ok": true, "channels": a.prober.All()})
}

// handleSwitch POST /api/switch — 手动切换冠军（P2 手动模式）
// v6：必须显式 confirm=true（二次确认，防手滑），记录操作人，留痕 + 告警。
func (a *App) handleSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To      string `json:"to"`
		Confirm bool   `json:"confirm"` // 二次确认：必须 true
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.To == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "缺少目标策略"})
		return
	}
	if !req.Confirm {
		writeJSON(w, map[string]any{"ok": false, "error": "切换冠军需要二次确认（confirm=true）"})
		return
	}
	if err := a.eng.SetDefault(req.To); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	op := a.operatorOf(r)
	a.mgr.ManualSwitch(req.To, op)
	a.alert("warn", fmt.Sprintf("手动切换冠军: %s → %s（操作人 %s）", a.mgr.History()[0].From, req.To, op))
	writeJSON(w, map[string]any{"ok": true, "champion": a.mgr.Champion()})
}

// handleMode POST /api/mode — 切换管理模式（auto 需二次确认，P3-3）
func (a *App) handleMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode   arena.Mode `json:"mode"`
		Confirm bool      `json:"confirm"` // 二次确认标记
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "参数错误"})
		return
	}
	if req.Mode == arena.ModeAuto && req.Confirm {
		a.mgr.ConfirmAuto()
	}
	if err := a.mgr.SetMode(req.Mode); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mode": a.mgr.Mode()})
}

// handleFault POST /api/fault — 故障注入/恢复（模拟渠道故障 → 熔断 → 回退）
func (a *App) handleFault(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel int  `json:"channel"`
		Down    bool `json:"down"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "参数错误"})
		return
	}
	if req.Down {
		a.faulted[req.Channel] = true
		// 同时向熔断器上报故障样本（触发策略级熔断）
		a.eng.Breaker().Report("S1_MinCost", false, 9*time.Second)
		a.eng.Breaker().Report("S2_MinLat", false, 9*time.Second)
		a.alert("error", fmt.Sprintf("故障注入: 渠道 %d 下线（操作人 %s）", req.Channel, a.operatorOf(r)))
	} else {
		delete(a.faulted, req.Channel)
		a.alert("info", fmt.Sprintf("故障恢复: 渠道 %d 恢复（操作人 %s）", req.Channel, a.operatorOf(r)))
	}
	a.prober.ProbeOnce()
	writeJSON(w, map[string]any{"ok": true, "channels": a.prober.All()})
}

// handleHistory GET /api/history — 切换历史
func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	h := a.mgr.History()
	if h == nil {
		h = []arena.Entry{} // 空历史返回 [] 而非 null（客户端好处理）
	}
	writeJSON(w, map[string]any{"history": h})
}

// handleScores GET /api/scores — 评估评分 + 晋升建议
// 用流量分配器跑评估（各策略按比例承接流量），而非固定单候选分桶
func (a *App) handleScores(w http.ResponseWriter, r *http.Request) {
	records, _ := replay.RunEngine(a.recs, a.kb, a.eng, a.traffic, nil)
	// v6：回放结果同步喂延迟直方图（/api/metrics 可查，Exemplar 挂 trace_id）
	for i, rec := range records {
		a.latHist.Observe(float64(rec.LatencyMs), map[string]string{
			"trace_id":    fmt.Sprintf("trace-%05d", i),
			"strategy_id": rec.StrategyID,
		})
	}
	agg := eval.NewAggregator()
	scores := agg.Aggregate(records)
	// v5 严格归因：统计重试记录数（选错渠道的代价）
	retried := 0
	for _, rec := range records {
		if rec.Retried {
			retried++
		}
	}
	writeJSON(w, map[string]any{
		"records": len(records),
		"scores":  scores,
		"retried": retried,
		"recommend": eval.Recommend(scores),
	})
}

// trafficResp 流量分配响应
type trafficResp struct {
	Weights map[string]float64 `json:"weights"` // 策略 → 比例%
	Total   float64            `json:"total"`
}

// handleTraffic GET/POST /api/traffic — 流量分配器
//   GET: 返回当前各策略流量比例（只读，不鉴权）
//   POST: 设置新比例（变更，需鉴权；总和需 100%，保存即热加载）
func (a *App) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, trafficResp{Weights: a.traffic.Weights(), Total: a.traffic.Total()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"ok": false, "error": "未授权（需要 Bearer ADMIN_TOKEN）"})
		return
	}
	var req struct {
		Weights map[string]float64 `json:"weights"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "参数错误"})
		return
	}
	if err := strategy.Validate(req.Weights); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	oldW := a.traffic.Weights()
	a.traffic.SetWeights(req.Weights)
	// v6 参数变更留痕：流量分配比例改动进审计历史
	a.mgr.LogParamChange(a.operatorOf(r), "traffic", fmt.Sprint(oldW), fmt.Sprint(a.traffic.Weights()))
	a.alert("info", fmt.Sprintf("流量分配调整: %v → %v（操作人 %s）", oldW, a.traffic.Weights(), a.operatorOf(r)))
	writeJSON(w, map[string]any{"ok": true, "weights": a.traffic.Weights()})
}

// handleWatermark POST /api/watermark — v5 并发水位模拟（演示并发打满 → 踢出候选池）
func (a *App) handleWatermark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Channel int `json:"channel"`
		Active  int `json:"active"` // 目标活跃并发数（0 = 释放）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "参数错误"})
		return
	}
	cur := a.watermark.Active()[req.Channel]
	// 差量调整到目标值（Acquire 只能 +1，用循环补差值）
	for cur < req.Active {
		a.watermark.Acquire(req.Channel)
		cur++
	}
	for cur > req.Active {
		a.watermark.Release(req.Channel)
		cur--
	}
	writeJSON(w, map[string]any{"ok": true, "watermark": a.watermark.Active()})
}

// handleWeights POST /api/weights — 权重微调器（S3/S4 综合策略评分权重）
func (a *App) handleWeights(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Strategy string  `json:"strategy"` // "S3_Hybrid" / "S4_Robust"
		Cost     float64 `json:"cost"`
		Latency  float64 `json:"latency"`
		Health   float64 `json:"health"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "参数错误"})
		return
	}
	w_ := strategy.HybridWeights{Cost: req.Cost, Latency: req.Latency, Health: req.Health}
	// 权重归一化：允许管理员填任意值，保存时归一化到总和 1
	total := w_.Cost + w_.Latency + w_.Health
	if total <= 0 {
		writeJSON(w, map[string]any{"ok": false, "error": "权重总和必须 > 0"})
		return
	}
	w_ = strategy.HybridWeights{Cost: w_.Cost / total, Latency: w_.Latency / total, Health: w_.Health / total}
	switch req.Strategy {
	case "S3_Hybrid":
		old := a.s3Weight
		a.s3Weight = w_
		a.eng.SetHybridWeights(w_)
		a.mgr.LogParamChange(a.operatorOf(r), "weights."+req.Strategy,
			fmt.Sprintf("%.1f/%.1f/%.1f", old.Cost, old.Latency, old.Health),
			fmt.Sprintf("%.1f/%.1f/%.1f", w_.Cost, w_.Latency, w_.Health))
		a.alert("info", fmt.Sprintf("权重微调 %s: %s（操作人 %s）", req.Strategy, fmt.Sprintf("%.1f/%.1f/%.1f", w_.Cost, w_.Latency, w_.Health), a.operatorOf(r)))
	case "S4_Robust":
		old := a.s4Weight
		a.s4Weight = w_
		a.eng.SetRobustWeights(w_)
		a.mgr.LogParamChange(a.operatorOf(r), "weights."+req.Strategy,
			fmt.Sprintf("%.1f/%.1f/%.1f", old.Cost, old.Latency, old.Health),
			fmt.Sprintf("%.1f/%.1f/%.1f", w_.Cost, w_.Latency, w_.Health))
		a.alert("info", fmt.Sprintf("权重微调 %s: %s（操作人 %s）", req.Strategy, fmt.Sprintf("%.1f/%.1f/%.1f", w_.Cost, w_.Latency, w_.Health), a.operatorOf(r)))
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "未知策略 " + req.Strategy})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "strategy": req.Strategy, "weights": w_})
}
