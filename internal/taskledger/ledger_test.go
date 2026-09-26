package taskledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// run 造一条台账记录，字段值只需可区分即可。
func run(kind, trigger string, ok, fail int) Run {
	return Run{
		Kind: kind, Trigger: trigger,
		Started:  time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone),
		Finished: time.Date(2026, 9, 27, 9, 1, 0, 0, cstZone),
		Elapsed:  "1m0s",
		Total:    ok + fail, OK: ok, Fail: fail,
	}
}

// TestRecordKeepsLatestPerKind 台账每类任务只留最近一轮：第二次 Record 覆盖第一次，
// 不同 kind 互不干扰（/status 的 task_ledger.runs 是「最近一轮」而非历史流水）。
func TestRecordKeepsLatestPerKind(t *testing.T) {
	s := New("")
	s.Record(run("checkin", TriggerSchedule, 5, 0))
	s.Record(run("checkin", TriggerRetry, 0, 7))
	s.Record(run("travel", TriggerSchedule, 3, 1))

	got := s.Runs()
	if len(got) != 2 {
		t.Fatalf("runs 应有 2 类，got %d: %v", len(got), got)
	}
	if c := got["checkin"]; c.Trigger != TriggerRetry || c.Fail != 7 {
		t.Errorf("checkin 应保留最近一轮（retry/fail=7），got trigger=%q fail=%d", c.Trigger, c.Fail)
	}
	if tr := got["travel"]; tr.OK != 3 || tr.Fail != 1 {
		t.Errorf("travel 记录被污染：%+v", tr)
	}
}

// TestRunsReturnsCopy 返回的是副本：调用方（/status 渲染）改不动内部状态。
func TestRunsReturnsCopy(t *testing.T) {
	s := New("")
	s.Record(run("checkin", TriggerSchedule, 1, 0))
	got := s.Runs()
	got["checkin"] = run("checkin", "tampered", 99, 99)
	delete(got, "checkin")

	if again := s.Runs(); again["checkin"].OK != 1 {
		t.Errorf("内部状态被返回值污染：%+v", again)
	}
}

// TestPlanRetryArmsWithinSameDay 正常路径：排期成功、Used 递增、ArmedRetries 能取到。
func TestPlanRetryArmsWithinSameDay(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)

	d := s.PlanRetry("checkin", now, 15*time.Minute, 1)
	if d.Reason != "" {
		t.Fatalf("应排期成功，got reason=%q", d.Reason)
	}
	if want := now.Add(15 * time.Minute); !d.At.Equal(want) {
		t.Errorf("At=%s want %s", d.At, want)
	}
	if d.Used != 1 {
		t.Errorf("Used=%d want 1（排期即计数）", d.Used)
	}

	// 未到点也要能取到：它是「下一个唤醒时刻」的候选之一，只给到点的会让这次
	// 重试失去唤醒源（主循环会睡到下一个正常时点，重试迟到或与正常轮次撞车）。
	armed := s.ArmedRetries(now.Add(14 * time.Minute))
	if got, ok := armed["checkin"]; !ok || !got.Equal(d.At) {
		t.Errorf("未到点也应已排期且时刻为计划时刻，got %v", armed)
	}
	// 到点后仍是同一个计划时刻（不会漂移）。
	armed = s.ArmedRetries(now.Add(15 * time.Minute))
	if got, ok := armed["checkin"]; !ok || !got.Equal(d.At) {
		t.Errorf("到点后应保持计划时刻 %s，got %v", d.At, armed)
	}
}

// TestArmedRetriesOverdueStillArmed 计划时刻已过（进程忙/刚启动）仍应返回，
// 且返回的是**计划时刻**而非 now——上层据此算出落在过去的唤醒时刻，timer 立即到期。
func TestArmedRetriesOverdueStillArmed(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	at := now.Add(5 * time.Minute)
	if d := s.PlanRetry("cat", now, 5*time.Minute, 1); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}

	armed := s.ArmedRetries(now.Add(time.Hour))
	if got, ok := armed["cat"]; !ok || !got.Equal(at) {
		t.Errorf("已过时刻仍应已排期且返回计划时刻 %s，got %v", at, armed)
	}
}

// TestPlanRetryDisabledByDelayZero 缺省（delay=0）= 关闭，不排期且不消耗次数。
func TestPlanRetryDisabledByDelayZero(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)

	d := s.PlanRetry("checkin", now, 0, 3)
	if d.Reason != ReasonDisabled {
		t.Fatalf("delay=0 应判 disabled，got %q", d.Reason)
	}
	if d.Used != 0 {
		t.Errorf("未排期不该消耗次数，Used=%d", d.Used)
	}
	if st := s.RetryStates()["checkin"]; st.Used != 0 || st.Next != nil {
		t.Errorf("关闭时不该留下状态：%+v", st)
	}
}

// TestPlanRetryDisabledByMaxZero max=0 是「不允许重试」的合法哨兵（不回落成默认 1）。
func TestPlanRetryDisabledByMaxZero(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	if d := s.PlanRetry("checkin", now, 10*time.Minute, 0); d.Reason != ReasonDisabled {
		t.Errorf("max=0 应判 disabled，got %q", d.Reason)
	}
}

// TestPlanRetryMaxReached 当日次数用尽后拒绝，且 Used 不再增长。
func TestPlanRetryMaxReached(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)

	if d := s.PlanRetry("checkin", now, 10*time.Minute, 2); d.Reason != "" {
		t.Fatalf("第 1 次应成功：%q", d.Reason)
	}
	// 消费掉第 1 次，再排第 2 次。
	s.ConsumeRetry("checkin", now.Add(10*time.Minute))
	if d := s.PlanRetry("checkin", now.Add(20*time.Minute), 10*time.Minute, 2); d.Reason != "" {
		t.Fatalf("第 2 次应成功：%q", d.Reason)
	}
	s.ConsumeRetry("checkin", now.Add(30*time.Minute))

	d := s.PlanRetry("checkin", now.Add(40*time.Minute), 10*time.Minute, 2)
	if d.Reason != ReasonMaxReached {
		t.Errorf("第 3 次应判 max_reached，got %q", d.Reason)
	}
	if d.Used != 2 {
		t.Errorf("Used 应停在 2，got %d", d.Used)
	}
}

// TestPlanRetryRefusesNextDay 计划时刻跨到次日时不排期：跨日重试会与次日正常排程
// 撞车，且「当日失败重试」的语义止于当日。
func TestPlanRetryRefusesNextDay(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 23, 50, 0, 0, cstZone)

	d := s.PlanRetry("checkin", now, 30*time.Minute, 3)
	if d.Reason != ReasonNextDay {
		t.Fatalf("跨日应判 next_day，got %q", d.Reason)
	}
	if d.Used != 0 {
		t.Errorf("未排期不该消耗次数，Used=%d", d.Used)
	}
	// 跨日边界按 CST 算，不是按进程本地时区：UTC 下 now 是 15:50，仍是同一个 CST 日。
	if got := day(now.Add(30 * time.Minute)); got != "2026-09-28" {
		t.Fatalf("测试前提错：+30min 应跨到 09-28，got %s", got)
	}
}

// TestConsumeRetryOnceOnly 消费是一次性的：第二次返回 false（否则主循环会反复
// 拿到同一个已过时刻，形成空转）。
func TestConsumeRetryOnceOnly(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	s.PlanRetry("checkin", now, 10*time.Minute, 1)
	due := now.Add(10 * time.Minute)

	if !s.ConsumeRetry("checkin", due) {
		t.Fatal("首次应消费成功")
	}
	if s.ConsumeRetry("checkin", due) {
		t.Error("第二次不该再消费")
	}
	if len(s.ArmedRetries(due)) != 0 {
		t.Error("消费后不该再已排期")
	}
	// 消费只清 Next，不清 Used（次数已用掉，当日不该再补排）。
	if st := s.RetryStates()["checkin"]; st.Used != 1 || st.Next != nil {
		t.Errorf("消费后 Used 应保留、Next 应清空，got %+v", st)
	}
}

// TestConsumeRetryLeavesFutureArmed 未到点的重试不该被消费：正常槽位先到时，
// 那次重试必须留到它自己的时刻（消费判据比 ArmedRetries 严，两者分工不同）。
func TestConsumeRetryLeavesFutureArmed(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	s.PlanRetry("checkin", now, 30*time.Minute, 1)

	if s.ConsumeRetry("checkin", now.Add(time.Minute)) {
		t.Fatal("未到点不该被消费")
	}
	if len(s.ArmedRetries(now.Add(30*time.Minute))) != 1 {
		t.Error("未到点被消费后重试就丢了")
	}
}

// TestConsumeRetryUnknownKind 从未排过期的 kind 消费返回 false（不 panic、不建槽）。
func TestConsumeRetryUnknownKind(t *testing.T) {
	s := New("")
	if s.ConsumeRetry("never", time.Now()) {
		t.Error("未知 kind 不该消费成功")
	}
	if len(s.RetryStates()) != 0 {
		t.Errorf("不该凭空建槽：%v", s.RetryStates())
	}
}

// TestRetryCounterResetsNextDay 惰性跨日滚动：换天后 Used/Next 清零，可重新排期。
func TestRetryCounterResetsNextDay(t *testing.T) {
	s := New("")
	d1 := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	if d := s.PlanRetry("checkin", d1, 10*time.Minute, 1); d.Reason != "" {
		t.Fatalf("第 1 天应排上：%q", d.Reason)
	}

	d2 := time.Date(2026, 9, 28, 9, 0, 0, 0, cstZone)
	if len(s.ArmedRetries(d2)) != 0 {
		t.Error("跨日后昨日的待重试应失效")
	}
	st := s.RetryStates()["checkin"]
	if st.Day != "2026-09-28" || st.Used != 0 || st.Next != nil {
		t.Errorf("跨日应清零，got %+v", st)
	}
	// 新的一天次数重新可用。
	if d := s.PlanRetry("checkin", d2, 10*time.Minute, 1); d.Reason != "" || d.Used != 1 {
		t.Errorf("新的一天应可再排一次，got %+v", d)
	}
}

// TestRetryStateNextOmittedWhenNil Next 用指针就是为了让 omitempty 生效：
// time.Time 是结构体，omitempty 对它无效，零值会渲染成 "0001-01-01T00:00:00Z"。
func TestRetryStateNextOmittedWhenNil(t *testing.T) {
	raw, err := json.Marshal(RetryState{Day: "2026-09-27", Used: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "next") || strings.Contains(string(raw), "0001-01-01") {
		t.Errorf("Next 为 nil 时应省略该键，got %s", raw)
	}

	at := time.Date(2026, 9, 27, 9, 15, 0, 0, cstZone)
	raw, _ = json.Marshal(RetryState{Day: "2026-09-27", Used: 1, Next: &at})
	if !strings.Contains(string(raw), `"next":"2026-09-27T09:15:00+08:00"`) {
		t.Errorf("Next 非 nil 应写出，got %s", raw)
	}
}

// TestPersistRoundTrip 落盘后重建 Store 能读回 runs 与 retry（重启后仍可对账）。
//
// ⚠️ 断言刻意分成「Record 之后」与「PlanRetry 之后」两段，而不是最后统一检查：
// PlanRetry 也调 saveLocked()，而它落的是**整个 runs map**，所以只写一条末尾断言时
// 即使把 Record 里的 saveLocked() 删掉，记录仍会被 PlanRetry 顺手带出去、测试照过
// ——「记录即落盘」这条不变式就成了空转（变异验证实测到过）。
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "task_ledger.json")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)

	s := New(path)
	s.Record(run("checkin", TriggerSchedule, 4, 1))
	// 父目录由 saveLocked 自建（与 state_file 同约定）。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Record 后应已落盘：%v", err)
	}
	if got := New(path).Runs()["checkin"]; got.OK != 4 || got.Fail != 1 {
		t.Fatalf("Record 必须自己落盘（不能靠后续 PlanRetry 带出去），重建读到 %+v", got)
	}

	if d := s.PlanRetry("checkin", now, 20*time.Minute, 2); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应已落盘：%v", err)
	}

	again := New(path)
	got := again.Runs()["checkin"]
	if got.OK != 4 || got.Fail != 1 || got.Trigger != TriggerSchedule {
		t.Errorf("runs 未正确恢复：%+v", got)
	}
	st := again.RetryStates()["checkin"]
	if st.Used != 1 || st.Next == nil || !st.Next.Equal(now.Add(20*time.Minute)) {
		t.Errorf("retry 未正确恢复：%+v", st)
	}
}

// TestPersistEmptyPathMemoryOnly 空路径 = 纯内存：不落任何文件（老部署不该被动
// 多出一个文件）。
func TestPersistEmptyPathMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	s := New("")
	s.Record(run("checkin", TriggerSchedule, 1, 0))
	s.PlanRetry("checkin", time.Now(), 10*time.Minute, 1)

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("空路径不该产生文件：%v", ents)
	}
	if s.Runs()["checkin"].OK != 1 {
		t.Error("内存态应仍可读")
	}
}

// TestPersistCorruptFileStartsEmpty 文件损坏按空台账启动，不 panic——台账是观测，
// 不该让服务起不来（与 admin 审计的 fail-fast 刻意相反）。
func TestPersistCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("写入: %v", err)
	}
	s := New(path)
	if len(s.Runs()) != 0 {
		t.Errorf("损坏文件应按空台账启动，got %v", s.Runs())
	}
	// 仍可正常写入并覆盖坏文件。
	s.Record(run("cat", TriggerSchedule, 1, 0))
	if New(path).Runs()["cat"].OK != 1 {
		t.Error("应能覆盖坏文件并读回")
	}
}

// TestPersistFailureDoesNotPanic 落盘失败只记日志：把父路径占成普通文件让 MkdirAll
// 必然失败，Record/PlanRetry/ConsumeRetry 都不得 panic，内存态照常可用。
func TestPersistFailureDoesNotPanic(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("写入占位文件: %v", err)
	}
	path := filepath.Join(blocker, "sub", "ledger.json")

	s := New(path)
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	s.Record(run("checkin", TriggerSchedule, 2, 0))
	if d := s.PlanRetry("checkin", now, 10*time.Minute, 1); d.Reason != "" {
		t.Fatalf("落盘失败不该影响排期：%q", d.Reason)
	}
	if !s.ConsumeRetry("checkin", now.Add(10*time.Minute)) {
		t.Fatal("落盘失败不该影响消费")
	}
	if s.Runs()["checkin"].OK != 2 {
		t.Error("内存态应仍可读")
	}
	// 父路径仍是普通文件，说明确实没写进去（不是被静默创建成目录）。
	info, err := os.Stat(blocker)
	if err != nil || info.IsDir() {
		t.Fatalf("测试前提被破坏：blocker 应仍是普通文件，err=%v", err)
	}
}

// TestConcurrentRecordAndRetry 并发写台账（调度 goroutine 与主循环同时动）不 panic、
// 不丢一致性——race 检测器下跑（-race）能真正验证互斥。
func TestConcurrentRecordAndRetry(t *testing.T) {
	s := New("")
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, cstZone)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			kind := []string{"checkin", "travel", "activity"}[i%3]
			for j := 0; j < 50; j++ {
				s.Record(run(kind, TriggerSchedule, j, 0))
				s.PlanRetry(kind, now, time.Duration(j+1)*time.Minute, 100)
				s.ArmedRetries(now.Add(time.Duration(j) * time.Minute))
				s.Runs()
				s.RetryStates()
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if len(s.Runs()) != 3 {
		t.Errorf("应有 3 类记录，got %d", len(s.Runs()))
	}
}
