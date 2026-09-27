package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// Prometheus /metrics 契约测试。
//
// 覆盖的不变量：
//	P1 exposition 语法合法（逐行匹配 0.0.4 文本格式），末尾有换行；
//	P2 每个指标家族的 HELP/TYPE 恰好一次（重复会让解析器整批丢弃）；
//	P3 label 值正确转义（模型名可能来自客户端，含引号/反斜杠/换行）；
//	P4 指标值来自既有聚合（MetricsSnapshotOf），不另建计数器；
//	P5 池维度系列与 Pool.RealmHealth / CountsDetailedForRealm 逐字段一致；
//	P6 鉴权与 /status 同口径（设 key 后无/错 Bearer → 401）；
//	P7 端点门控：metrics.enabled=false 时路径不存在（404）；
//	P8 输出确定性（两次抓取逐字节相等，不泄漏 map 迭代序）；
//	P9 计数重置语义可见（since 来自快照，不是"当前时刻"）。

// promSampleLineRe 合法样本行：指标名[(label="值",...)] 空格 数值。
var promSampleLineRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*")*\})? (?:NaN|[+-]?Inf|[+-]?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)$`)

// promCommentLineRe 合法注释行：# HELP <name> <文本> 或 # TYPE <name> <type>。
var promCommentLineRe = regexp.MustCompile(`^# (?:HELP|TYPE) [a-zA-Z_:][a-zA-Z0-9_:]*(?: .*)?$`)

// seedMetric 构造一条 chatStat 并走唯一埋点 recordChatMetric，避免依赖完整 chat 链路。
// 每个用例开头必须 ResetMetrics()，用例结束 t.Cleanup(ResetMetrics)——聚合是包级单例，
// 不复位会跨用例污染。
func seedMetric(model string, status int, prompt, comp int, credit float64) {
	st := &chatStat{
		start:     time.Now(),
		model:     model,
		mode:      "stream",
		status:    status,
		toks:      comp,
		hasUsage:  true,
		prompt:    prompt,
		credit:    credit,
		hasCredit: true,
		ttfb:      5 * time.Millisecond,
	}
	recordChatMetric(st, 12*time.Millisecond)
}

// serveMetrics 起一个带 /metrics 的 handler 并返回响应体。
func serveMetrics(t *testing.T, cfg Config) (int, string) {
	t.Helper()
	h := NewHandler(cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Code, rec.Body.String()
}

// seriesValue 在 exposition 文本里查找指定系列（指标名 + 精确 label 集合）的数值。
// 注意 label 值按**未转义**形式比较——本文件用的 label 都是纯 ASCII，无转义差异。
func seriesValue(body, name string, labels ...string) (float64, bool) {
	want := name
	if len(labels) > 0 {
		var sb strings.Builder
		sb.WriteString(name)
		sb.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(labels[i])
			sb.WriteString(`="`)
			sb.WriteString(labels[i+1])
			sb.WriteByte('"')
		}
		sb.WriteByte('}')
		want = sb.String()
	}
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "#") {
			continue
		}
		sp := strings.LastIndexByte(ln, ' ')
		if sp < 0 || ln[:sp] != want {
			continue
		}
		v, err := strconv.ParseFloat(ln[sp+1:], 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// labelValueOf 取某指标首条样本的指定 label 的**已转义**原始值（调用方按需反解）。
func labelValueOf(body, name, label string) (string, bool) {
	prefix := name + "{"
	for _, ln := range strings.Split(body, "\n") {
		if !strings.HasPrefix(ln, prefix) {
			continue
		}
		rest := ln[len(prefix):]
		key := label + `="`
		i := strings.Index(rest, key)
		if i < 0 {
			continue
		}
		var sb strings.Builder
		for j := i + len(key); j < len(rest); j++ {
			c := rest[j]
			if c == '\\' && j+1 < len(rest) {
				sb.WriteByte(c)
				sb.WriteByte(rest[j+1])
				j++
				continue
			}
			if c == '"' {
				return sb.String(), true
			}
			sb.WriteByte(c)
		}
	}
	return "", false
}

// unescapePromLabel 反解 label 转义（\n→换行、\\→\、\"→"），用于还原原始值比对。
func unescapePromLabel(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TestPromMetricsFormatContract P1：逐行语法合法 + 末尾换行 + 关键家族齐备。
func TestPromMetricsFormatContract(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	seedMetric("glm-5.3", 200, 10, 20, 0.5)
	seedMetric("global:deepseek-v4.1-flash", 503, 3, 0, 0)

	p := pool.New("")
	code, body := serveMetrics(t, Config{Pool: p, MetricsEnabled: true, StickyCount: func() int { return 3 }})
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200", code)
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("exposition 必须以换行结尾")
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "#"):
			if !promCommentLineRe.MatchString(ln) {
				t.Errorf("第 %d 行不是合法注释行: %q", i+1, ln)
			}
		default:
			if !promSampleLineRe.MatchString(ln) {
				t.Errorf("第 %d 行不是合法样本行: %q", i+1, ln)
			}
		}
	}
	// 关键家族必须存在（否则语法测试会"空转通过"）。
	for _, name := range []string{
		"wb2api_pool_accounts_total", "wb2api_pool_accounts", "wb2api_pool_in_flight",
		"wb2api_sticky_sessions", "wb2api_cost_explore_events_total",
		"wb2api_waf_ip_block_active", "wb2api_stats_since_timestamp_seconds",
		"wb2api_requests_total", "wb2api_model_requests_total",
		"wb2api_model_tokens_total", "wb2api_model_cache_tokens_total",
		"wb2api_model_credit_total", "wb2api_model_avg_ttfb_seconds",
		"wb2api_model_avg_latency_seconds", "wb2api_model_tokens_per_second",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("缺少指标家族 %s", name)
		}
	}
}

// TestPromMetricsHelpTypeOncePerFamily P2：HELP/TYPE 每家族恰好一次。
// 本文件按"家族 → 外层循环（realm/model）内层写样本"的顺序输出，没有去重就会重复。
func TestPromMetricsHelpTypeOncePerFamily(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	seedMetric("a-model", 200, 1, 1, 0)
	seedMetric("b-model", 200, 1, 1, 0)

	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})

	_, body := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})

	helpCount := map[string]int{}
	typeCount := map[string]int{}
	for _, ln := range strings.Split(body, "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 || f[0] != "#" {
			continue
		}
		switch f[1] {
		case "HELP":
			helpCount[f[2]]++
		case "TYPE":
			typeCount[f[2]]++
		}
	}
	if len(typeCount) == 0 {
		t.Fatal("未解析到任何 TYPE 行（测试前提失效）")
	}
	for name, n := range typeCount {
		if n != 1 {
			t.Errorf("%s 的 TYPE 出现 %d 次 want 1", name, n)
		}
	}
	for name, n := range helpCount {
		if n != 1 {
			t.Errorf("%s 的 HELP 出现 %d 次 want 1", name, n)
		}
	}
	if len(helpCount) != len(typeCount) {
		t.Errorf("HELP 家族数 %d != TYPE 家族数 %d（每个家族两者必须成对）", len(helpCount), len(typeCount))
	}
}

// TestPromMetricsLabelEscaping P3：含引号/反斜杠/换行的模型名被正确转义，
// 反解后还原为原名；未转义会让整行语法破裂（原始换行会拆行）。
func TestPromMetricsLabelEscaping(t *testing.T) {
	weird := "m\"q\\b\nnl"
	snap := MetricsSnapshot{
		Since:  time.Now(),
		Models: []ModelStatPayload{{Model: weird, Requests: 1, Success: 1}},
	}
	out := writePromMetrics(snap, nil, 0, 0, false, nil)

	for i, ln := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.HasPrefix(ln, "#") {
			continue
		}
		if !promSampleLineRe.MatchString(ln) {
			t.Fatalf("第 %d 行语法破裂（转义缺失？）: %q", i+1, ln)
		}
	}
	esc, ok := labelValueOf(out, "wb2api_model_requests_total", "model")
	if !ok {
		t.Fatal("未找到 model 标签")
	}
	if got := unescapePromLabel(esc); got != weird {
		t.Errorf("label 反解 = %q want %q", got, weird)
	}
	if esc == weird {
		t.Error("label 未被转义（原文直接写出）")
	}
}

// TestPromMetricsReusesAggregation P4：指标值来自既有聚合，不另建计数器。
// 若有人为 /metrics 另起一套累加，本断言会与其分叉。
func TestPromMetricsReusesAggregation(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	seedMetric("glm-5.3", 200, 10, 20, 0.25)
	seedMetric("glm-5.3", 200, 5, 7, 0.25)
	seedMetric("glm-5.3", 503, 0, 0, 0)

	p := pool.New("")
	_, body := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})

	snap := MetricsSnapshotOf()
	var mm *ModelStatPayload
	for i := range snap.Models {
		if snap.Models[i].Model == "glm-5.3" {
			mm = &snap.Models[i]
		}
	}
	if mm == nil {
		t.Fatal("聚合里应有 glm-5.3")
	}

	checks := []struct {
		name   string
		labels []string
		want   float64
	}{
		{"wb2api_model_requests_total", []string{"model", "glm-5.3", "status", "success"}, float64(mm.Success)},
		{"wb2api_model_requests_total", []string{"model", "glm-5.3", "status", "failed"}, float64(mm.Failed)},
		{"wb2api_model_tokens_total", []string{"model", "glm-5.3", "type", "prompt"}, float64(mm.PromptTokens)},
		{"wb2api_model_tokens_total", []string{"model", "glm-5.3", "type", "completion"}, float64(mm.CompletionTokens)},
		{"wb2api_model_credit_total", []string{"model", "glm-5.3"}, mm.Credit},
		{"wb2api_requests_total", []string{"status", "success"}, float64(snap.Total.Success)},
		{"wb2api_requests_total", []string{"status", "failed"}, float64(snap.Total.Failed)},
	}
	for _, c := range checks {
		got, ok := seriesValue(body, c.name, c.labels...)
		if !ok {
			t.Errorf("未找到系列 %s%v", c.name, c.labels)
			continue
		}
		if got != c.want {
			t.Errorf("%s%v = %v want %v（导出值与聚合分叉）", c.name, c.labels, got, c.want)
		}
	}
	if got, _ := seriesValue(body, "wb2api_model_requests_total", "model", "glm-5.3", "status", "success"); got != 2 {
		t.Errorf("success 应为 2，got %v", got)
	}
}

// TestPromMetricsRealmSeriesMatchPool P5：池维度系列与 RealmHealth/CountsDetailedForRealm 一致。
func TestPromMetricsRealmSeriesMatchPool(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Cooldown("cn2", pool.CoolSoft, time.Hour, "429 rate limit")

	_, body := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})

	for _, realm := range []struct {
		label string
		key   string
	}{
		{"cn", "cn"},
		{"global", "global"},
		{"all", ""},
	} {
		h := p.RealmHealth(realm.key)
		if got, ok := seriesValue(body, "wb2api_pool_accounts_total", "realm", realm.label); !ok || got != float64(h.Total) {
			t.Errorf("pool_accounts_total{realm=%q} = %v(ok=%v) want %d", realm.label, got, ok, h.Total)
		}
		if got, ok := seriesValue(body, "wb2api_pool_in_flight", "realm", realm.label); !ok || got != float64(h.InFlight) {
			t.Errorf("pool_in_flight{realm=%q} = %v(ok=%v) want %d", realm.label, got, ok, h.InFlight)
		}
		// 五个共享字段与 CountsDetailedForRealm 交叉核对（两个入口不得漂移）。
		ct, ch, cc, cd, cf := p.CountsDetailedForRealm(realm.key)
		states := map[string]int{
			"healthy": h.Healthy, "cooling": h.Cooling, "disabled": h.Disabled, "in_flight_full": h.InFlightFull,
		}
		if states["healthy"] != ch || states["cooling"] != cc || states["disabled"] != cd || states["in_flight_full"] != cf {
			t.Errorf("realm=%q RealmHealth(%v) 与 CountsDetailedForRealm(%d,%d,%d,%d,%d) 漂移",
				realm.label, states, ct, ch, cc, cd, cf)
		}
		if got, ok := seriesValue(body, "wb2api_pool_accounts_total", "realm", realm.label); !ok || got != float64(ct) {
			t.Errorf("pool_accounts_total{realm=%q} 与 CountsDetailedForRealm.total 不一致", realm.label)
		}
		for state, want := range states {
			got, ok := seriesValue(body, "wb2api_pool_accounts", "realm", realm.label, "state", state)
			if !ok {
				t.Errorf("缺少系列 pool_accounts{realm=%q,state=%q}", realm.label, state)
				continue
			}
			if got != float64(want) {
				t.Errorf("pool_accounts{realm=%q,state=%q} = %v want %d", realm.label, state, got, want)
			}
		}
	}
	// 具体数值锁定（防止"两边同时错"）：cn 有 1 健康 1 冷却。
	if got, _ := seriesValue(body, "wb2api_pool_accounts", "realm", "cn", "state", "healthy"); got != 1 {
		t.Errorf("cn healthy = %v want 1", got)
	}
	if got, _ := seriesValue(body, "wb2api_pool_accounts", "realm", "cn", "state", "cooling"); got != 1 {
		t.Errorf("cn cooling = %v want 1", got)
	}
}

// TestPromMetricsRequiresAuth P6：与 /status 同鉴权口径（设 key 后无/错 Bearer → 401）。
func TestPromMetricsRequiresAuth(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	p := pool.New("")
	h := NewHandler(Config{Pool: p, MetricsEnabled: true, APIKey: "secret"})

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"无 Authorization", "", http.StatusUnauthorized},
		{"错 key", "Bearer nope", http.StatusUnauthorized},
		{"缺 Bearer 前缀", "secret", http.StatusUnauthorized},
		{"正确 key", "Bearer secret", http.StatusOK},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/metrics", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: code=%d want %d", c.name, rec.Code, c.want)
		}
	}
}

// TestPromMetricsRouteGated P7：metrics.enabled=false 时路径不注册（404），
// 而不是注册后返回 403——不向未鉴权探测暴露"这里存在指标面"。
func TestPromMetricsRouteGated(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	p := pool.New("")

	code, _ := serveMetrics(t, Config{Pool: p}) // 未开启
	if code != http.StatusNotFound {
		t.Errorf("metrics 未开启时 /metrics 应 404（路径不存在），got %d", code)
	}
	code, _ = serveMetrics(t, Config{Pool: p, MetricsEnabled: true})
	if code != http.StatusOK {
		t.Errorf("metrics 开启时 /metrics 应 200，got %d", code)
	}
}

// TestPromMetricsStableOrdering P8：两次抓取逐字节相等（不泄漏 map 迭代序）。
// 池/模型/状态三个维度都来自 map，任何一处漏排序都会让本断言抖动失败。
func TestPromMetricsStableOrdering(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	// 多模型 + 多账号，最大化 map 迭代序暴露面。
	for _, m := range []string{"m-c", "m-a", "m-b", "global:m-z", "m-d"} {
		seedMetric(m, 200, 1, 2, 0.1)
	}
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	for _, uid := range []string{"z1", "a1", "m1"} {
		p.Add(&auth.Auth{UID: uid, Domain: "www.codebuddy.cn", AccessToken: "at"})
	}
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Disable("m1", "session dead")

	_, body1 := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})
	_, body2 := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})
	if body1 != body2 {
		t.Error("两次抓取输出不一致（存在未排序的 map 迭代）")
	}
	// 模型标签按字典序（显式断言排序语义，而非仅依赖两次相等）。
	var order []string
	for _, ln := range strings.Split(body1, "\n") {
		if !strings.HasPrefix(ln, "wb2api_model_requests_streaming_total{") {
			continue
		}
		if v, ok := labelValueOf(ln+"\n", "wb2api_model_requests_streaming_total", "model"); ok {
			order = append(order, v)
		}
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("模型标签未按字典序: %v", order)
	}
}

// TestPromMetricsSinceExposedOnReset P9：since 来自聚合快照而非"当前时刻"，
// 且 ResetMetrics 后计数归零——看板据此识别计数器重置。
func TestPromMetricsSinceExposedOnReset(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)
	p := pool.New("")

	seedMetric("glm-5.3", 200, 1, 1, 0)
	_, body1 := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})
	if got, _ := seriesValue(body1, "wb2api_requests_total", "status", "success"); got != 1 {
		t.Fatalf("重置前 success = %v want 1", got)
	}

	ResetMetrics()
	_, body2 := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})
	if got, _ := seriesValue(body2, "wb2api_requests_total", "status", "success"); got != 0 {
		t.Errorf("重置后 success = %v want 0", got)
	}

	// 把聚合起点拨到 1000 秒前，导出值必须跟随（若实现用 time.Now() 则本断言失败）。
	globalMetrics.mu.Lock()
	globalMetrics.since = time.Now().Add(-1000 * time.Second)
	want := globalMetrics.since.Unix()
	globalMetrics.mu.Unlock()

	_, body3 := serveMetrics(t, Config{Pool: p, MetricsEnabled: true})
	got, ok := seriesValue(body3, "wb2api_stats_since_timestamp_seconds")
	if !ok {
		t.Fatal("缺少 wb2api_stats_since_timestamp_seconds")
	}
	if int64(got) != want {
		t.Errorf("since = %d want %d（必须来自聚合快照，不是当前时刻）", int64(got), want)
	}
}
