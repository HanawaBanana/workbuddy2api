// schedule_jitter_test.go 排程抖动配置（schedule.jitter_minutes）的契约测试。
//
// 核心不变量：
//  1. **缺省 0**：不带该键的老配置归一后仍是 0（= 精确整点 = 旧行为逐字不变）；
//  2. **0 不被改写**：0 是"关闭抖动"的合法值，不是"未设置"——不得回落成别的数；
//  3. **负值报错**（无合理语义），**超上限报错**（偏移必须 < 24h，否则当天那次任务
//     会被整体推到次日）。
package config

import (
	"strings"
	"testing"
)

// TestJitterDefaultsToZero 缺省即 0：DefaultSchedule 与空 Schedule 归一后都是 0。
func TestJitterDefaultsToZero(t *testing.T) {
	if got := DefaultSchedule().JitterMinutes; got != 0 {
		t.Errorf("DefaultSchedule().JitterMinutes=%d want 0（缺省 = 精确整点）", got)
	}
	var s Schedule
	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize 空段不应报错: %v", err)
	}
	if s.JitterMinutes != 0 {
		t.Errorf("空 schedule 归一后 JitterMinutes=%d want 0", s.JitterMinutes)
	}
}

// TestJitterZeroNotOverwritten 0 是"关闭"的哨兵，不是"未设置"。
// 若有人按"<=0 回落默认"的惯性写，这条会挂——那会把显式关闭抖动的人
// 悄悄打开一个非 0 窗口。
func TestJitterZeroNotOverwritten(t *testing.T) {
	s := DefaultSchedule()
	s.JitterMinutes = 0
	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize 不应报错: %v", err)
	}
	if s.JitterMinutes != 0 {
		t.Errorf("显式 0 被改写成 %d（0 = 关闭，不得回落）", s.JitterMinutes)
	}
}

// TestJitterExplicitValuePreserved 显式正值原样保留。
func TestJitterExplicitValuePreserved(t *testing.T) {
	s := DefaultSchedule()
	s.JitterMinutes = 15
	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize 不应报错: %v", err)
	}
	if s.JitterMinutes != 15 {
		t.Errorf("JitterMinutes=%d want 15（显式值必须保留）", s.JitterMinutes)
	}
}

// TestJitterNegativeRejected 负值无合理语义，必须报错而非静默回落。
func TestJitterNegativeRejected(t *testing.T) {
	s := DefaultSchedule()
	s.JitterMinutes = -1
	err := s.Normalize()
	if err == nil {
		t.Fatal("jitter_minutes=-1 应被拒绝")
	}
	if !strings.Contains(err.Error(), "jitter_minutes") {
		t.Errorf("错误文案应点到 jitter_minutes，got %q", err.Error())
	}
}

// TestJitterUpperBound 上限 1440（一天）：偏移必须 < 24h，否则触发时刻会漂到
// 名义时点之后一整天，当天那次实际上被跳过。
func TestJitterUpperBound(t *testing.T) {
	s := DefaultSchedule()
	s.JitterMinutes = 1440
	if err := s.Normalize(); err != nil {
		t.Errorf("1440 应被接受（上限内）: %v", err)
	}
	if s.JitterMinutes != 1440 {
		t.Errorf("JitterMinutes=%d want 1440", s.JitterMinutes)
	}

	s2 := DefaultSchedule()
	s2.JitterMinutes = 1441
	err := s2.Normalize()
	if err == nil {
		t.Fatal("jitter_minutes=1441 应被拒绝（超上限）")
	}
	if !strings.Contains(err.Error(), "jitter_minutes") {
		t.Errorf("错误文案应点到 jitter_minutes，got %q", err.Error())
	}
}
