package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Alerting 配置段（可用性阈值告警）的契约测试。
//
// 核心不变量：
//	1. **缺省关闭**：老 config 不含 alerting 键时 Enabled=false，行为逐字不变；
//	2. **0 是合法哨兵**（= 关闭该规则），不得被 normalize 回落成默认值——
//	    否则纯 CN 部署的 min_healthy_global=0 会变成 1 并常驻误报；
//	3. enabled=true 时 webhook_url 缺失/非法 → **启动报错**（fail-fast），
//	    不静默运行一个永远发不出去的告警器。

func loadFromJSON(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(fp)
}

// TestAlertingDisabledByDefault Default() 与空 JSON 都得到关闭态。
func TestAlertingDisabledByDefault(t *testing.T) {
	if Default().Alerting.Enabled {
		t.Error("Default() 应把 alerting.enabled 置为 false")
	}
	c, err := loadFromJSON(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerting.Enabled {
		t.Error("空 config 加载后 alerting.enabled 应为 false")
	}
	// 默认阈值：cn=1（开）、global=0（关）、熔断=0（关）。
	if c.Alerting.MinHealthyCN != 1 || c.Alerting.MinHealthyGlobal != 0 || c.Alerting.BreakerThreshold != 0 {
		t.Errorf("默认阈值 = cn:%d global:%d breaker:%d want 1/0/0",
			c.Alerting.MinHealthyCN, c.Alerting.MinHealthyGlobal, c.Alerting.BreakerThreshold)
	}
	if c.Alerting.IntervalSeconds != 30 || c.Alerting.TimeoutSeconds != 5 || c.Alerting.StartupGraceSeconds != 30 {
		t.Errorf("默认周期 = %d/%d/%d want 30/5/30",
			c.Alerting.IntervalSeconds, c.Alerting.TimeoutSeconds, c.Alerting.StartupGraceSeconds)
	}
	if c.Alerting.ForTicks != 2 || c.Alerting.ClearTicks != 2 || c.Alerting.RecoverHealthy != 1 {
		t.Errorf("默认拍数/恢复阈值 = %d/%d/%d want 2/2/1",
			c.Alerting.ForTicks, c.Alerting.ClearTicks, c.Alerting.RecoverHealthy)
	}
	if c.Alerting.SendResolve {
		t.Error("send_resolve 应默认 false")
	}
}

// TestAlertingLegacyConfigStaysOff 老 config（无 alerting 段）行为不变。
func TestAlertingLegacyConfigStaysOff(t *testing.T) {
	c, err := loadFromJSON(t, `{"listen":":7863","api_key":"k","admin":{"enabled":false},"metrics":{"enabled":false}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerting.Enabled {
		t.Error("不含 alerting 段的老 config 应保持关闭")
	}
}

// TestAlertingEnabledWithoutURLFailsFast enabled=true 但 webhook_url 为空 → 启动报错
// （与 admin.enabled + 空 api_key 同风格的 fail-fast）。
func TestAlertingEnabledWithoutURLFailsFast(t *testing.T) {
	_, err := loadFromJSON(t, `{"alerting":{"enabled":true,"webhook_url":""}}`)
	if err == nil {
		t.Fatal("alerting.enabled=true 且 webhook_url 为空应拒绝启动")
	}
	if !strings.Contains(err.Error(), "alerting") {
		t.Errorf("错误文案应点到 alerting, got %q", err.Error())
	}
}

// TestAlertingBadSchemeRejected 非法 scheme（如 file:// / 裸字符串）必须报错。
func TestAlertingBadSchemeRejected(t *testing.T) {
	for _, bad := range []string{
		"file:///etc/passwd",
		"ftp://example.com/hook",
		"not a url at all",
		"https://",
	} {
		if _, err := loadFromJSON(t, `{"alerting":{"enabled":true,"webhook_url":`+jsonQuote(bad)+`}}`); err == nil {
			t.Errorf("webhook_url=%q 应被拒绝", bad)
		}
	}
}

// TestAlertingValidURLsAccepted 合法 http/https 通过。
func TestAlertingValidURLsAccepted(t *testing.T) {
	for _, ok := range []string{
		"https://hooks.example.com/services/abc",
		"http://127.0.0.1:9099/alert",
	} {
		c, err := loadFromJSON(t, `{"alerting":{"enabled":true,"webhook_url":`+jsonQuote(ok)+`}}`)
		if err != nil {
			t.Errorf("webhook_url=%q 应通过: %v", ok, err)
			continue
		}
		if !c.Alerting.Enabled {
			t.Errorf("webhook_url=%q 时 enabled 应为 true", ok)
		}
	}
}

// TestAlertingZeroThresholdsNotDefaulted 0 是"关闭该规则"的哨兵，不得被回落成默认。
// 若有人把 min_healthy_global / breaker_threshold 也写成 "<=0 → 回落默认"，
// 纯 CN 部署会因 min_healthy_global 变 1 而常驻误报——本测试即该回归的护栏。
func TestAlertingZeroThresholdsNotDefaulted(t *testing.T) {
	c, err := loadFromJSON(t, `{"alerting":{"enabled":true,"webhook_url":"https://h.example/x","min_healthy_cn":0,"min_healthy_global":0,"breaker_threshold":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerting.MinHealthyCN != 0 || c.Alerting.MinHealthyGlobal != 0 || c.Alerting.BreakerThreshold != 0 {
		t.Errorf("0 阈值被改写成 cn:%d global:%d breaker:%d（哨兵语义丢失）",
			c.Alerting.MinHealthyCN, c.Alerting.MinHealthyGlobal, c.Alerting.BreakerThreshold)
	}
}

// TestAlertingNegativeValuesRejected 负值无合理语义，必须报错而非静默回落。
func TestAlertingNegativeValuesRejected(t *testing.T) {
	for _, key := range []string{
		"min_healthy_cn", "min_healthy_global", "breaker_threshold", "recover_healthy",
		"interval_seconds", "timeout_seconds", "startup_grace_seconds", "for_ticks", "clear_ticks",
	} {
		body := `{"alerting":{"enabled":true,"webhook_url":"https://h.example/x",` + jsonQuote(key) + `:-1}}`
		if _, err := loadFromJSON(t, body); err == nil {
			t.Errorf("alerting.%s=-1 应被拒绝", key)
		}
	}
}

// TestAlertingRecoverHealthyZeroDefaulted recover_healthy 的 0 不是哨兵（0 会让迟滞失效：
// healthy>=0 恒真 → 告警立刻解除），应回落默认 1。
func TestAlertingRecoverHealthyZeroDefaulted(t *testing.T) {
	c, err := loadFromJSON(t, `{"alerting":{"enabled":true,"webhook_url":"https://h.example/x","recover_healthy":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerting.RecoverHealthy != 1 {
		t.Errorf("recover_healthy=0 应回落默认 1，got %d", c.Alerting.RecoverHealthy)
	}
}

// TestAlertingDisabledSkipsURLValidation 关闭时不校验 URL（避免误伤含空 webhook 的
// 老配置：关掉的功能不该因无关字段拦启动）。
func TestAlertingDisabledSkipsURLValidation(t *testing.T) {
	if _, err := loadFromJSON(t, `{"alerting":{"enabled":false,"webhook_url":"garbage"}}`); err != nil {
		t.Errorf("alerting 关闭时不应校验 webhook_url: %v", err)
	}
}

// TestAlertingEnvOverride env 覆盖（含整数项与布尔项），非法值忽略。
func TestAlertingEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"alerting":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WB2A_ALERTING_ENABLED", "true")
	t.Setenv("WB2A_ALERTING_WEBHOOK_URL", "https://h.example/hook")
	t.Setenv("WB2A_ALERTING_MIN_HEALTHY_CN", "3")
	t.Setenv("WB2A_ALERTING_MIN_HEALTHY_GLOBAL", "2")
	t.Setenv("WB2A_ALERTING_BREAKER_THRESHOLD", "4")
	t.Setenv("WB2A_ALERTING_FOR_TICKS", "5")
	t.Setenv("WB2A_ALERTING_SEND_RESOLVE", "true")
	t.Setenv("WB2A_ALERTING_SECRET", "s3cr3t")

	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Alerting.Enabled {
		t.Error("WB2A_ALERTING_ENABLED=true 应开启")
	}
	if c.Alerting.WebhookURL != "https://h.example/hook" || c.Alerting.Secret != "s3cr3t" {
		t.Errorf("env 未覆盖 url/secret: %q/%q", c.Alerting.WebhookURL, c.Alerting.Secret)
	}
	if c.Alerting.MinHealthyCN != 3 || c.Alerting.MinHealthyGlobal != 2 || c.Alerting.BreakerThreshold != 4 || c.Alerting.ForTicks != 5 {
		t.Errorf("env 整数项未覆盖: %+v", c.Alerting)
	}
	if !c.Alerting.SendResolve {
		t.Error("WB2A_ALERTING_SEND_RESOLVE=true 应生效")
	}

	// 非法 env 值忽略：不报错，也不得覆盖 config。
	//
	// 注意 Load 每次都从 Default() 重新构造再叠 config + env，两次 Load 之间**没有**
	// 状态继承；因此这里必须换一份显式写死阈值的 config 作基准，否则"非法 env 被忽略"
	// 与"env 没生效"无法区分（前者 config 值胜出，后者是默认值胜出）。
	dir2 := t.TempDir()
	fp2 := filepath.Join(dir2, "c.json")
	if err := os.WriteFile(fp2, []byte(`{"alerting":{"enabled":false,"min_healthy_cn":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_ALERTING_MIN_HEALTHY_CN", "not-a-number")
	t.Setenv("WB2A_ALERTING_ENABLED", "yes-please")
	c, err = Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerting.MinHealthyCN != 7 {
		t.Errorf("非法整数 env 应被忽略（保持 config 值 7），got %d", c.Alerting.MinHealthyCN)
	}
	if c.Alerting.Enabled {
		t.Error("非法布尔 env 应被忽略（config 为 false，不得被 \"yes-please\" 打开）")
	}
}

// TestAlertingEnvEnabledWithoutURLFailsFast env 开启但无 URL 同样 fail-fast
// （env 覆盖先于 normalize 校验，两条入口一致拦截）。
func TestAlertingEnvEnabledWithoutURLFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_ALERTING_ENABLED", "1")
	if _, err := Load(fp); err == nil {
		t.Fatal("env 开启 alerting 但无 webhook_url 也应 fail-fast")
	}
}

// jsonQuote 把字符串编成 JSON 字符串字面量（测试拼 body 用）。
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
