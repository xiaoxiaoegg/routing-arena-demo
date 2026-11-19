// cmd/admin/admin_test.go
// 管理后台 API 单测：用 httptest 直接测各 handler（无需真实端口）。
// 覆盖正常分支 + 错误分支（非法比例/未知策略/二次确认防护）。

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"routing-arena-demo/internal/sim"
)

// newTestApp 构造测试用 App（读 testdata CSV，纯内存不落盘）
func newTestApp(t *testing.T) *App {
	t.Helper()
	src, err := sim.LoadOrGenerate("../../testdata/sample_logs.csv", sim.DefaultConfig())
	if err != nil {
		t.Fatalf("取数失败: %v", err)
	}
	app, err := newApp(src, "")
	if err != nil {
		t.Fatalf("newApp 失败: %v", err)
	}
	return app
}

// newTestAppWithData 构造带落盘目录的 App（测试持久化用）
func newTestAppWithData(t *testing.T, dataDir string) *App {
	t.Helper()
	src, err := sim.LoadOrGenerate("../../testdata/sample_logs.csv", sim.DefaultConfig())
	if err != nil {
		t.Fatalf("取数失败: %v", err)
	}
	app, err := newApp(src, dataDir)
	if err != nil {
		t.Fatalf("newApp 失败: %v", err)
	}
	return app
}

// do 执行一次 HTTP 请求并返回 recorder
func do(app *App, method, path, body string) *httptest.ResponseRecorder {
	return doWithHeader(app, method, path, body, nil)
}

// doWithHeader 带自定义头的请求（测试操作人 / 鉴权头用）
func doWithHeader(app *App, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	// 手动路由（与 main 一致）
	switch path {
	case "/api/state":
		app.handleState(w, req)
	case "/api/switch":
		app.handleSwitch(w, req)
	case "/api/mode":
		app.handleMode(w, req)
	case "/api/fault":
		app.handleFault(w, req)
	case "/api/history":
		app.handleHistory(w, req)
	case "/api/scores":
		app.handleScores(w, req)
	case "/api/traffic":
		app.handleTraffic(w, req)
	case "/api/weights":
		app.handleWeights(w, req)
	case "/api/alerts":
		app.handleAlerts(w, req)
	case "/api/metrics":
		app.handleMetrics(w, req)
	default:
		app.handleIndex(w, req)
	}
	return w
}

// parseJSON 解析响应 JSON
func parseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应非 JSON: %v, body=%s", err, w.Body.String())
	}
	return m
}

func TestStateAPI(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodGet, "/api/state", "")
	if w.Code != 200 {
		t.Fatalf("state 应 200，实际 %d", w.Code)
	}
	m := parseJSON(t, w)
	if m["champion"] != "S0_Champion" {
		t.Errorf("初始冠军应为 S0，实际 %v", m["champion"])
	}
	if m["mode"] != "manual" {
		t.Errorf("初始模式应为 manual，实际 %v", m["mode"])
	}
	channels := m["channels"].([]any)
	if len(channels) != 3 {
		t.Errorf("应有 3 渠道，实际 %d", len(channels))
	}
}

func TestSwitchAPI(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/switch", `{"to":"S1_MinCost","confirm":true}`)
	m := parseJSON(t, w)
	if m["ok"] != true || m["champion"] != "S1_MinCost" {
		t.Errorf("切换失败: %v", m)
	}
	// 切换留痕（含操作人）
	w2 := do(app, http.MethodGet, "/api/history", "")
	m2 := parseJSON(t, w2)
	hist := m2["history"].([]any)
	if len(hist) != 1 {
		t.Errorf("切换后应有 1 条历史，实际 %d", len(hist))
	}
	entry := hist[0].(map[string]any)
	if entry["Operator"] != "admin" || entry["Triggered"] != "admin" {
		t.Errorf("历史应记录操作人 admin: %v", entry)
	}
}

func TestSwitchRequiresConfirm(t *testing.T) {
	// v6：手动切冠军必须显式 confirm=true
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/switch", `{"to":"S1_MinCost"}`)
	m := parseJSON(t, w)
	if m["ok"] != false {
		t.Errorf("不带 confirm 应被拒，实际 %v", m)
	}
	if !strings.Contains(m["error"].(string), "二次确认") {
		t.Errorf("应提示二次确认: %v", m["error"])
	}
	// 未被拒的历史也不应产生
	w2 := do(app, http.MethodGet, "/api/history", "")
	m2 := parseJSON(t, w2)
	if len(m2["history"].([]any)) != 0 {
		t.Error("被拒的切换不应留痕")
	}
	// 带操作人头
	w3 := doWithHeader(app, http.MethodPost, "/api/switch", `{"to":"S1_MinCost","confirm":true}`, map[string]string{"X-Operator": "zhangsan"})
	m3 := parseJSON(t, w3)
	if m3["ok"] != true {
		t.Fatalf("带确认应成功: %v", m3)
	}
	w4 := do(app, http.MethodGet, "/api/history", "")
	m4 := parseJSON(t, w4)
	entry := m4["history"].([]any)[0].(map[string]any)
	if entry["Operator"] != "zhangsan" {
		t.Errorf("应记录 X-Operator 操作人 zhangsan: %v", entry)
	}
}

func TestSwitchInvalidStrategy(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/switch", `{"to":"S9_Unknown","confirm":true}`)
	m := parseJSON(t, w)
	if m["ok"] != false {
		t.Errorf("未注册策略应切换失败，实际 %v", m)
	}
}

func TestModeRequiresConfirm(t *testing.T) {
	// P3-3 二次确认：不带 confirm 切 auto 被拒
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/mode", `{"mode":"auto"}`)
	m := parseJSON(t, w)
	if m["ok"] != false {
		t.Errorf("未二次确认切 auto 应被拒，实际 %v", m)
	}
	// 带 confirm 成功
	w2 := do(app, http.MethodPost, "/api/mode", `{"mode":"auto","confirm":true}`)
	m2 := parseJSON(t, w2)
	if m2["ok"] != true || m2["mode"] != "auto" {
		t.Errorf("确认后切 auto 应成功，实际 %v", m2)
	}
}

func TestFaultAPI(t *testing.T) {
	app := newTestApp(t)
	// 注入渠道1 故障
	w := do(app, http.MethodPost, "/api/fault", `{"channel":1,"down":true}`)
	m := parseJSON(t, w)
	if m["ok"] != true {
		t.Fatalf("故障注入失败: %v", m)
	}
	channels := m["channels"].([]any)
	// 渠道1 应标记 down
	ch1 := channels[0].(map[string]any)
	if ch1["Up"] != false {
		t.Errorf("渠道1 应标记故障，实际 %v", ch1["Up"])
	}
	// 恢复
	w2 := do(app, http.MethodPost, "/api/fault", `{"channel":1,"down":false}`)
	m2 := parseJSON(t, w2)
	ch1b := m2["channels"].([]any)[0].(map[string]any)
	if ch1b["Up"] != true {
		t.Errorf("恢复后渠道1 应可用，实际 %v", ch1b["Up"])
	}
}

func TestTrafficGET(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodGet, "/api/traffic", "")
	m := parseJSON(t, w)
	weights := m["weights"].(map[string]any)
	if m["total"].(float64) != 100 {
		t.Errorf("初始比例总和应 100，实际 %v", m["total"])
	}
	if _, ok := weights["S3_Hybrid"]; !ok {
		t.Error("流量分配应含 S3_Hybrid")
	}
}

func TestTrafficPOSTValid(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/traffic", `{"weights":{"S0_Champion":60,"S1_MinCost":20,"S3_Hybrid":20}}`)
	m := parseJSON(t, w)
	if m["ok"] != true {
		t.Fatalf("合法比例应保存成功: %v", m)
	}
	// 验证已热加载
	w2 := do(app, http.MethodGet, "/api/traffic", "")
	m2 := parseJSON(t, w2)
	weights := m2["weights"].(map[string]any)
	if weights["S0_Champion"].(float64) != 60 {
		t.Errorf("S0 比例应更新为 60，实际 %v", weights["S0_Champion"])
	}
}

func TestTrafficPOSTInvalidTotal(t *testing.T) {
	app := newTestApp(t)
	// 总和 70% 应拒绝
	w := do(app, http.MethodPost, "/api/traffic", `{"weights":{"S0_Champion":50,"S1_MinCost":20}}`)
	m := parseJSON(t, w)
	if m["ok"] != false {
		t.Errorf("总和 70%% 应拒绝，实际 %v", m)
	}
	if !strings.Contains(m["error"].(string), "100") {
		t.Errorf("错误信息应提示 100%%，实际 %v", m["error"])
	}
}

func TestWeightsPOSTNormalize(t *testing.T) {
	app := newTestApp(t)
	// 填 60/30/10 → 归一化 0.6/0.3/0.1
	w := do(app, http.MethodPost, "/api/weights", `{"strategy":"S3_Hybrid","cost":60,"latency":30,"health":10}`)
	m := parseJSON(t, w)
	if m["ok"] != true {
		t.Fatalf("权重设置失败: %v", m)
	}
	weights := m["weights"].(map[string]any)
	if abs(weights["Cost"].(float64)-0.6) > 0.01 {
		t.Errorf("成本权重应归一化为 0.6，实际 %v", weights["Cost"])
	}
	if abs(weights["Health"].(float64)-0.1) > 0.01 {
		t.Errorf("健康权重应归一化为 0.1，实际 %v", weights["Health"])
	}
}

func TestWeightsPOSTUnknownStrategy(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/weights", `{"strategy":"S9_Nope","cost":1,"latency":1,"health":1}`)
	m := parseJSON(t, w)
	if m["ok"] != false {
		t.Errorf("未知策略应拒绝，实际 %v", m)
	}
}

func TestWeightsPOSTZeroTotal(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodPost, "/api/weights", `{"strategy":"S3_Hybrid","cost":0,"latency":0,"health":0}`)
	m := parseJSON(t, w)
	if m["ok"] != false {
		t.Errorf("权重全 0 应拒绝，实际 %v", m)
	}
}

func TestIndexPage(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodGet, "/", "")
	if w.Code != 200 {
		t.Fatalf("首页应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "路由策略池竞技场") {
		t.Error("首页应含标题")
	}
	if !strings.Contains(body, "流量分配器") {
		t.Error("首页应含流量分配器 Tab")
	}
	if !strings.Contains(body, "权重微调") {
		t.Error("首页应含权重微调 Tab")
	}
}

func TestScoresAPI(t *testing.T) {
	app := newTestApp(t)
	w := do(app, http.MethodGet, "/api/scores", "")
	if w.Code != 200 {
		t.Fatalf("scores 应 200，实际 %d", w.Code)
	}
	m := parseJSON(t, w)
	if m["records"].(float64) != 40 {
		t.Errorf("应评估 40 条记录，实际 %v", m["records"])
	}
	scores := m["scores"].([]any)
	if len(scores) == 0 {
		t.Fatal("scores 不应为空")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// ---------------------------------------------------------------------------
// v6 新增：鉴权 / 参数变更留痕 / 告警 / 持久化
// ---------------------------------------------------------------------------

// TestAuthRequired 设置 ADMIN_TOKEN 后，变更 API 需要 Bearer token
func TestAuthRequired(t *testing.T) {
	app := newTestApp(t)
	app.token = "s3cret-token"

	// 无 token → 401
	h := app.requireAuth(app.handleSwitch)
	req := httptest.NewRequest(http.MethodPost, "/api/switch", bytes.NewReader([]byte(`{"to":"S1_MinCost","confirm":true}`)))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，实际 %d", w.Code)
	}
	// 错 token → 401
	req2 := httptest.NewRequest(http.MethodPost, "/api/switch", bytes.NewReader([]byte(`{"to":"S1_MinCost","confirm":true}`)))
	req2.Header.Set("Authorization", "Bearer wrong")
	w2 := httptest.NewRecorder()
	h(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401，实际 %d", w2.Code)
	}
	// 对 token → 放行
	req3 := httptest.NewRequest(http.MethodPost, "/api/switch", bytes.NewReader([]byte(`{"to":"S1_MinCost","confirm":true}`)))
	req3.Header.Set("Authorization", "Bearer s3cret-token")
	w3 := httptest.NewRecorder()
	h(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("对 token 应放行，实际 %d body=%s", w3.Code, w3.Body.String())
	}
	// 开发模式（token 为空）→ 不鉴权
	app2 := newTestApp(t)
	h2 := app2.requireAuth(app2.handleSwitch)
	req4 := httptest.NewRequest(http.MethodPost, "/api/switch", bytes.NewReader([]byte(`{"to":"S1_MinCost","confirm":true}`)))
	w4 := httptest.NewRecorder()
	h2(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("开发模式应放行，实际 %d", w4.Code)
	}
}

// TestParamChangeInHistory 权重/流量调整进审计历史
func TestParamChangeInHistory(t *testing.T) {
	app := newTestApp(t)
	// 权重微调
	do(app, http.MethodPost, "/api/weights", `{"strategy":"S3_Hybrid","cost":60,"latency":30,"health":10}`)
	// 流量分配
	do(app, http.MethodPost, "/api/traffic", `{"weights":{"S0_Champion":60,"S1_MinCost":20,"S3_Hybrid":20}}`)

	w := do(app, http.MethodGet, "/api/history", "")
	m := parseJSON(t, w)
	hist := m["history"].([]any)
	if len(hist) != 2 {
		t.Fatalf("应有 2 条参数变更历史，实际 %d: %v", len(hist), hist)
	}
	first := hist[0].(map[string]any)
	if !strings.Contains(first["Reason"].(string), "param change: weights.S3_Hybrid") {
		t.Errorf("第一条应为权重变更: %v", first["Reason"])
	}
	second := hist[1].(map[string]any)
	if !strings.Contains(second["Reason"].(string), "param change: traffic") {
		t.Errorf("第二条应为流量变更: %v", second["Reason"])
	}
	// 参数变更不改冠军
	if app.mgr.Champion() != "S0_Champion" {
		t.Errorf("参数变更不应改冠军: %s", app.mgr.Champion())
	}
	// 操作人默认 admin
	if first["Operator"] != "admin" {
		t.Errorf("默认操作人应为 admin: %v", first["Operator"])
	}
}

// TestAlertsAPI 故障注入/切换产生告警，/api/alerts 可查
func TestAlertsAPI(t *testing.T) {
	app := newTestApp(t)
	do(app, http.MethodPost, "/api/fault", `{"channel":1,"down":true}`)
	do(app, http.MethodPost, "/api/switch", `{"to":"S1_MinCost","confirm":true}`)

	w := do(app, http.MethodGet, "/api/alerts", "")
	m := parseJSON(t, w)
	alerts := m["alerts"].([]any)
	if len(alerts) < 2 {
		t.Fatalf("应有 ≥2 条告警（故障注入+切换），实际 %d", len(alerts))
	}
	joined := ""
	for _, a := range alerts {
		joined += a.(map[string]any)["msg"].(string) + " "
	}
	if !strings.Contains(joined, "故障注入") || !strings.Contains(joined, "切换冠军") {
		t.Errorf("告警内容不符: %s", joined)
	}
}

// TestPersistenceRoundTrip 落盘目录下：切换历史重启可查
func TestPersistenceRoundTrip(t *testing.T) {
	dataDir := t.TempDir()

	// 第一次启动：切一次冠军
	app1 := newTestAppWithData(t, dataDir)
	do(app1, http.MethodPost, "/api/switch", `{"to":"S1_MinCost","confirm":true}`)

	// 第二次启动（模拟重启）：历史应恢复
	app2 := newTestAppWithData(t, dataDir)
	if len(app2.mgr.History()) != 1 {
		t.Fatalf("重启后应恢复 1 条历史，实际 %d", len(app2.mgr.History()))
	}
	entry := app2.mgr.History()[0]
	if entry.To != "S1_MinCost" || entry.Operator != "admin" {
		t.Errorf("恢复的历史内容不对: %+v", entry)
	}
	// 历史文件确实落盘
	if _, err := os.Stat(filepath.Join(dataDir, "history.jsonl")); err != nil {
		t.Errorf("history.jsonl 应存在: %v", err)
	}
	// 告警文件也应存在（启动时 OnTrip 未触发，但 alert 只发生在变更后；
	// 第二次启动后做一次切换，告警应落盘）
	do(app2, http.MethodPost, "/api/switch", `{"to":"S2_MinLat","confirm":true}`)
	if _, err := os.Stat(filepath.Join(dataDir, "alerts.jsonl")); err != nil {
		t.Errorf("alerts.jsonl 应存在: %v", err)
	}
}

// TestMetricsAPI /api/metrics 输出 Prometheus 文本（调用 /api/scores 后直方图有数据）
func TestMetricsAPI(t *testing.T) {
	app := newTestApp(t)
	do(app, http.MethodGet, "/api/scores", "") // 喂数据给直方图
	w := do(app, http.MethodGet, "/api/metrics", "")
	if w.Code != 200 {
		t.Fatalf("metrics 应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "# TYPE req_latency_ms histogram") {
		t.Errorf("应输出 histogram 类型行: %s", body[:200])
	}
	if !strings.Contains(body, "req_latency_ms_count") {
		t.Errorf("应有 count 行: %s", body[:300])
	}
}
