package alert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 告警状态机与投递契约测试。
//
// 覆盖的不变量：
//	A1 持续越界达 for_ticks 后**恰好一次**投递（边沿触发）；
//	A2 firing 期间持续越界**不重复投递**（防刷屏，核心不变式）；
//	A3 恢复后再次越界能**重新**触发（重臂）；
//	A4 阈值附近抖动（未达 for_ticks 就回落）不触发（迟滞）；
//	A5 global 规则默认关闭（纯 CN 部署不误报）；
//	A6 启动宽限期内不投递；
//	A7 webhook 超时有界返回、不阻塞、不 panic；
//	A8 enabled=false 时零 HTTP 外发；
//	A9 载荷不含任何凭证/token/uid/会话键（泄漏守卫）；
//	A10 配置 secret 时签名可验证，未配置时无签名头。

// captured 一次收到的 webhook 请求。
type captured struct {
	body []byte
	sig  string
}

// receiver 测试用 webhook 接收端。
type receiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []captured
	// delay 收到请求后先 sleep（模拟慢接收端，用于超时测试）。
	delay time.Duration
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if r.delay > 0 {
			time.Sleep(r.delay)
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, captured{body: b, sig: req.Header.Get("X-WB2A-Signature")})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) URL() string { return r.srv.URL }

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *receiver) raw(t *testing.T, i int) captured {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.reqs) {
		t.Fatalf("只有 %d 条请求，取不到第 %d 条", len(r.reqs), i)
	}
	return r.reqs[i]
}

func (r *receiver) payload(t *testing.T, i int) Payload {
	t.Helper()
	var p Payload
	if err := json.Unmarshal(r.raw(t, i).body, &p); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v (body=%s)", err, r.raw(t, i).body)
	}
	return p
}

// fakeSource 可控健康快照源。
type fakeSource struct {
	mu     sync.Mutex
	cn     Health
	global Health
	waf    bool
}

func (f *fakeSource) Health(realm string) Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	if realm == "global" {
		return f.global
	}
	return f.cn
}

func (f *fakeSource) WAFActive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waf
}

func (f *fakeSource) setCN(h Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cn = h
}

// newTestMonitor 构造已跳过启动宽限的监控器（把 startedAt 拨到一小时前），
// 让 Tick 可以立即评估。
func newTestMonitor(t *testing.T, cfg Config, src Source) *Monitor {
	t.Helper()
	cfg.Enabled = true
	m := New(cfg, src)
	m.startedAt = time.Now().Add(-time.Hour)
	return m
}

// advance 从 base 起连推 n 拍（每拍间隔 1s），返回最后一次的时刻。
func advance(m *Monitor, base time.Time, n int) time.Time {
	for i := 0; i < n; i++ {
		base = base.Add(time.Second)
		m.Tick(base)
	}
	return base
}

// TestAlertFiresOnceOnSustainedThreshold A1：未达 for_ticks 不投递，达阈**恰好一次**，
// 载荷字段正确。
func TestAlertFiresOnceOnSustainedThreshold(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0, Cooling: 2, Disabled: 1, Breaker: 1}}
	m := newTestMonitor(t, Config{WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 2}, src)

	base := time.Now()
	m.Tick(base) // badTicks=1 → pending
	if got := rec.count(); got != 0 {
		t.Fatalf("第 1 拍（未达 for_ticks=2）不应投递，got %d", got)
	}
	m.Tick(base.Add(time.Second)) // badTicks=2 → firing
	if got := rec.count(); got != 1 {
		t.Fatalf("第 2 拍应恰好投递 1 次，got %d", got)
	}

	p := rec.payload(t, 0)
	if p.Event != "alert" {
		t.Errorf("event = %q want alert", p.Event)
	}
	if p.Rule != "realm_healthy_low" || p.Realm != "cn" {
		t.Errorf("rule/realm = %q/%q want realm_healthy_low/cn", p.Rule, p.Realm)
	}
	if p.Value != 0 || p.Threshold != 1 {
		t.Errorf("value/threshold = %v/%v want 0/1", p.Value, p.Threshold)
	}
	if p.Version != 1 || p.Service != defaultServiceName {
		t.Errorf("version/service = %d/%q want 1/%s", p.Version, p.Service, defaultServiceName)
	}
	if p.Since.IsZero() || p.FiredAt.IsZero() {
		t.Error("since/fired_at 不得为零值（接收端幂等键需要）")
	}
	if p.Snapshot.CN.Total != 3 || p.Snapshot.CN.Healthy != 0 || p.Snapshot.CN.Breaker != 1 {
		t.Errorf("snapshot.cn = %+v 与源不一致", p.Snapshot.CN)
	}
}

// TestAlertDeduplicatedWhileFiring A2（核心不变式）：firing 期间持续越界不再投递。
// 若去掉"只在 pending→firing 跃迁发送"的机制，本断言会看到 10 次投递。
func TestAlertDeduplicatedWhileFiring(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0}}
	m := newTestMonitor(t, Config{WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 1}, src)

	last := advance(m, time.Now(), 10) // 连续 10 拍持续越界
	if got := rec.count(); got != 1 {
		t.Fatalf("持续越界 10 拍应恰好投递 1 次（不刷屏），got %d", got)
	}
	// 继续推进，仍不重复。
	advance(m, last, 10)
	if got := rec.count(); got != 1 {
		t.Errorf("继续越界 10 拍仍应只有 1 次，got %d", got)
	}
}

// TestAlertRearmsAfterRecovery A3：恢复（连续 clear_ticks 拍达标）后再次越界能重新触发。
func TestAlertRearmsAfterRecovery(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0}}
	m := newTestMonitor(t, Config{
		WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 1, ClearTicks: 2, RecoverHealthy: 1,
	}, src)

	base := advance(m, time.Now(), 1) // 触发
	if got := rec.count(); got != 1 {
		t.Fatalf("应触发 1 次，got %d", got)
	}

	// 恢复：healthy=1 达恢复阈值，连续 2 拍才解除。
	src.setCN(Health{Total: 3, Healthy: 1})
	base = advance(m, base, 1)
	if got := rec.count(); got != 1 {
		t.Errorf("恢复第 1 拍（未达 clear_ticks=2）不应解除，got %d", got)
	}
	base = advance(m, base, 1) // 第 2 拍 → OK
	// 再次越界 → 重新触发
	src.setCN(Health{Total: 3, Healthy: 0})
	advance(m, base, 1)
	if got := rec.count(); got != 2 {
		t.Fatalf("恢复后再次越界应重新触发（共 2 次），got %d", got)
	}
	if p := rec.payload(t, 1); p.Rule != "realm_healthy_low" {
		t.Errorf("第二次事件 rule = %q", p.Rule)
	}
}

// TestAlertHysteresisNoFlap A4：阈值附近抖动（未达 for_ticks 就回落）不触发。
func TestAlertHysteresisNoFlap(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0}}
	m := newTestMonitor(t, Config{WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 3}, src)

	base := time.Now()
	for i := 0; i < 5; i++ {
		// 只坏 1 拍就恢复 → badTicks 永远到不了 3。
		src.setCN(Health{Total: 3, Healthy: 0})
		base = base.Add(time.Second)
		m.Tick(base)
		src.setCN(Health{Total: 3, Healthy: 1})
		base = base.Add(time.Second)
		m.Tick(base)
	}
	if got := rec.count(); got != 0 {
		t.Errorf("抖动（未达 for_ticks）不应触发，got %d 次投递", got)
	}

	// 反证：把 for_ticks 降到 1 后同样的抖动序列会触发——证明上面的 0 不是"压根没在评估"。
	rec2 := newReceiver(t)
	m2 := newTestMonitor(t, Config{WebhookURL: rec2.URL(), MinHealthyCN: 1, ForTicks: 1}, src)
	src.setCN(Health{Total: 3, Healthy: 0})
	m2.Tick(time.Now())
	if got := rec2.count(); got != 1 {
		t.Errorf("对照组（for_ticks=1）应触发 1 次，got %d", got)
	}
}

// TestAlertGlobalRuleOffByDefault A5：min_healthy_global=0 → 该规则根本不构建，
// 纯 CN 部署（无 global 账号）不会常驻误报。
func TestAlertGlobalRuleOffByDefault(t *testing.T) {
	rec := newReceiver(t)
	// global 健康数为 0，但 MinHealthyGlobal 缺省 0（关闭）。
	src := &fakeSource{cn: Health{Total: 2, Healthy: 2}, global: Health{Total: 0, Healthy: 0}}
	m := newTestMonitor(t, Config{WebhookURL: rec.URL(), MinHealthyCN: 1}, src)

	advance(m, time.Now(), 5)
	if got := rec.count(); got != 0 {
		t.Fatalf("global 规则默认关闭时不应有任何投递，got %d", got)
	}
	for _, r := range m.rules {
		if r.realm == "global" {
			t.Fatal("MinHealthyGlobal=0 时不应构建 global 规则")
		}
	}

	// 显式开启后应当触发（证明规则本身可用，上面的 0 不是实现缺失）。
	rec2 := newReceiver(t)
	m2 := newTestMonitor(t, Config{WebhookURL: rec2.URL(), MinHealthyCN: 1, MinHealthyGlobal: 1, ForTicks: 1}, src)
	m2.Tick(time.Now())
	if got := rec2.count(); got != 1 {
		t.Fatalf("显式开启 global 规则后应触发，got %d", got)
	}
	if p := rec2.payload(t, 0); p.Realm != "global" {
		t.Errorf("realm = %q want global", p.Realm)
	}
}

// TestAlertStartupGrace A6：启动宽限期内不评估（auths 尚未 sync 时健康数为 0）。
func TestAlertStartupGrace(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0}}
	cfg := Config{Enabled: true, WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 1, StartupGrace: time.Hour}
	m := New(cfg, src) // 不拨 startedAt：仍在宽限期内

	now := time.Now()
	advance(m, now, 3)
	if got := rec.count(); got != 0 {
		t.Fatalf("宽限期内不应投递，got %d", got)
	}

	// 宽限期外（把起点拨回去）应当触发。
	m.startedAt = time.Now().Add(-2 * time.Hour)
	m.Tick(time.Now())
	if got := rec.count(); got != 1 {
		t.Fatalf("宽限期外应触发 1 次，got %d", got)
	}
}

// TestAlertWebhookTimeoutDoesNotBlock A7：接收端比 timeout 慢时，Tick 有界返回、
// 不 panic、不卡住评估循环。
func TestAlertWebhookTimeoutDoesNotBlock(t *testing.T) {
	rec := newReceiver(t)
	rec.delay = 300 * time.Millisecond
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0}}
	m := newTestMonitor(t, Config{
		WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 1, Timeout: 40 * time.Millisecond,
	}, src)

	start := time.Now()
	m.Tick(time.Now())
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("Tick 耗时 %v，应被 Timeout=40ms 界定（不阻塞评估循环）", elapsed)
	}
	// 后续拍仍能正常评估（循环未被拖死）。
	before := rec.count()
	advance(m, time.Now(), 2)
	if rec.count() != before {
		t.Error("firing 期间不应新增投递")
	}
}

// TestAlertDefaultOffNoHTTP A8：enabled=false → 不起 goroutine、零外发。
func TestAlertDefaultOffNoHTTP(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 3, Healthy: 0}}
	m := New(Config{WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 1}, src) // Enabled 缺省 false

	advance(m, time.Now(), 10)
	if got := rec.count(); got != 0 {
		t.Fatalf("未开启时不得外发任何请求，got %d", got)
	}
	// Run 也应立即返回（不阻塞）。
	done := make(chan struct{})
	go func() { m.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enabled=false 时 Run 应立即返回")
	}
}

// TestAlertPayloadNoSecrets A9：载荷不含凭证/token/uid/会话键（泄漏守卫）。
func TestAlertPayloadNoSecrets(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 1, Healthy: 0, Breaker: 1}, waf: true}
	m := newTestMonitor(t, Config{
		WebhookURL: rec.URL(), Secret: "topsecret", MinHealthyCN: 1, ForTicks: 1,
		BreakerThreshold: 1,
	}, src)

	advance(m, time.Now(), 2)
	if rec.count() == 0 {
		t.Fatal("应至少投递一次（测试前提失效）")
	}
	for i := 0; i < rec.count(); i++ {
		body := strings.ToLower(string(rec.raw(t, i).body))
		for _, banned := range []string{"api_key", "apikey", "access_token", "refresh_token", "device_token", "bearer ", "\"uid\"", "conversation", "prompt_cache"} {
			if strings.Contains(body, banned) {
				t.Errorf("载荷含敏感字段 %q: %s", banned, body)
			}
		}
		// 签名密钥本身绝不能出现在 body 里。
		if strings.Contains(body, "topsecret") {
			t.Error("载荷泄漏了 webhook secret")
		}
	}
}

// TestAlertHMACSignature A10：配置 secret 时签名可被验证，未配置时无签名头。
func TestAlertHMACSignature(t *testing.T) {
	const secret = "s3cr3t"
	rec := newReceiver(t)
	src := &fakeSource{cn: Health{Total: 1, Healthy: 0}}
	m := newTestMonitor(t, Config{
		WebhookURL: rec.URL(), Secret: secret, MinHealthyCN: 1, ForTicks: 1,
	}, src)

	m.Tick(time.Now())
	if rec.count() != 1 {
		t.Fatalf("应投递 1 次，got %d", rec.count())
	}
	got := rec.raw(t, 0)
	if !strings.HasPrefix(got.sig, "sha256=") {
		t.Fatalf("签名头格式错误: %q", got.sig)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(got.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(got.sig), []byte(want)) {
		t.Errorf("签名不匹配\n got=%s\nwant=%s", got.sig, want)
	}

	// 未配置 secret → 无签名头。
	rec2 := newReceiver(t)
	m2 := newTestMonitor(t, Config{WebhookURL: rec2.URL(), MinHealthyCN: 1, ForTicks: 1}, src)
	m2.Tick(time.Now())
	if sig := rec2.raw(t, 0).sig; sig != "" {
		t.Errorf("未配置 secret 时不应有签名头，got %q", sig)
	}
}

// TestAlertConfigNormalizeDefaults 零值/负值回落默认，显式值不被覆盖。
func TestAlertConfigNormalizeDefaults(t *testing.T) {
	c := Config{}
	c.normalize()
	if c.Interval != defaultInterval || c.Timeout != defaultTimeout ||
		c.ForTicks != defaultForTicks || c.ClearTicks != defaultClearTicks ||
		c.StartupGrace != defaultStartupGrace || c.RecoverHealthy != defaultRecover ||
		c.ServiceName != defaultServiceName {
		t.Errorf("零值应全部回落默认，got %+v", c)
	}

	c = Config{Interval: time.Minute, Timeout: 2 * time.Second, ForTicks: 5, ClearTicks: 7, StartupGrace: time.Second, RecoverHealthy: 3, ServiceName: "custom"}
	c.normalize()
	if c.Interval != time.Minute || c.Timeout != 2*time.Second || c.ForTicks != 5 ||
		c.ClearTicks != 7 || c.StartupGrace != time.Second || c.RecoverHealthy != 3 || c.ServiceName != "custom" {
		t.Errorf("显式配置不应被覆盖，got %+v", c)
	}
}

// TestAlertSendResolveOptIn 恢复通知默认不发；SendResolve=true 时才发。
func TestAlertSendResolveOptIn(t *testing.T) {
	src := &fakeSource{cn: Health{Total: 1, Healthy: 0}}

	// 默认：只发 alert，不发 resolve。
	rec := newReceiver(t)
	m := newTestMonitor(t, Config{WebhookURL: rec.URL(), MinHealthyCN: 1, ForTicks: 1, ClearTicks: 1, RecoverHealthy: 1}, src)
	m.Tick(time.Now())
	src.setCN(Health{Total: 1, Healthy: 1})
	m.Tick(time.Now().Add(time.Second))
	if got := rec.count(); got != 1 {
		t.Errorf("SendResolve 默认 false 时只应发 alert，got %d 条", got)
	}

	// 开启：alert + resolve。
	rec2 := newReceiver(t)
	m2 := newTestMonitor(t, Config{
		WebhookURL: rec2.URL(), MinHealthyCN: 1, ForTicks: 1, ClearTicks: 1, RecoverHealthy: 1, SendResolve: true,
	}, src)
	src.setCN(Health{Total: 1, Healthy: 0})
	m2.Tick(time.Now())
	src.setCN(Health{Total: 1, Healthy: 1})
	m2.Tick(time.Now().Add(time.Second))
	if got := rec2.count(); got != 2 {
		t.Fatalf("SendResolve=true 应发 alert+resolve 共 2 条，got %d", got)
	}
	if p := rec2.payload(t, 1); p.Event != "resolve" {
		t.Errorf("第二条 event = %q want resolve", p.Event)
	}
}

// TestAlertBreakerAndWAFRules 熔断与 WAF 两条规则的触发/恢复。
func TestAlertBreakerAndWAFRules(t *testing.T) {
	rec := newReceiver(t)
	src := &fakeSource{
		cn:  Health{Total: 5, Healthy: 5, Breaker: 0},
		waf: false,
	}
	m := newTestMonitor(t, Config{
		WebhookURL: rec.URL(), BreakerThreshold: 2, ForTicks: 1,
	}, src)

	// 健康数充足 → 无告警。
	m.Tick(time.Now())
	if got := rec.count(); got != 0 {
		t.Fatalf("无越界时不应投递，got %d", got)
	}

	// 熔断数达阈 + WAF 激活 → 两条规则各一次。
	src.mu.Lock()
	src.cn.Breaker = 2
	src.waf = true
	src.mu.Unlock()
	m.Tick(time.Now().Add(time.Second))
	if got := rec.count(); got != 2 {
		t.Fatalf("熔断+WAF 应各触发一次共 2 条，got %d", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		seen[rec.payload(t, i).Rule] = true
	}
	if !seen["breaker_high"] || !seen["waf_ip_block"] {
		t.Errorf("应同时收到 breaker_high 与 waf_ip_block，got %v", seen)
	}
}
