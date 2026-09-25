// budget_test.go 当日积分预算闸（budget.go）的契约测试。
//
// 核心不变量：
//  1. **缺省关闭**：limit <= 0 时永远放行，行为与引入前逐字一致；
//  2. **判据是严格小于**：used == limit 即已到顶，不再放行；
//  3. **只统计观测到的扣费**：上游没给 credit 时不计数（缺失≠0，与成本账本同纪律）；
//     否则用量永远涨不上去、闸形同虚设；
//  4. **跨 CST 自然日归零**，且按固定 +8 偏移算日界；
//  5. **闸在读 body 之前**：被拒的请求不该先把请求体读进内存再丢掉；
//  6. 拒绝次数可观测（运维要能区分「预算挡的」与「账号全挂了」）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestBudgetDisabledAlwaysAdmits 关闭态（0 = 不限）：永远放行，且不记拒绝。
// 这是「缺省 = 旧行为」在闸上的落点——老 config 不含该键时行为逐字不变。
func TestBudgetDisabledAlwaysAdmits(t *testing.T) {
	b := newDailyBudget(0)
	for i := 0; i < 100; i++ {
		b.add(1000, true) // 即使用量远超任何想象的值
		if !b.admit() {
			t.Fatalf("第 %d 次：关闭态必须永远放行", i)
		}
	}
	used, limit, rejected := b.snapshot()
	if used == 0 {
		t.Error("关闭态仍应累计用量——运维要能用观察模式跑几天再定阈值")
	}
	if limit != 0 {
		t.Errorf("limit=%v want 0", limit)
	}
	if rejected != 0 {
		t.Errorf("关闭态不应记拒绝, rejected=%d", rejected)
	}
}

// TestBudgetStrictlyLessThanLimit 判据是 used < limit：刚好到顶就不再放行。
func TestBudgetStrictlyLessThanLimit(t *testing.T) {
	b := newDailyBudget(10)
	b.add(9.99, true)
	if !b.admit() {
		t.Fatal("9.99 < 10 应放行")
	}
	b.add(0.01, true) // used 恰好 10
	if b.admit() {
		t.Fatal("used == limit 已经到顶，不得放行（判据必须是严格小于）")
	}
	_, _, rejected := b.snapshot()
	if rejected != 1 {
		t.Errorf("rejected=%d want 1", rejected)
	}
}

// TestBudgetIgnoresUnobservedCredit 上游没给 credit 时不计数。
// 把「缺观测」当 0 会让用量永远涨不上去，闸永远不触发——这是本功能最容易写错的地方。
func TestBudgetIgnoresUnobservedCredit(t *testing.T) {
	b := newDailyBudget(10)
	b.add(100, false) // 观测缺失
	if used, _, _ := b.snapshot(); used != 0 {
		t.Fatalf("未观测到的扣费不得计入, used=%v", used)
	}
	if !b.admit() {
		t.Fatal("用量仍为 0，应放行")
	}
	// 对照组：同样是 100，但这次有观测 → 必须计入并触发拒绝。
	b.add(100, true)
	if b.admit() {
		t.Fatal("有观测的 100 ≥ 10 应拒绝")
	}
}

// TestBudgetIgnoresNegativeCredit 上游字段异常给负值时宁可少算，也不倒扣——
// 倒扣会把已经关上的闸重新打开。
func TestBudgetIgnoresNegativeCredit(t *testing.T) {
	b := newDailyBudget(10)
	b.add(10, true)
	if b.admit() {
		t.Fatal("precondition: 应已到顶")
	}
	b.add(-100, true) // 异常负值
	if b.admit() {
		t.Fatal("负值不得倒扣当日用量、不得把闸重新打开")
	}
	if used, _, _ := b.snapshot(); used != 10 {
		t.Errorf("used=%v want 10（负值应被忽略）", used)
	}
}

// TestBudgetResetsOnNewCSTDay 跨日归零：用量与拒绝次数都要清。
// 白盒把 day 拨到过去（惰性滚动在下次访问时触发），不必等真实跨天。
func TestBudgetResetsOnNewCSTDay(t *testing.T) {
	b := newDailyBudget(10)
	b.add(10, true)
	b.admit() // 记一次拒绝
	if used, _, rejected := b.snapshot(); used != 10 || rejected != 1 {
		t.Fatalf("precondition: used=%v rejected=%d", used, rejected)
	}

	b.mu.Lock()
	b.day = "2000-01-01" // 假装计数停留在很久以前
	b.mu.Unlock()

	used, _, rejected := b.snapshot()
	if used != 0 {
		t.Errorf("跨日应归零, used=%v", used)
	}
	if rejected != 0 {
		t.Errorf("跨日应清拒绝计数, rejected=%d", rejected)
	}
	if !b.admit() {
		t.Error("归零后应重新放行")
	}
	// 且新一天的用量从零起算（不是把昨天的值加回来）
	b.add(3, true)
	if used, _, _ := b.snapshot(); used != 3 {
		t.Errorf("新一日用量 used=%v want 3", used)
	}
}

// TestCSTDayUsesFixedPlus8 日界按固定 +8 偏移算。
// 23:30 UTC 已经是次日的 07:30 CST——若用 UTC 算日界，闸会在错误的时刻重置。
func TestCSTDayUsesFixedPlus8(t *testing.T) {
	utc := time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC)
	if got := cstDay(utc); got != "2026-01-02" {
		t.Fatalf("cstDay(23:30 UTC)=%q want 2026-01-02（CST = UTC+8）", got)
	}
	// 另一侧边界：15:59 UTC = 23:59 CST 仍是当日；16:00 UTC = 次日 00:00 CST
	if got := cstDay(time.Date(2026, 1, 1, 15, 59, 0, 0, time.UTC)); got != "2026-01-01" {
		t.Errorf("cstDay(15:59 UTC)=%q want 2026-01-01", got)
	}
	if got := cstDay(time.Date(2026, 1, 1, 16, 0, 0, 0, time.UTC)); got != "2026-01-02" {
		t.Errorf("cstDay(16:00 UTC)=%q want 2026-01-02（CST 零点）", got)
	}
}

// TestBudgetNilSafe 零值（直接手搓 &Handler{} 时的 budget 字段）不 panic、一律放行。
// 让「忘了接线」退化成「闸不存在」，而不是让整个对话接口崩掉。
func TestBudgetNilSafe(t *testing.T) {
	var b *dailyBudget
	if !b.admit() {
		t.Error("nil 闸应放行")
	}
	b.add(5, true) // 不得 panic
	used, limit, rejected := b.snapshot()
	if used != 0 || limit != 0 || rejected != 0 {
		t.Errorf("nil 闸快照应全零, got %v %v %v", used, limit, rejected)
	}
}

// TestChatRejectedWhenBudgetExhausted 顶到上限后，对话请求被拒且给出明确的错误码。
func TestChatRejectedWhenBudgetExhausted(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, BudgetLimit: 5})
	h.budget.add(5, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429, body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "daily_budget_exceeded")
	if calls != 0 {
		t.Errorf("被预算挡下的请求不得打到上游, calls=%d", calls)
	}
	// 错误文案要带用量，否则运维只知道被拒、不知道离阈值多远。
	if !strings.Contains(rec.Body.String(), "5.00") {
		t.Errorf("错误文案应带出用量与上限, body=%s", rec.Body)
	}
	_, _, rejected := h.budget.snapshot()
	if rejected != 1 {
		t.Errorf("拒绝次数应被记录, rejected=%d", rejected)
	}
}

// budgetProbeReader 记录请求体被读了几次。用来把「闸在读 body 之前」变成可断言
// 的事实，而不是靠状态码间接推断（状态码只能区分「读了并失败」与「没读」，
// 区分不了「读了但忽略错误」——那同样会把几十 MB 白读进内存）。
type budgetProbeReader struct{ reads int }

func (r *budgetProbeReader) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

// TestBudgetGatePrecedesBodyRead 闸必须在读 body 之前。
// 判据是「请求体一次都没被读过」：若闸挪到读取之后，reads 会变成 1——被拒的请求
// 会先把几十 MB 的 body 读进内存再丢掉。
func TestBudgetGatePrecedesBodyRead(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, BudgetLimit: 5})
	h.budget.add(5, true)

	probe := &budgetProbeReader{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", probe))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}
	if probe.reads != 0 {
		t.Fatalf("被拒的请求不得读请求体（否则大 body 会被白读进内存）, reads=%d", probe.reads)
	}
}

// TestChatAdmittedBelowLimitReachesUpstream 未到上限时闸不拦，请求照常打到上游。
func TestChatAdmittedBelowLimitReachesUpstream(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, BudgetLimit: 5})
	h.budget.add(4.99, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("未到上限不应被拦, body=%s", rec.Body)
	}
	if calls != 1 {
		t.Fatalf("未到上限的请求应打到上游, calls=%d code=%d body=%s", calls, rec.Code, rec.Body)
	}
}

// TestChatStatDoneAccumulatesBudget done() 是记账的唯一出口（与 metrics 同一纪律）：
// 走它才会进预算，且只在 hasCredit 时累加。
func TestChatStatDoneAccumulatesBudget(t *testing.T) {
	resetMetricsForTest(t) // done() 会写全局 metrics，隔离掉

	b := newDailyBudget(100)
	st := newChatStat(time.Now(), []byte(`{"model":"budget-probe","messages":[]}`), true, b)
	st.hasCredit = true
	st.credit = 3.5
	st.done()

	if used, _, _ := b.snapshot(); used != 3.5 {
		t.Fatalf("done() 应把 credit 计入预算, used=%v want 3.5", used)
	}

	// 对照组：hasCredit=false 时即便 credit 字段有值也不得计入。
	st2 := newChatStat(time.Now(), []byte(`{"model":"budget-probe","messages":[]}`), true, b)
	st2.credit = 999 // 未观测：上游没给这个字段
	st2.done()
	if used, _, _ := b.snapshot(); used != 3.5 {
		t.Fatalf("未观测的扣费不得计入, used=%v want 3.5", used)
	}

	// 幂等：done() 重复调用不得重复记账（它自身有 logged 守卫）。
	st.done()
	if used, _, _ := b.snapshot(); used != 3.5 {
		t.Fatalf("done() 重复调用不得重复记账, used=%v", used)
	}
}

// TestStatusExposesDailyBudget /status 透出当日用量、上限、拒绝次数与归属日，
// 否则闸一开就成了黑盒：只知道在拒请求，不知道为什么。
func TestStatusExposesDailyBudget(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, BudgetLimit: 42})
	h.budget.add(7.25, true)
	h.budget.admit() // 未到顶，不记拒绝

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var body struct {
		DailyBudget struct {
			Used     float64 `json:"used"`
			Limit    float64 `json:"limit"`
			Rejected int64   `json:"rejected"`
			Day      string  `json:"day"`
		} `json:"daily_budget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v body=%s", err, rec.Body)
	}
	db := body.DailyBudget
	if db.Used != 7.25 || db.Limit != 42 {
		t.Errorf("daily_budget used=%v limit=%v want 7.25 / 42", db.Used, db.Limit)
	}
	if db.Day != cstDay(time.Now()) {
		t.Errorf("daily_budget day=%q want %q", db.Day, cstDay(time.Now()))
	}
}

// TestStatusBudgetZeroWhenDisabled 闸关闭时也要透出该字段（limit=0），
// 让运维能用「观察模式」先看真实日耗再定阈值。
func TestStatusBudgetZeroWhenDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p}) // BudgetLimit 零值 = 关闭

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if !strings.Contains(rec.Body.String(), `"daily_budget"`) {
		t.Fatalf("关闭态也应透出 daily_budget（limit=0 即不限）, body=%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"limit":0`) {
		t.Errorf("关闭态 limit 应为 0, body=%s", rec.Body)
	}
}
