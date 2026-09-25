// budget_config_test.go 当日积分预算闸（budget.daily_credit_limit）的配置契约测试。
//
// 重点是把「0 是哨兵而非未设置」这条钉死：把 0 回落成某个默认上限，等于在老部署上
// 凭空开始拒请求——正是「缺省 = 旧行为」要禁止的事。
package main

import (
	"strings"
	"testing"
)

// TestBudgetDefaultsOff 缺省 0（不限）。
func TestBudgetDefaultsOff(t *testing.T) {
	c, err := Load(writeCfg(t, `{"api_key":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 0 {
		t.Errorf("budget.daily_credit_limit 应缺省 0（不限），got %v", c.Budget.DailyCreditLimit)
	}
}

// TestBudgetLegacyConfigStaysOff 老 config（不含 budget 段）加载后闸保持关闭。
func TestBudgetLegacyConfigStaysOff(t *testing.T) {
	c, err := Load(writeCfg(t, `{"api_key":"k","pool":{"breaker_threshold":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 0 {
		t.Errorf("旧 config 应保持闸关闭，got %v", c.Budget.DailyCreditLimit)
	}
}

// TestBudgetZeroIsSentinelNotDefaulted 显式写 0 与不写等价，且**不会被回落**成别的值。
func TestBudgetZeroIsSentinelNotDefaulted(t *testing.T) {
	c, err := Load(writeCfg(t, `{"api_key":"k","budget":{"daily_credit_limit":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 0 {
		t.Errorf("显式 0 必须原样保留（0 = 不限的哨兵），got %v", c.Budget.DailyCreditLimit)
	}
}

// TestBudgetExplicitValuePreserved 正数原样保留（含小数——credit 是浮点量）。
func TestBudgetExplicitValuePreserved(t *testing.T) {
	c, err := Load(writeCfg(t, `{"api_key":"k","budget":{"daily_credit_limit":123.5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 123.5 {
		t.Errorf("daily_credit_limit=%v want 123.5", c.Budget.DailyCreditLimit)
	}
}

// TestBudgetNegativeRejected 负值无合理语义（会让闸恒拒），启动即报错而不是静默回落。
func TestBudgetNegativeRejected(t *testing.T) {
	_, err := Load(writeCfg(t, `{"api_key":"k","budget":{"daily_credit_limit":-1}}`))
	if err == nil {
		t.Fatal("负的上限应拒绝启动")
	}
	if !strings.Contains(err.Error(), "daily_credit_limit") {
		t.Errorf("错误文案应点到键名, got %q", err.Error())
	}
}

// TestBudgetEnvOverride env 覆盖（对齐 WB2A_ADMIN_ENABLED 风格：解析失败静默忽略）。
func TestBudgetEnvOverride(t *testing.T) {
	fp := writeCfg(t, `{"api_key":"k"}`)

	t.Setenv("WB2A_BUDGET_DAILY_CREDIT_LIMIT", "50.25")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 50.25 {
		t.Fatalf("env 未生效: %v", c.Budget.DailyCreditLimit)
	}

	// env 显式置 0 覆盖 config 的正值（关闸逃生门）
	fp2 := writeCfg(t, `{"api_key":"k","budget":{"daily_credit_limit":99}}`)
	t.Setenv("WB2A_BUDGET_DAILY_CREDIT_LIMIT", "0")
	c, err = Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 0 {
		t.Errorf("env 置 0 应覆盖 config 的 99（逃生门），got %v", c.Budget.DailyCreditLimit)
	}

	// 非法值忽略：保持 config 值
	t.Setenv("WB2A_BUDGET_DAILY_CREDIT_LIMIT", "not-a-number")
	c, err = Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c.Budget.DailyCreditLimit != 99 {
		t.Errorf("非法 env 值应被忽略（保持 config 值 99），got %v", c.Budget.DailyCreditLimit)
	}
}

// TestBudgetEnvNegativeFailsFast env 给的负值同样被 normalize 拦下
// （env 覆盖先于校验，两条入口一致拦截）。
func TestBudgetEnvNegativeFailsFast(t *testing.T) {
	t.Setenv("WB2A_BUDGET_DAILY_CREDIT_LIMIT", "-3")
	if _, err := Load(writeCfg(t, `{"api_key":"k"}`)); err == nil {
		t.Fatal("env 给的负值也应 fail-fast")
	}
}

// TestConfigExampleBudgetMatchesDefault 示例文件里的取值要与实现默认值一致，
// 否则「照抄示例」与「不写该键」会得到两种行为，示例就失去了参照意义。
func TestConfigExampleBudgetMatchesDefault(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json 应能被 Load 解析: %v", err)
	}
	if c.Budget.DailyCreditLimit != Default().Budget.DailyCreditLimit {
		t.Errorf("示例 daily_credit_limit=%v 与默认值 %v 不一致",
			c.Budget.DailyCreditLimit, Default().Budget.DailyCreditLimit)
	}
}
