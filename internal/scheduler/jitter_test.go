// jitter_test.go 排程触发时刻抖动（schedule.jitter_minutes）的契约测试。
//
// 核心不变量：
//  1. **0 = 精确整点**（缺省即旧行为，逐字不变）；
//  2. **偏移落在 [0, N) 分钟**，且只加在"定下日期之后"的名义时点上；
//  3. **确定性**：同一任务 + 同一天 + 同一小时永远得到同一个偏移。这条是正确性
//     要求而非测试偏好——Run 主循环每轮重算 nextWake，若偏移每次现摇，某个槽位
//     触发后重算出的时刻仍可能在未来，同一小时会被反复派发（任务重复执行）；
//  4. **抖动把同小时的任务族摊开**：签到与旅行都配 9 点时不再挤在同一秒。
package scheduler

import (
	"testing"
	"time"
)

// onlyCheckin 只留签到排程，其余全关——把 nextWake 的候选收敛到单类，便于断言。
func onlyCheckin(hours []int, jitter int) *Scheduler {
	return New(Config{
		CheckinHours: hours, JitterMinutes: jitter,
		TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true,
		SchoolDisabled: true, CatDisabled: true,
	})
}

// TestJitterZeroKeepsExactHour 抖动 0（缺省）= 精确整点，与引入前逐字一致。
// 这条是 C4「缺省 = 旧行为」在排程上的体现：老配置不带 jitter_minutes 时，
// 触发时刻必须仍是 09:00:00.000。
func TestJitterZeroKeepsExactHour(t *testing.T) {
	s := onlyCheckin([]int{9}, 0)
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)
	at, _ := s.nextWake(now)
	want := time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)
	if !at.Equal(want) {
		t.Fatalf("jitter=0 应精确落在整点：got %v want %v", at, want)
	}
}

// TestJitterOffsetWithinWindow 偏移必须落在 [0, N) 分钟内，且不得越过名义小时
// （N ≤ 60 时）。同时锁住"偏移是往后退"而不是往前——往前退会让任务提前跑，
// 与"把整点负载摊开"的意图相反。
func TestJitterOffsetWithinWindow(t *testing.T) {
	const jitter = 30
	s := onlyCheckin([]int{9}, jitter)
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)
	at, _ := s.nextWake(now)

	nominal := time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)
	off := at.Sub(nominal)
	if off < 0 {
		t.Fatalf("偏移不得为负（不得提前触发）：got %v", off)
	}
	if off >= time.Duration(jitter)*time.Minute {
		t.Fatalf("偏移 %v 超出窗口 [0,%dm)", off, jitter)
	}
	if at.Hour() != 9 {
		t.Fatalf("jitter=%d（<60）不应跨出名义小时：got %v", jitter, at)
	}
	// 子分钟粒度：窗口按分钟计，偏移应落在整秒上（实现用秒取模）。
	if at.Nanosecond() != 0 {
		t.Fatalf("偏移应落在整秒：got %v", at)
	}
}

// TestJitterDeterministic 同一（任务类, 日期, 小时）多次计算必须完全一致。
// 这是**正确性**要求：见文件头第 3 条。
func TestJitterDeterministic(t *testing.T) {
	s := onlyCheckin([]int{9, 21}, 45)
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)

	first, firstKinds := s.nextWake(now)
	for i := 0; i < 50; i++ {
		at, kinds := s.nextWake(now)
		if !at.Equal(first) {
			t.Fatalf("第 %d 次算出不同时刻：%v != %v（偏移必须确定性）", i, at, first)
		}
		if len(kinds) != len(firstKinds) {
			t.Fatalf("第 %d 次任务集不同：%v != %v", i, kinds, firstKinds)
		}
	}
}

// TestJitterNoDuplicateDispatchOfSameSlot **本组最重要的测试**：在抖动后的时刻
// 重新计算 nextWake（模拟 Run 主循环触发后立刻进入下一轮），必须顺延到**次日**的
// 同一小时，而不是把同一个槽位再算一次。
//
// 若偏移改成每次现摇随机数，这里会得到一个仍在未来的时刻 → 同一小时被重复派发
// → 签到/上报对上游重复执行。这就是"抖动必须确定性"的实证。
func TestJitterNoDuplicateDispatchOfSameSlot(t *testing.T) {
	const jitter = 30
	s := onlyCheckin([]int{9}, jitter)
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)

	at, _ := s.nextWake(now)
	if at.Day() != 25 {
		t.Fatalf("首次应在 9/25：got %v", at)
	}

	// Run 主循环：醒来后（现在 == at）立刻重算下一轮。
	next, _ := s.nextWake(at)
	if next.Day() == 25 {
		t.Fatalf("同一槽位被重复派发：在 %v 触发后重算出 %v（应顺延到 9/26）", at, next)
	}
	if want := 26; next.Day() != want {
		t.Fatalf("应顺延到 9/26：got %v", next)
	}
	// 次日那一刻也必须是同一小时的抖动后时刻（而不是被挤到别的钟点）。
	if next.Hour() != 9 {
		t.Fatalf("次日应在 9 点档：got %v", next)
	}
}

// TestJitterBoundaryStillTodayWhenNotYetDue 名义整点已过但"抖动后时刻"还没到，
// 仍算今天——否则会白跳过一整个槽位。
func TestJitterBoundaryStillTodayWhenNotYetDue(t *testing.T) {
	const jitter = 60
	// 先取到今天的抖动后时刻，再取一个刚好在它之前 1 秒的 now。
	base := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)
	s := onlyCheckin([]int{9}, jitter)
	due, _ := s.nextWake(base)
	if !due.After(time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)) {
		t.Skip("该日偏移为 0，边界无意义")
	}

	before := due.Add(-time.Second)
	at, _ := s.nextWake(before)
	if !at.Equal(due) {
		t.Fatalf("到期前 1 秒应仍算出同一时刻：got %v want %v", at, due)
	}
	// 到期瞬间（now == due）必须顺延，不能重复派发。
	after, _ := s.nextWake(due)
	if after.Day() == 25 {
		t.Fatalf("到期瞬间应顺延到次日：got %v", after)
	}
}

// TestJitterDiffersByTaskKind 任务类参与散列：签到与旅行都配 9 点时，
// 抖动后不应再挤在同一秒（这正是"摊开负载"的目的）。
func TestJitterDiffersByTaskKind(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9}, TravelHours: []int{9},
		JitterMinutes:    30,
		ActivityDisabled: true, KeepaliveDisabled: true,
		SchoolDisabled: true, CatDisabled: true,
	})
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)

	checkin := nextFire(now, s.cfg.CheckinHours, taskCheckin, s.cfg.JitterMinutes, "")
	travel := nextFire(now, s.cfg.TravelHours, taskTravel, s.cfg.JitterMinutes, "")
	if checkin.Equal(travel) {
		t.Fatalf("不同任务类不应得到同一偏移（否则没起到摊开作用）：都是 %v", checkin)
	}
	// 且两者都仍在 9 点档内（窗口 30 分钟 < 60）。
	for _, at := range []time.Time{checkin, travel} {
		if at.Hour() != 9 {
			t.Fatalf("应仍在 9 点档：got %v", at)
		}
	}
}

// TestJitterVariesByDay 同一天同一小时固定，换一天应变化——否则所有部署在
// 不同日期会累积成同一条"错开的整点"规律，摊开效果退化。
func TestJitterVariesByDay(t *testing.T) {
	const jitter = 60
	// 取 40 天里签到(9点)的偏移集合，应当出现多于一种取值。
	seen := map[time.Duration]bool{}
	for d := 0; d < 40; d++ {
		day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local).AddDate(0, 0, d)
		at := nextFire(day, []int{9}, taskCheckin, jitter, "")
		nominal := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, time.Local)
		seen[at.Sub(nominal)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("40 天只得到 %d 种偏移，抖动应随日期变化：%v", len(seen), seen)
	}
}

// TestJitterOffsetHelperEdges jitterOffset 的边界：0/负 = 0；正值落在 [0,N) 分钟。
func TestJitterOffsetHelperEdges(t *testing.T) {
	nominal := time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)
	if got := jitterOffset(taskCheckin, nominal, 0, ""); got != 0 {
		t.Fatalf("jitterMinutes=0 应为 0，got %v", got)
	}
	if got := jitterOffset(taskCheckin, nominal, -5, ""); got != 0 {
		t.Fatalf("负窗口应为 0（不 panic），got %v", got)
	}
	for _, n := range []int{1, 15, 60, 1440} {
		got := jitterOffset(taskCheckin, nominal, n, "")
		if got < 0 || got >= time.Duration(n)*time.Minute {
			t.Fatalf("jitterOffset(n=%d)=%v 超出 [0,%dm)", n, got, n)
		}
	}
}

// TestJitterSaltSeparatesDeployments 实例盐把「同配置的不同部署」错开。
//
// 背景：偏移种子原本只有「任务类 + 名义时点」，不含任何实例身份——于是所有部署在
// 同一任务/同一天/同一小时会算出**同一个**偏移，整点齐发只是被平移成一个固定的
// 新齐发点，对"摊开全网负载"没有效果。盐为空串时保持逐字兼容；非空时各部署互不相同。
func TestJitterSaltSeparatesDeployments(t *testing.T) {
	nominal := time.Date(2026, 9, 26, 9, 0, 0, 0, time.Local)

	// 确定性前提：同一输入（含空盐）必须稳定。
	if a, b := jitterOffset(taskCheckin, nominal, 10, ""), jitterOffset(taskCheckin, nominal, 10, ""); a != b {
		t.Fatalf("确定性前提不成立：%v != %v", a, b)
	}

	seen := map[time.Duration]string{}
	for _, salt := range []string{"", "deploy-alpha", "deploy-bravo", "deploy-charlie"} {
		got := jitterOffset(taskCheckin, nominal, 10, salt)
		if got < 0 || got >= 10*time.Minute {
			t.Fatalf("salt=%q 偏移 %v 越界 [0,10m)", salt, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("salt=%q 与 %q 算到同一偏移 %v（盐未生效）", salt, prev, got)
		}
		seen[got] = salt
	}

	// 换一天偏移随之改变（与既有确定性语义一致；极小概率撞同值，仅记录）。
	if jitterOffset(taskCheckin, nominal, 10, "deploy-alpha") ==
		jitterOffset(taskCheckin, nominal.AddDate(0, 0, 1), 10, "deploy-alpha") {
		t.Log("同盐跨日落在同一偏移（1/600 概率，非失败）")
	}
}
