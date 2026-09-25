package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Metrics 配置段（Prometheus /metrics 开关）的契约测试。
//
// 核心不变量：**缺省关闭**。老 config 不含 metrics 键时必须加载为 false，从而
// 路由不注册、行为与改动前逐字不变（项目纪律：新能力默认保持原行为）。

// TestMetricsDisabledByDefault Default() 显式置 false（不依赖零值巧合），
// 且空 JSON 加载后仍为 false。
func TestMetricsDisabledByDefault(t *testing.T) {
	if Default().Metrics.Enabled {
		t.Error("Default() 应把 metrics.enabled 置为 false")
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Metrics.Enabled {
		t.Error("空 config 加载后 metrics.enabled 应为 false")
	}
}

// TestMetricsLegacyConfigStaysOff 老 config（改动前格式，无 metrics 段）行为不变：
// 显式列出现有键的老配置加载后 metrics 仍为 false。
func TestMetricsLegacyConfigStaysOff(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":7863","api_key":"k","admin":{"enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Metrics.Enabled {
		t.Error("不含 metrics 段的老 config 应保持关闭")
	}
}

// TestMetricsConfigOverride 显式开启生效。
func TestMetricsConfigOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"metrics":{"enabled":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Metrics.Enabled {
		t.Error(`metrics.enabled=true 应开启`)
	}
}

// TestMetricsEnabledNeedsNoApiKey 与 admin 的关键语义差异：/metrics 是**只读**
// 数据端点（与 /status、/v1/stats 同类），开启它不构成 mutation 风险，因此
// 不要求 api_key —— 空 key 时和 /status 一样退化为不鉴权，由运维自行评估暴露面。
// 若有人照抄 admin 的 fail-fast，本测试会失败。
func TestMetricsEnabledNeedsNoApiKey(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"","metrics":{"enabled":true}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("metrics 开启 + 空 api_key 应通过（只读端点，不同于 admin 的 fail-fast）: %v", err)
	}
}

// TestMetricsEnvOverride WB2A_METRICS_ENABLED 覆盖（ParseBool，非法值忽略，
// 风格对齐 WB2A_ADMIN_ENABLED / WB2A_PASSTHROUGH_IP）。
func TestMetricsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"metrics":{"enabled":false}}`), 0o600)

	t.Setenv("WB2A_METRICS_ENABLED", "true")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Metrics.Enabled {
		t.Error("WB2A_METRICS_ENABLED=true 应开启")
	}

	// env 关闭覆盖 config 的 true
	os.WriteFile(fp, []byte(`{"metrics":{"enabled":true}}`), 0o600)
	t.Setenv("WB2A_METRICS_ENABLED", "false")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Metrics.Enabled {
		t.Error("WB2A_METRICS_ENABLED=false 应覆盖 config 的 true")
	}

	// 非法值忽略：config 的 true 保持
	t.Setenv("WB2A_METRICS_ENABLED", "yes-please")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Metrics.Enabled {
		t.Error("非法 env 值应被忽略（保持 config 值）")
	}
}
