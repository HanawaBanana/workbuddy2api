package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// checkinActivityScheduler 构造一个只有 upstream stub 的最小 Scheduler。
func checkinActivityScheduler(t *testing.T, h http.HandlerFunc) (*Scheduler, *pool.Pool, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p, srv
}

// TestWarnCheckinActivityQueriesOnce 活动状态是活动级数据：一轮只打一次上游，
// 不随账号数放大（否则一个只读探测会把请求量变成 O(N)）。
func TestWarnCheckinActivityQueriesOnce(t *testing.T) {
	var calls atomic.Int32
	s, p, srv := checkinActivityScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/checkin-activity-status" {
			http.Error(w, "not found", 404)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", 405)
			return
		}
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"enabled":true,` +
			`"end_at":"2030-01-01T00:00:00+08:00","credits":100}}`))
	})
	defer srv.Close()

	s.warnCheckinActivity(p.List())

	if n := calls.Load(); n != 1 {
		t.Errorf("checkin-activity-status calls=%d want 1", n)
	}
}

// TestWarnCheckinActivityUpstreamErrorIsSoft 上游报错只记日志：不 panic、不返回错误、
// 不影响签到（该探测不属于签到链路，失败不该污染台账）。
func TestWarnCheckinActivityUpstreamErrorIsSoft(t *testing.T) {
	s, p, srv := checkinActivityScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":500,"msg":"boom"}`, 500)
	})
	defer srv.Close()

	s.warnCheckinActivity(p.List()) // 不应 panic
}

// TestWarnCheckinActivityNoAccounts 空池：静默跳过，不打上游。
func TestWarnCheckinActivityNoAccounts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.warnCheckinActivity(p.List())

	if n := calls.Load(); n != 0 {
		t.Errorf("calls=%d want 0（无账号不应打上游）", n)
	}
}
