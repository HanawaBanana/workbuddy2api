package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskledger"
	"workbuddy2api/internal/upstream"
)

// ledgerScheduler 构造带内存台账、空池的调度器（六类任务全启用、默认时点）。
func ledgerScheduler(cfg Config) *Scheduler {
	if cfg.Pool == nil {
		cfg.Pool = pool.New("")
	}
	return New(cfg)
}

// fixedNow 测试用的固定时刻（CST 域，与 travelDay/taskledger.day 同源）。
func fixedNow(h, m int) time.Time {
	return time.Date(2026, 9, 27, h, m, 0, 0, cstZone)
}

// TestAllFailedJudgement 全灭判据：**有失败，且没有任何账号做成**（ok+already==0）。
//
// 为什么不是失败率阈值：只要还有账号成功就说明上游是通的，失败是账号级的
// （token 过期、单号被限流），重试只是对同一批注定失败的号再打一遍上游。
func TestAllFailedJudgement(t *testing.T) {
	cases := []struct {
		name string
		t    runTally
		want bool
	}{
		{"全挂：只有失败", runTally{total: 5, fail: 5}, true},
		{"全挂但部分号被跳过", runTally{total: 3, fail: 3, skipped: 2}, true},
		{"部分成功 → 上游是通的，不算全灭", runTally{total: 5, ok: 2, fail: 3}, false},
		{"幂等成功也算做成（今天已签到）", runTally{total: 5, already: 5}, false},
		{"幂等成功 + 失败 → 不算全灭", runTally{total: 5, already: 1, fail: 4}, false},
		{"无失败（全跳过）→ 不是全灭", runTally{total: 5, skipped: 5}, false},
		{"空池 → 不是全灭", runTally{}, false},
		{"被停机打断 → 计数是部分的，不判全灭", runTally{total: 1, fail: 1, interrupted: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := ledgerScheduler(Config{})
			r := s.recordRun(taskCheckin, triggerSchedule, fixedNow(9, 0), c.t)
			if r.AllFailed != c.want {
				t.Errorf("AllFailed=%v want %v（tally=%+v）", r.AllFailed, c.want, c.t)
			}
		})
	}
}

// TestInterruptedRunNote 被 ctx 取消打断时台账要写明原因：否则「只跑了 3 个号」
// 与「跑了 300 个号」在 /status 上长得一样，运维会误判规模。
func TestInterruptedRunNote(t *testing.T) {
	s := ledgerScheduler(Config{})
	r := s.recordRun(taskTravel, triggerSchedule, fixedNow(9, 0), runTally{total: 3, ok: 3, interrupted: true})
	if r.Note != "interrupted" {
		t.Errorf("Note=%q want \"interrupted\"", r.Note)
	}
	if r.AllFailed {
		t.Error("被打断的轮次不该判全灭")
	}
}

// TestRecordRunManualNeverArmsRetry 人工触发不排重试：运维刚点过一次，自己会判断
// 要不要再点；自动重跑会让一次人工操作静默变成两次上游写。
func TestRecordRunManualNeverArmsRetry(t *testing.T) {
	s := ledgerScheduler(Config{RetryDelayMinutes: 15, RetryMaxPerDay: 1})
	now := fixedNow(9, 0)

	s.recordRun(taskCheckin, triggerManual, now, runTally{total: 3, fail: 3})
	if armed := s.Ledger().ArmedRetries(now); len(armed) != 0 {
		t.Errorf("人工触发的全灭不该排重试，got %v", armed)
	}

	// 同一份计数换成定时轮次 → 排上。重试基准是**本轮结束时刻**（实际跑完那一刻），
	// 不是入参里的 started——「延迟 N 分钟后再跑」针对的是跑完，不是开始。
	s.recordRun(taskCheckin, triggerSchedule, now, runTally{total: 3, fail: 3})
	r := s.Ledger().Runs()["checkin"]
	armed := s.Ledger().ArmedRetries(r.Finished)
	at, ok := armed["checkin"]
	if !ok {
		t.Fatalf("定时轮次全灭应排重试，got %v", armed)
	}
	if want := r.Finished.Add(15 * time.Minute); !at.Equal(want) {
		t.Errorf("重试时刻=%s want %s（相对本轮结束时刻）", at, want)
	}
}

// TestRecordRunNoRetryWhenNotAllFailed 部分成功不排重试（成功路径零新增上游请求）。
func TestRecordRunNoRetryWhenNotAllFailed(t *testing.T) {
	s := ledgerScheduler(Config{RetryDelayMinutes: 15, RetryMaxPerDay: 3})
	now := fixedNow(9, 0)

	s.recordRun(taskActivity, triggerSchedule, now, runTally{total: 10, ok: 1, fail: 9})
	if armed := s.Ledger().ArmedRetries(now); len(armed) != 0 {
		t.Errorf("部分成功不该排重试，got %v", armed)
	}
}

// TestRecordRunRetryDisabledByDefault 缺省（不配 retry_delay_minutes）时全灭也只记
// 台账、不排重试——「缺省 = 旧行为」。
func TestRecordRunRetryDisabledByDefault(t *testing.T) {
	s := ledgerScheduler(Config{})
	now := fixedNow(9, 0)
	r := s.recordRun(taskCheckin, triggerSchedule, now, runTally{total: 3, fail: 3})
	if !r.AllFailed {
		t.Error("台账仍应记下全灭（观测与重试是两件事）")
	}
	if armed := s.Ledger().ArmedRetries(now); len(armed) != 0 {
		t.Errorf("重试默认关闭，不该排期，got %v", armed)
	}
}

// TestNextWakeIncludesArmedRetryBeforeNextSlot 已排期的重试必须参与「下一个唤醒
// 时刻」的竞争：只算正常时点会让重试一路等到下一个整点（迟到 25 分钟）。
func TestNextWakeIncludesArmedRetryBeforeNextSlot(t *testing.T) {
	s := ledgerScheduler(Config{RetryDelayMinutes: 5, RetryMaxPerDay: 1})
	now := fixedNow(9, 30) // 默认下一个正常时点是 10:00（活跃上报）
	if d := s.Ledger().PlanRetry("checkin", now, 5*time.Minute, 1); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}

	at, kinds := s.nextWake(now)
	if want := now.Add(5 * time.Minute); !at.Equal(want) {
		t.Errorf("唤醒时刻=%s want %s（重试应早于 10:00 的正常槽位）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeDedupesRetryCoincidingWithSlot 重试恰好排在某个正常整点上时，
// 同一类任务只能派发一次——不去重会让 runBatch 对同一任务并行跑两遍，
// 同一批账号被同时打两次上游。
func TestNextWakeDedupesRetryCoincidingWithSlot(t *testing.T) {
	s := ledgerScheduler(Config{RetryDelayMinutes: 15, RetryMaxPerDay: 1})
	now := fixedNow(9, 45) // 活跃上报的正常槽位在 10:00
	if d := s.Ledger().PlanRetry("activity", now, 15*time.Minute, 1); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}

	at, kinds := s.nextWake(now)
	if want := fixedNow(10, 0); !at.Equal(want) {
		t.Fatalf("唤醒时刻=%s want %s", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Errorf("kinds=%v want 恰好一个 [activity]（重试与正常槽位重合时去重）", kinds)
	}
}

// TestNextWakeSkipsDisabledKindRetry 已禁用的任务不因一次残留重试被拉起来。
func TestNextWakeSkipsDisabledKindRetry(t *testing.T) {
	s := ledgerScheduler(Config{CheckinDisabled: true, RetryDelayMinutes: 5, RetryMaxPerDay: 1})
	now := fixedNow(9, 30)
	if d := s.Ledger().PlanRetry("checkin", now, 5*time.Minute, 1); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}

	at, kinds := s.nextWake(now)
	if want := fixedNow(10, 0); !at.Equal(want) {
		t.Errorf("唤醒时刻=%s want %s（禁用任务的残留重试不该成为唤醒源）", at, want)
	}
	for _, k := range kinds {
		if k == taskCheckin {
			t.Errorf("kinds=%v 不该含已禁用的 checkin", kinds)
		}
	}
}

// TestTakeRetryTrigger 消费判据：只吃到点的重试。未到点就消费会让那次重试静默消失。
func TestTakeRetryTrigger(t *testing.T) {
	s := ledgerScheduler(Config{RetryDelayMinutes: 10, RetryMaxPerDay: 2})
	now := fixedNow(9, 0)
	if d := s.Ledger().PlanRetry("checkin", now, 10*time.Minute, 2); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}

	if s.takeRetryTrigger(taskCheckin, now.Add(time.Minute)) {
		t.Error("未到点不该被消费")
	}
	if !s.takeRetryTrigger(taskCheckin, now.Add(10*time.Minute)) {
		t.Error("到点应消费成功")
	}
	if s.takeRetryTrigger(taskCheckin, now.Add(11*time.Minute)) {
		t.Error("消费是一次性的，不该重复消费")
	}
	// 其它任务不受影响（消费按 kind 隔离）。
	if s.takeRetryTrigger(taskTravel, now.Add(10*time.Minute)) {
		t.Error("不该消费到其它 kind 的重试")
	}
}

// TestDispatchMarksRetryTrigger 端到端：一次已到点的重试经 dispatch 跑完后，
// 台账里的 trigger 是 retry 而不是 schedule（运维要能分辨「这是补跑」）。
func TestDispatchMarksRetryTrigger(t *testing.T) {
	installFakeExec(t) // 脚本类任务不真拉 python3，Run() 返回 nil
	s := ledgerScheduler(Config{RetryDelayMinutes: 15, RetryMaxPerDay: 2})
	now := time.Now()
	if d := s.Ledger().PlanRetry("school", now, time.Millisecond, 2); d.Reason != "" {
		t.Fatalf("排期失败：%q", d.Reason)
	}
	time.Sleep(5 * time.Millisecond) // 让计划时刻成为「已到点」

	s.dispatch(context.Background(), taskSchool)

	r := s.Ledger().Runs()["school"]
	if r.Trigger != triggerRetry {
		t.Errorf("Trigger=%q want %q", r.Trigger, triggerRetry)
	}
	if r.Total != 1 || r.OK != 1 || r.Fail != 0 {
		t.Errorf("脚本成功应记 total=1 ok=1 fail=0，got %+v", r)
	}
	// 已消费：不该再留着（否则主循环会反复拿到同一个已过时刻空转）。
	if armed := s.Ledger().ArmedRetries(time.Now()); len(armed) != 0 {
		t.Errorf("重试应已消费，got %v", armed)
	}
}

// TestRunSchoolFailureRecordsNoteAndArmsRetry 脚本失败：退出码非 0 即算失败，
// 记下失败摘要，并在定时轮次里排重试。
func TestRunSchoolFailureRecordsNoteAndArmsRetry(t *testing.T) {
	f := installFakeExec(t)
	f.err = errors.New("exit status 1")
	s := ledgerScheduler(Config{RetryDelayMinutes: 15, RetryMaxPerDay: 1})

	s.runSchool(triggerSchedule)

	r := s.Ledger().Runs()["school"]
	if r.Total != 1 || r.Fail != 1 || r.OK != 0 {
		t.Errorf("失败应记 total=1 fail=1 ok=0，got %+v", r)
	}
	if !r.AllFailed {
		t.Error("脚本全挂应判全灭")
	}
	if r.Note == "" {
		t.Error("应记下失败摘要（note），否则 /status 只显示「失败 1」无从排查")
	}
	if len(s.Ledger().ArmedRetries(time.Now())) != 1 {
		t.Error("定时轮次的脚本失败应排重试")
	}
}

// TestRunCheckinEmptyPoolRecordsZero 空池是合法状态：记一条 total=0 的记录
// （「跑过了、没有账号」），而不是不记——否则 /status 分不清「没跑」与「没账号」。
func TestRunCheckinEmptyPoolRecordsZero(t *testing.T) {
	s := ledgerScheduler(Config{Upstream: &upstream.Client{}})
	s.RunCheckinNow()

	r := s.Ledger().Runs()["checkin"]
	if r.Trigger != triggerManual {
		t.Errorf("Trigger=%q want %q", r.Trigger, triggerManual)
	}
	if r.Total != 0 || r.Fail != 0 || r.AllFailed {
		t.Errorf("空池应记 total=0 且不判全灭，got %+v", r)
	}
}

// TestRunTravelAllFailedArmsRetry 上游整体不可达（所有账号都报错）→ 全灭 → 排重试。
// 这正是重试存在的理由：一个都没成几乎只能是网络/上游抖动/WAF，而非账号级问题。
func TestRunTravelAllFailedArmsRetry(t *testing.T) {
	fastTravel(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, RetryDelayMinutes: 10, RetryMaxPerDay: 1})

	s.runTravel(context.Background(), triggerSchedule)

	r := s.Ledger().Runs()["travel"]
	if r.Total != 2 || r.Fail != 2 || r.OK != 0 {
		t.Fatalf("两个号都该记失败，got %+v", r)
	}
	if !r.AllFailed {
		t.Error("全号失败应判全灭")
	}
	if len(s.Ledger().ArmedRetries(time.Now())) != 1 {
		t.Error("全灭应排重试")
	}
}

// TestRunTravelSkipsAreNotFailures 在途/已达上限/门槛未达都是**预期正常态**：
// 算成失败会让台账天天报全灭并触发毫无意义的当日重试。
func TestRunTravelSkipsAreNotFailures(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"档案喵"}`, state: `{"state":"traveling","record_id":7}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.runTravel(context.Background(), triggerSchedule)

	r := s.Ledger().Runs()["travel"]
	if r.Total != 1 || r.Skipped != 1 || r.Fail != 0 {
		t.Fatalf("在途应记 skipped=1 fail=0，got %+v", r)
	}
	if r.AllFailed {
		t.Error("全跳过不该判全灭")
	}
}

// TestRunKeepaliveRecordsOutcome 保活计数：刷新成功记 OK，刷新报错记 Fail。
func TestRunKeepaliveRecordsOutcome(t *testing.T) {
	t.Run("成功", func(t *testing.T) {
		stub := &travelStub{}
		srv := billingAndGrowthServer(stub)
		defer srv.Close()
		s, _ := newTravelScheduler(t, srv, "u1", "u2")

		s.runKeepalive(triggerSchedule)

		r := s.Ledger().Runs()["keepalive"]
		if r.Total != 2 || r.OK != 2 || r.Fail != 0 {
			t.Errorf("两个号刷新成功应记 total=2 ok=2，got %+v", r)
		}
	})

	t.Run("失败", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()

		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
		up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
		s := New(Config{Pool: p, Upstream: up, RetryDelayMinutes: 10, RetryMaxPerDay: 1})

		s.runKeepalive(triggerSchedule)

		r := s.Ledger().Runs()["keepalive"]
		if r.Total != 1 || r.Fail != 1 || r.OK != 0 {
			t.Fatalf("刷新失败应记 total=1 fail=1，got %+v", r)
		}
		if !r.AllFailed || len(s.Ledger().ArmedRetries(time.Now())) != 1 {
			t.Error("全号刷新失败应判全灭并排重试")
		}
	})
}

// TestRunKeepaliveSkipsDisabledAndCredentialLess 禁用号与无 refresh token 的号
// 记 skipped，不计入 total（它们没有真正打上游）。
func TestRunKeepaliveSkipsDisabledAndCredentialLess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"}) // 无 refresh token
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u3", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.runKeepalive(triggerSchedule)

	r := s.Ledger().Runs()["keepalive"]
	if r.Total != 1 || r.Skipped != 2 || r.Fail != 1 {
		t.Errorf("应只对 u1 计 total=1、其余 skipped=2，got %+v", r)
	}
}

// TestLedgerNilTolerant 手搓的零值 Scheduler 没有台账：记台账与重试都是空操作，
// 不得 panic（调度 goroutine 里 panic 会带走整个进程）。
//
// 用 New 之后把 ledger 置 nil 来模拟「手搓」：零值 Config 连排程小时都没有，
// 那样 nextWake 恒返回零时点，测不出「台账缺席不该影响排程」这一点。
func TestLedgerNilTolerant(t *testing.T) {
	s := New(Config{})
	s.ledger = nil

	if _, kinds := s.nextWake(fixedNow(9, 0)); len(kinds) == 0 {
		t.Error("无台账时正常时点仍应算出（不能因为台账缺席就停摆）")
	}
	if s.takeRetryTrigger(taskCheckin, time.Now()) {
		t.Error("无台账时不该报告「这是一次重试」")
	}
	r := s.recordRun(taskCheckin, triggerSchedule, fixedNow(9, 0), runTally{total: 1, fail: 1})
	if r.Kind != "checkin" || !r.AllFailed {
		t.Errorf("无台账时仍应返回判定结果，got %+v", r)
	}
}

// TestLedgerPersistsAcrossSchedulers 落盘路径给了就跨重启可对账——服务自动更新
// 重启后，此前只能看到一截日志。
func TestLedgerPersistsAcrossSchedulers(t *testing.T) {
	installFakeExec(t)
	path := t.TempDir() + "/task_ledger.json"
	cfg := Config{LedgerFile: path, RetryDelayMinutes: 15, RetryMaxPerDay: 2}

	s1 := ledgerScheduler(cfg)
	s1.runSchool(triggerSchedule)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("台账应已落盘：%v", err)
	}

	s2 := ledgerScheduler(cfg)
	got := s2.Ledger().Runs()["school"]
	if got.OK != 1 || got.Trigger != triggerSchedule {
		t.Errorf("重启后应读回上一轮台账，got %+v", got)
	}
	// 重试排期也应恢复（否则重启会白送一次补跑额度）。
	if st := s2.Ledger().RetryStates()["school"]; st.Used != 0 || st.Next != nil {
		t.Logf("school 本轮成功、无重试，retry 状态：%+v", st)
	}
}

// TestLedgerRetryStatePersisted 重试计数跨重启保留：否则频繁重启会反复补跑。
func TestLedgerRetryStatePersisted(t *testing.T) {
	path := t.TempDir() + "/task_ledger.json"
	cfg := Config{LedgerFile: path, RetryDelayMinutes: 15, RetryMaxPerDay: 1}

	s1 := ledgerScheduler(cfg)
	now := time.Now()
	s1.recordRun(taskCheckin, triggerSchedule, now, runTally{total: 2, fail: 2})
	if len(s1.Ledger().ArmedRetries(now)) != 1 {
		t.Fatal("测试前提：应已排一次重试")
	}

	s2 := ledgerScheduler(cfg)
	if len(s2.Ledger().ArmedRetries(now)) != 1 {
		t.Error("重启后已排期的重试应仍待命")
	}
	// 次数已用掉 → 再全灭也不再补排（否则重启就多打一轮上游）。
	d := s2.Ledger().PlanRetry("checkin", now, 15*time.Minute, 1)
	if d.Reason != taskledger.ReasonMaxReached {
		t.Errorf("重启后应仍记得当日已用 1 次，got reason=%q", d.Reason)
	}
}
