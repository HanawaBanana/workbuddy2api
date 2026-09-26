package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScheduleRetryDefaults 三个新键的默认值：
//   - ledger_file 空 = 不落盘（老部署不该被动多出一个文件）
//   - retry_delay_minutes 0 = 关闭当日失败重试（行为与引入前逐字一致）
//   - retry_max_per_day 1 = 只写了 delay 时的保守默认（开关关着时惰性）
func TestScheduleRetryDefaults(t *testing.T) {
	d := DefaultSchedule()
	if d.LedgerFile != "" {
		t.Errorf("LedgerFile 默认应为空（不落盘），got %q", d.LedgerFile)
	}
	if d.RetryDelayMinutes != 0 {
		t.Errorf("RetryDelayMinutes 默认应为 0（关闭），got %d", d.RetryDelayMinutes)
	}
	if d.RetryMaxPerDay != 1 {
		t.Errorf("RetryMaxPerDay 默认应为 1，got %d", d.RetryMaxPerDay)
	}
}

// TestScheduleRetryUnmarshalKeepsDefaultMax 键缺席时保留默认 1（靠「先置默认值再
// Unmarshal」实现，与 ActivityReportCount 的缺省 5 同一套路）：用户只写了 delay
// 就自动得到「每天最多补跑一次」，不必再配一次。
func TestScheduleRetryUnmarshalKeepsDefaultMax(t *testing.T) {
	s := DefaultSchedule()
	if err := json.Unmarshal([]byte(`{"retry_delay_minutes":15}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if s.RetryDelayMinutes != 15 {
		t.Errorf("RetryDelayMinutes=%d want 15", s.RetryDelayMinutes)
	}
	if s.RetryMaxPerDay != 1 {
		t.Errorf("键缺席应保留默认 1，got %d", s.RetryMaxPerDay)
	}
}

// TestScheduleRetryUnmarshalExplicitMaxZero 显式 0 = 不允许重试，是**合法哨兵**，
// Normalize 不得把它回落成 1——回落等于把用户明确关掉的能力又打开。
func TestScheduleRetryUnmarshalExplicitMaxZero(t *testing.T) {
	s := DefaultSchedule()
	if err := json.Unmarshal([]byte(`{"retry_delay_minutes":15,"retry_max_per_day":0}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if s.RetryMaxPerDay != 0 {
		t.Errorf("显式 0 应保留（哨兵值不回落），got %d", s.RetryMaxPerDay)
	}
}

// TestScheduleRetryNormalizeRejectsNegative 负值报错而非静默回落：静默回落会掩盖
// 配置笔误（写 -1 的人多半是想写别的）。
func TestScheduleRetryNormalizeRejectsNegative(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*Schedule)
		want  string
	}{
		{"delay 为负", func(s *Schedule) { s.RetryDelayMinutes = -1 }, "retry_delay_minutes"},
		{"max 为负", func(s *Schedule) { s.RetryMaxPerDay = -2 }, "retry_max_per_day"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := DefaultSchedule()
			c.patch(&s)
			err := s.Normalize()
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应含字段名 %q，got %q", c.want, err.Error())
			}
		})
	}
}

// TestScheduleRetryNormalizeRejectsDelayOverADay 延迟 >= 24h 必然跨到次日，那时
// taskledger 会判 next_day 而不排期（「当日失败重试」的语义止于当日）——配置等于
// 没写，不如启动就报错说清。
func TestScheduleRetryNormalizeRejectsDelayOverADay(t *testing.T) {
	s := DefaultSchedule()
	s.RetryDelayMinutes = 1441
	err := s.Normalize()
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "1440") {
		t.Errorf("错误信息应给出上限 1440，got %q", err.Error())
	}

	// 边界 1440 本身合法（24h 是上限而非非法值）。
	s.RetryDelayMinutes = 1440
	if err := s.Normalize(); err != nil {
		t.Errorf("1440 应合法：%v", err)
	}
}

// TestScheduleRetryNormalizeAcceptsZeroDelay 0 = 关闭是缺省语义，必须合法。
func TestScheduleRetryNormalizeAcceptsZeroDelay(t *testing.T) {
	s := DefaultSchedule()
	s.RetryDelayMinutes = 0
	if err := s.Normalize(); err != nil {
		t.Errorf("0 = 关闭，应合法：%v", err)
	}
}

// TestScheduleRetryLedgerFileNotValidated 台账落盘路径不做校验：落盘失败只记日志、
// 不影响任务执行（观测组件不该拖垮关键路径），因此不能在这里 fail-fast。
func TestScheduleRetryLedgerFileNotValidated(t *testing.T) {
	s := DefaultSchedule()
	s.LedgerFile = "/no/such/dir/ledger.json" // 故意不可写
	if err := s.Normalize(); err != nil {
		t.Errorf("台账路径不该在归一阶段校验：%v", err)
	}
}

// TestScheduleRetryKeysDoNotDisturbExisting 新键不影响既有键的默认与归一：
// 既有部署升级后行为必须逐字不变。
func TestScheduleRetryKeysDoNotDisturbExisting(t *testing.T) {
	s := DefaultSchedule()
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9 || s.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours 默认被改动：%v", s.CheckinHours)
	}
	if s.ActivityReportCount != 5 {
		t.Errorf("activity_report_count 默认被改动：%d", s.ActivityReportCount)
	}
	if s.JitterMinutes != 0 {
		t.Errorf("jitter_minutes 默认被改动：%d", s.JitterMinutes)
	}
	if !s.CheckinEnabled || !s.TravelEnabled || !s.ActivityEnabled ||
		!s.KeepaliveEnabled || !s.SchoolEnabled || !s.CatEnabled {
		t.Error("任务开关默认应全为 true")
	}
}
