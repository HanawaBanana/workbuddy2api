// task_ledger_test.go 任务执行台账在 server 侧的两条出口：/status 的 task_ledger
// 段与 /metrics 的 wb2api_task_* 指标家族。
//
// 台账本身（记录、重试排期、跨日滚动）由 internal/taskledger 与 internal/scheduler
// 的测试覆盖；这里只钉住**渲染契约**：
//   - 字段名就是台账结构体的 JSON 标签（运维/告警规则照这个写）；
//   - 未接线（Config.TaskLedger 为 nil）时 /status 不含该键、/metrics 不含该家族
//     ——老部署与既有测试的响应体形状不变；
//   - 只为「跑过至少一轮」的任务输出指标序列：从未跑过没有序列，而不是时刻 0
//     （否则每次部署都会让 `time() - ts > 86400` 类告警误报一轮）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskledger"
)

// fakeTaskLedger 注入用的台账视图：只做两件事——把给定的 map 原样返回。
type fakeTaskLedger struct {
	runs  map[string]taskledger.Run
	retry map[string]taskledger.RetryState
}

func (f *fakeTaskLedger) Runs() map[string]taskledger.Run               { return f.runs }
func (f *fakeTaskLedger) RetryStates() map[string]taskledger.RetryState { return f.retry }

// ledgerRun 造一条台账记录。
func ledgerRun(kind, trigger string, ok, fail int, finished time.Time) taskledger.Run {
	return taskledger.Run{
		Kind: kind, Trigger: trigger,
		Started: finished.Add(-time.Minute), Finished: finished,
		Elapsed: "1m0s",
		Total:   ok + fail, OK: ok, Fail: fail,
		AllFailed: fail > 0 && ok == 0,
	}
}

// TestStatusExposesTaskLedger /status 透出六类任务最近一轮与当日重试状态：
// 这是「昨晚 21 点签到跑了吗、成功几个」唯一不必翻日志的答案。
func TestStatusExposesTaskLedger(t *testing.T) {
	fin := time.Date(2026, 9, 27, 21, 3, 0, 0, time.Local)
	next := fin.Add(15 * time.Minute)
	led := &fakeTaskLedger{
		runs: map[string]taskledger.Run{
			"checkin": ledgerRun("checkin", taskledger.TriggerRetry, 12, 0, fin),
			"travel":  ledgerRun("travel", taskledger.TriggerSchedule, 0, 3, fin),
		},
		retry: map[string]taskledger.RetryState{
			"travel": {Day: "2026-09-27", Used: 1, Next: &next},
		},
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), TaskLedger: led})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var body struct {
		TaskLedger struct {
			Runs map[string]struct {
				Kind      string `json:"kind"`
				Trigger   string `json:"trigger"`
				Elapsed   string `json:"elapsed"`
				Total     int    `json:"total"`
				OK        int    `json:"ok"`
				Fail      int    `json:"fail"`
				AllFailed bool   `json:"all_failed"`
			} `json:"runs"`
			Retry map[string]struct {
				Day  string     `json:"day"`
				Used int        `json:"used"`
				Next *time.Time `json:"next"`
			} `json:"retry"`
		} `json:"task_ledger"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v body=%s", err, rec.Body)
	}
	ck := body.TaskLedger.Runs["checkin"]
	if ck.Kind != "checkin" || ck.Trigger != taskledger.TriggerRetry {
		t.Errorf("checkin 记录不对：%+v", ck)
	}
	if ck.Total != 12 || ck.OK != 12 || ck.AllFailed {
		t.Errorf("checkin 计数不对：%+v", ck)
	}
	if ck.Elapsed != "1m0s" {
		t.Errorf("elapsed 应透出人类可读时长，got %q", ck.Elapsed)
	}
	tr := body.TaskLedger.Runs["travel"]
	if tr.Fail != 3 || !tr.AllFailed {
		t.Errorf("travel 应透出全灭，got %+v", tr)
	}
	st := body.TaskLedger.Retry["travel"]
	if st.Used != 1 || st.Next == nil || !st.Next.Equal(next) {
		t.Errorf("retry 状态不对：%+v", st)
	}
}

// TestStatusOmitsTaskLedgerWhenUnwired 未接线时不写该键（而非写 null）：
// 老部署与既有测试的响应体形状保持不变。
func TestStatusOmitsTaskLedgerWhenUnwired(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if strings.Contains(rec.Body.String(), "task_ledger") {
		t.Errorf("未接线时不该含 task_ledger，body=%s", rec.Body)
	}
	// 既有字段照常在（不能因为加了这个键把别的挤掉）。
	for _, key := range []string{`"accounts"`, `"daily_budget"`, `"cost_explore"`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("既有字段 %s 不见了，body=%s", key, rec.Body)
		}
	}
}

// TestStatusTaskLedgerRetryNextOmittedWhenIdle 无待重试时 next 应被省略，
// 不能渲染成 "0001-01-01T00:00:00Z"（time.Time 是结构体，omitempty 对它无效，
// 所以 RetryState.Next 用的是指针）。
//
// 断言按 JSON 键存在性做，不能整串搜 "0001-01-01"：/status 的 accounts 段本来
// 就有若干零值时间字段，整串搜会把它们的既有形状误判成本次回归。
func TestStatusTaskLedgerRetryNextOmittedWhenIdle(t *testing.T) {
	led := &fakeTaskLedger{
		runs:  map[string]taskledger.Run{"cat": ledgerRun("cat", taskledger.TriggerSchedule, 1, 0, time.Now())},
		retry: map[string]taskledger.RetryState{"cat": {Day: "2026-09-27", Used: 2}},
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), TaskLedger: led})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var body struct {
		TaskLedger struct {
			Retry map[string]map[string]json.RawMessage `json:"retry"`
		} `json:"task_ledger"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v body=%s", err, rec.Body)
	}
	st, ok := body.TaskLedger.Retry["cat"]
	if !ok {
		t.Fatalf("应透出 cat 的重试状态，body=%s", rec.Body)
	}
	if raw, ok := st["next"]; ok {
		t.Errorf("无待重试时不该写出 next，got %s", raw)
	}
	// 计数照常透出（运维据此判断次数是否用尽）。
	if raw, ok := st["used"]; !ok || string(raw) != "2" {
		t.Errorf("used 应为 2，got %s", raw)
	}
	if raw, ok := st["day"]; !ok || string(raw) != `"2026-09-27"` {
		t.Errorf("day 应透出，got %s", raw)
	}
}

// TestPromMetricsTaskFamilies /metrics 输出三类任务指标，且**只为跑过的任务**
// 输出序列：从未跑过没有序列，而不是时刻 0。
func TestPromMetricsTaskFamilies(t *testing.T) {
	fin := time.Date(2026, 9, 27, 21, 3, 0, 0, time.UTC)
	tasks := map[string]taskledger.Run{
		"checkin": ledgerRun("checkin", taskledger.TriggerSchedule, 12, 0, fin),
		"travel":  ledgerRun("travel", taskledger.TriggerSchedule, 0, 3, fin),
	}
	out := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, tasks)

	for _, want := range []string{
		"# HELP wb2api_task_last_run_timestamp_seconds",
		"# TYPE wb2api_task_last_run_timestamp_seconds gauge",
		"# HELP wb2api_task_last_run_accounts",
		"# HELP wb2api_task_last_run_all_failed",
		`wb2api_task_last_run_timestamp_seconds{kind="checkin"} `,
		`wb2api_task_last_run_accounts{kind="checkin",result="ok"} 12`,
		`wb2api_task_last_run_accounts{kind="checkin",result="already"} 0`,
		`wb2api_task_last_run_accounts{kind="checkin",result="total"} 12`,
		`wb2api_task_last_run_all_failed{kind="checkin"} 0`,
		`wb2api_task_last_run_all_failed{kind="travel"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q\n---\n%s", want, out)
		}
	}
	// 跑过的任务的结束时刻要准确（告警靠 time() - ts 判断停跑）。
	if !strings.Contains(out, `wb2api_task_last_run_timestamp_seconds{kind="checkin"} `+formatPromValue(float64(fin.Unix()))) {
		t.Errorf("checkin 时间戳不对\n---\n%s", out)
	}
	// 没跑过的任务不输出序列。
	for _, absent := range []string{`kind="school"`, `kind="cat"`, `kind="keepalive"`, `kind="activity"`} {
		if strings.Contains(out, absent) {
			t.Errorf("未跑过的任务不该有序列：%s\n---\n%s", absent, out)
		}
	}
}

// TestPromMetricsTaskOutputDeterministic 同一状态下两次渲染逐字节相等：kind 与
// result 维度都用固定顺序，不遍历 map（本仓库对稳定输出的既有要求）。
func TestPromMetricsTaskOutputDeterministic(t *testing.T) {
	fin := time.Date(2026, 9, 27, 21, 3, 0, 0, time.UTC)
	tasks := map[string]taskledger.Run{
		"cat":       ledgerRun("cat", taskledger.TriggerSchedule, 1, 0, fin),
		"checkin":   ledgerRun("checkin", taskledger.TriggerSchedule, 12, 0, fin),
		"keepalive": ledgerRun("keepalive", taskledger.TriggerSchedule, 5, 1, fin),
	}
	first := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, tasks)
	for i := 0; i < 20; i++ {
		if got := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, tasks); got != first {
			t.Fatalf("第 %d 次渲染与首次不同：输出顺序不稳定", i+2)
		}
	}
	// 顺序按 promTaskKinds（checkin → activity → keepalive → travel → school → cat）。
	iCheckin := strings.Index(first, `kind="checkin"`)
	iKeep := strings.Index(first, `kind="keepalive"`)
	iCat := strings.Index(first, `kind="cat"`)
	if !(iCheckin < iKeep && iKeep < iCat) {
		t.Errorf("kind 维度应按固定顺序输出：checkin(%d) keepalive(%d) cat(%d)", iCheckin, iKeep, iCat)
	}
}

// TestPromMetricsTaskSectionAbsentWhenUnwired 未接线时整个任务家族不出现——
// 开了 metrics 但没接台账（例如只跑 cmd/activity）不该输出一堆 0。
func TestPromMetricsTaskSectionAbsentWhenUnwired(t *testing.T) {
	out := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, nil)
	if strings.Contains(out, "wb2api_task_") {
		t.Errorf("未接线时不该输出任务指标\n---\n%s", out)
	}
	// 其余家族照常。
	if !strings.Contains(out, "wb2api_pool_accounts_total") {
		t.Error("其余指标家族不该受影响")
	}
}

// TestPromMetricsEndpointWiresTaskLedger /metrics 端点确实把 Config.TaskLedger
// 读进渲染（接线层面的回归：writePromMetrics 的单元测试不会发现「忘了传」）。
func TestPromMetricsEndpointWiresTaskLedger(t *testing.T) {
	fin := time.Date(2026, 9, 27, 21, 3, 0, 0, time.UTC)
	led := &fakeTaskLedger{runs: map[string]taskledger.Run{
		"checkin": ledgerRun("checkin", taskledger.TriggerSchedule, 7, 0, fin),
	}}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), MetricsEnabled: true, TaskLedger: led})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `wb2api_task_last_run_accounts{kind="checkin",result="ok"} 7`) {
		t.Errorf("端点未把台账接进渲染\n---\n%s", rec.Body)
	}
}

// TestTaskLedgerReaderSatisfiedByStore *taskledger.Store 必须结构上满足本包声明的
// 窄接口——这是 main 里 TaskLedger: sch.Ledger() 能编译的前提，写成编译期断言，
// 免得以后改接口时只在 cmd/server 才发现。
var _ TaskLedgerReader = (*taskledger.Store)(nil)
