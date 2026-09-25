// schedule_jitter_config_test.go schedule.jitter_minutes 在真实 Load 链路（Default →
// JSON 覆盖 → env → normalize）上的契约。
//
// internal/config 的测试只覆盖 Normalize 本身；这里补的是「老 config 不带该键时
// 启动后确实是 0」这条集成不变量——即 C4「缺省 = 旧行为」在配置链路末端仍成立。
package main

import (
	"strings"
	"testing"
)

// TestScheduleJitterLegacyConfigStaysZero 不含 jitter_minutes 的老配置加载后为 0
// （精确整点，与引入前逐字一致）。
func TestScheduleJitterLegacyConfigStaysZero(t *testing.T) {
	c, err := loadFromJSON(t, `{"listen":":7863","schedule":{"checkin_hours":[9,21]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.JitterMinutes != 0 {
		t.Errorf("老配置 JitterMinutes=%d want 0（缺省 = 精确整点）", c.Schedule.JitterMinutes)
	}
}

// TestScheduleJitterConfigOverride 显式配置透传到 Config.Schedule。
func TestScheduleJitterConfigOverride(t *testing.T) {
	c, err := loadFromJSON(t, `{"schedule":{"jitter_minutes":15}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.JitterMinutes != 15 {
		t.Errorf("JitterMinutes=%d want 15", c.Schedule.JitterMinutes)
	}
}

// TestScheduleJitterInvalidRejectedAtStartup 非法值在启动期拦截（不静默回落），
// 错误文案要能指回配置键。
func TestScheduleJitterInvalidRejectedAtStartup(t *testing.T) {
	for _, bad := range []string{`-1`, `1441`} {
		_, err := loadFromJSON(t, `{"schedule":{"jitter_minutes":`+bad+`}}`)
		if err == nil {
			t.Errorf("jitter_minutes=%s 应在启动期被拒绝", bad)
			continue
		}
		if !strings.Contains(err.Error(), "jitter_minutes") {
			t.Errorf("jitter_minutes=%s 错误文案应点到该键，got %q", bad, err.Error())
		}
	}
}
