// schedule_ledger_config_test.go schedule.ledger_file / retry_delay_minutes /
// retry_max_per_day 在真实 Load 链路（Default → JSON 覆盖 → env → normalize）上的契约。
//
// internal/config 的测试只覆盖 Normalize 本身；这里补的是配置链路末端的集成不变量：
//  1. **缺省 = 旧行为**：老 config 不含这三个键时，重试关闭、台账不落盘；
//  2. **0 是合法哨兵**：retry_max_per_day=0 表示「不允许重试」，不得被回落成 1；
//  3. env 覆盖与 config 两条入口最终状态一致，非法 env 值静默忽略（对齐 WB2A_* 风格）；
//  4. config.example.json 的取值与实现默认值一致（它同时是容器里的默认配置，
//     见 Dockerfile 的 COPY config.example.json /app/config.json）。
package main

import (
	"strings"
	"testing"
)

// TestScheduleLedgerLegacyConfigStaysOff 不含这三个键的老配置加载后：重试关闭、
// 台账纯内存、每日重试上限保留默认 1（惰性，开关关着时不生效）。
func TestScheduleLedgerLegacyConfigStaysOff(t *testing.T) {
	c, err := loadFromJSON(t, `{"listen":":7863","schedule":{"checkin_hours":[9,21]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.RetryDelayMinutes != 0 {
		t.Errorf("老配置 RetryDelayMinutes=%d want 0（缺省 = 关闭重试）", c.Schedule.RetryDelayMinutes)
	}
	if c.Schedule.LedgerFile != "" {
		t.Errorf("老配置 LedgerFile=%q want 空（不被动落盘）", c.Schedule.LedgerFile)
	}
	if c.Schedule.RetryMaxPerDay != 1 {
		t.Errorf("RetryMaxPerDay=%d want 1（保守默认，开关关着时惰性）", c.Schedule.RetryMaxPerDay)
	}
}

// TestScheduleLedgerConfigOverride 显式配置透传到 Config.Schedule。
func TestScheduleLedgerConfigOverride(t *testing.T) {
	c, err := loadFromJSON(t, `{"schedule":{"ledger_file":"./data/task_ledger.json","retry_delay_minutes":20,"retry_max_per_day":3}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.LedgerFile != "./data/task_ledger.json" {
		t.Errorf("LedgerFile=%q", c.Schedule.LedgerFile)
	}
	if c.Schedule.RetryDelayMinutes != 20 {
		t.Errorf("RetryDelayMinutes=%d want 20", c.Schedule.RetryDelayMinutes)
	}
	if c.Schedule.RetryMaxPerDay != 3 {
		t.Errorf("RetryMaxPerDay=%d want 3", c.Schedule.RetryMaxPerDay)
	}
}

// TestScheduleLedgerMaxZeroNotDefaulted 显式 retry_max_per_day=0 必须原样保留：
// 回落成 1 等于把用户明确关掉的能力又打开。
func TestScheduleLedgerMaxZeroNotDefaulted(t *testing.T) {
	c, err := loadFromJSON(t, `{"schedule":{"retry_delay_minutes":20,"retry_max_per_day":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.RetryMaxPerDay != 0 {
		t.Errorf("显式 0 应保留（哨兵值），got %d", c.Schedule.RetryMaxPerDay)
	}
}

// TestScheduleLedgerEnvOverride env 覆盖（对齐 WB2A_ADMIN_ENABLED / WB2A_BUDGET_*
// 风格：解析失败静默忽略，保持 config 值）。
func TestScheduleLedgerEnvOverride(t *testing.T) {
	fp := writeCfg(t, `{"api_key":"k","schedule":{"ledger_file":"./a.json","retry_delay_minutes":5,"retry_max_per_day":2}}`)

	t.Setenv("WB2A_SCHEDULE_LEDGER_FILE", "./b.json")
	t.Setenv("WB2A_SCHEDULE_RETRY_DELAY_MINUTES", "30")
	t.Setenv("WB2A_SCHEDULE_RETRY_MAX_PER_DAY", "4")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.LedgerFile != "./b.json" {
		t.Errorf("env 未覆盖 LedgerFile: %q", c.Schedule.LedgerFile)
	}
	if c.Schedule.RetryDelayMinutes != 30 {
		t.Errorf("env 未覆盖 RetryDelayMinutes: %d", c.Schedule.RetryDelayMinutes)
	}
	if c.Schedule.RetryMaxPerDay != 4 {
		t.Errorf("env 未覆盖 RetryMaxPerDay: %d", c.Schedule.RetryMaxPerDay)
	}

	// env 显式置 0 覆盖 config 的正值（关掉重试的逃生门）。
	t.Setenv("WB2A_SCHEDULE_RETRY_DELAY_MINUTES", "0")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.RetryDelayMinutes != 0 {
		t.Errorf("env 置 0 应覆盖 config 的 5，got %d", c.Schedule.RetryDelayMinutes)
	}

	// 非法值忽略：保持 config 值。
	t.Setenv("WB2A_SCHEDULE_RETRY_DELAY_MINUTES", "not-a-number")
	t.Setenv("WB2A_SCHEDULE_RETRY_MAX_PER_DAY", "abc")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.RetryDelayMinutes != 5 || c.Schedule.RetryMaxPerDay != 2 {
		t.Errorf("非法 env 值应被忽略（保持 config 的 5/2），got %d/%d",
			c.Schedule.RetryDelayMinutes, c.Schedule.RetryMaxPerDay)
	}
}

// TestScheduleLedgerEnvNegativeFailsFast env 给的负值同样被 normalize 拦下
// （env 覆盖先于校验，两条入口一致拦截）。
func TestScheduleLedgerEnvNegativeFailsFast(t *testing.T) {
	for _, env := range []string{"WB2A_SCHEDULE_RETRY_DELAY_MINUTES", "WB2A_SCHEDULE_RETRY_MAX_PER_DAY"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "-1")
			_, err := Load(writeCfg(t, `{"api_key":"k"}`))
			if err == nil {
				t.Fatal("env 给的负值也应 fail-fast")
			}
			if !strings.Contains(err.Error(), "schedule.retry_") {
				t.Errorf("错误文案应点到配置键，got %q", err.Error())
			}
		})
	}
}

// TestScheduleLedgerInvalidRejectedAtStartup 非法值在启动期拦截（不静默回落），
// 错误文案要能指回配置键。
func TestScheduleLedgerInvalidRejectedAtStartup(t *testing.T) {
	cases := []struct{ body, wantKey string }{
		{`{"schedule":{"retry_delay_minutes":-1}}`, "retry_delay_minutes"},
		{`{"schedule":{"retry_delay_minutes":1441}}`, "retry_delay_minutes"},
		{`{"schedule":{"retry_max_per_day":-2}}`, "retry_max_per_day"},
	}
	for _, c := range cases {
		_, err := loadFromJSON(t, c.body)
		if err == nil {
			t.Errorf("%s 应在启动期被拒绝", c.body)
			continue
		}
		if !strings.Contains(err.Error(), c.wantKey) {
			t.Errorf("%s 错误文案应点到 %s，got %q", c.body, c.wantKey, err.Error())
		}
	}
}

// TestScheduleLedgerFileNotFailFast 台账路径不可写**不该**拦启动：台账是观测，
// 落盘失败只记日志（与 admin.audit_file 的 fail-fast 刻意相反——那份是安全特性，
// 空着等于安全承诺失效）。
func TestScheduleLedgerFileNotFailFast(t *testing.T) {
	c, err := loadFromJSON(t, `{"schedule":{"ledger_file":"/no/such/dir/task_ledger.json"}}`)
	if err != nil {
		t.Fatalf("台账路径不该拦启动：%v", err)
	}
	if c.Schedule.LedgerFile != "/no/such/dir/task_ledger.json" {
		t.Errorf("路径应原样透传，got %q", c.Schedule.LedgerFile)
	}
}

// TestConfigExampleScheduleLedgerMatchesDefault 示例文件里的取值要与实现默认值一致，
// 否则「照抄示例」与「不写该键」会得到两种行为，示例就失去了参照意义。
// 它同时是容器里的默认配置（Dockerfile 把 config.example.json 拷成 /app/config.json）。
func TestConfigExampleScheduleLedgerMatchesDefault(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("示例配置应能加载：%v", err)
	}
	d := Default().Schedule
	if c.Schedule.LedgerFile != d.LedgerFile {
		t.Errorf("示例 ledger_file=%q 与默认 %q 不一致", c.Schedule.LedgerFile, d.LedgerFile)
	}
	if c.Schedule.RetryDelayMinutes != d.RetryDelayMinutes {
		t.Errorf("示例 retry_delay_minutes=%d 与默认 %d 不一致", c.Schedule.RetryDelayMinutes, d.RetryDelayMinutes)
	}
	if c.Schedule.RetryMaxPerDay != d.RetryMaxPerDay {
		t.Errorf("示例 retry_max_per_day=%d 与默认 %d 不一致", c.Schedule.RetryMaxPerDay, d.RetryMaxPerDay)
	}
}
