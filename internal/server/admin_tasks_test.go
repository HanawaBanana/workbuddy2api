// admin_tasks_test.go 手动任务触发端点（admin_tasks.go）的契约测试。
//
// 核心不变量：
//  1. **开关复用 admin.enabled**：关闭时路由不注册（404 且为 mux 默认纯文本形态），
//     与真 404 不可区分——不向未鉴权探测暴露「这里有个任务触发面」；
//  2. **白名单派发正确**：六个任务名各自打到**对应**的 Run*Now，不做任意派发；
//     未知名字 404；
//  3. **异步受理**：handler 立即回 202，不等任务跑完（同步会让调用方超时，
//     且无法区分「卡住」与「正在跑」）；
//  4. **同任务防重入**：已在跑时回 409，任务结束后标记释放、可再次触发。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// fakeTaskRunner 记录被触发的任务名。block 非 nil 时每个任务阻塞到该 chan 关闭
// ——这是验证「异步受理」与「防重入」的关键：若 handler 同步执行任务，第一次
// POST 就会在这里死锁，测试直接超时失败。
type fakeTaskRunner struct {
	mu    sync.Mutex
	calls []string
	block chan struct{}
}

func (f *fakeTaskRunner) note(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
}

func (f *fakeTaskRunner) RunCheckinNow()   { f.note("checkin") }
func (f *fakeTaskRunner) RunActivityNow()  { f.note("activity") }
func (f *fakeTaskRunner) RunKeepaliveNow() { f.note("keepalive") }
func (f *fakeTaskRunner) RunTravelNow()    { f.note("travel") }
func (f *fakeTaskRunner) RunSchoolNow()    { f.note("school") }
func (f *fakeTaskRunner) RunCatNow()       { f.note("cat") }

func (f *fakeTaskRunner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// waitCalls 等 fake 记录到 want 个调用。任务在后台 goroutine 里跑，断言前必须
// 等它真的起来，否则测的是「goroutine 调度」而不是「handler 派发」。
func (f *fakeTaskRunner) waitCalls(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %d 次任务触发超时，实到 %v", want, f.snapshot())
	return nil
}

// postTask 打一次 /admin/tasks/{name}/run。
func postTask(t *testing.T, h *Handler, name, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/tasks/"+name+"/run", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAdminTaskRouteGatedByAdminEnabled 复用 admin.enabled 开关：关闭时路由不注册。
// 若有人把路由注册挪到 admin 条件块外，这里会拿到 401（withAuth 进场）而非 404。
func TestAdminTaskRouteGatedByAdminEnabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	fake := &fakeTaskRunner{}
	h := NewHandler(Config{Pool: p, APIKey: "k", Tasks: fake}) // AdminEnabled 零值 false

	// 即使带正确 key 也得 404：路由未注册，withAuth 根本不进场。
	rec := postTask(t, h, "checkin", "k")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 when admin disabled", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("关闭态 404 应为 mux 默认纯文本形态, body=%q", rec.Body.String())
	}
	// GET 探测 POST 路径：未注册就没有 405（注册了才会回 405 + Allow 头）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/tasks/checkin/run", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET: code=%d want 404（未注册，不得出现 405）", rec.Code)
	}
	// 关闭态不得真的触发任何任务。
	time.Sleep(20 * time.Millisecond)
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("关闭态不应触发任务, got %v", got)
	}
}

// TestAdminTaskRequiresSameAPIKey 与账号管理端点、/status 共用同一把 key。
func TestAdminTaskRequiresSameAPIKey(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	fake := &fakeTaskRunner{}
	h := NewHandler(Config{Pool: p, APIKey: "secret", AdminEnabled: true, Tasks: fake})

	if rec := postTask(t, h, "checkin", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key: code=%d want 401", rec.Code)
	}
	if rec := postTask(t, h, "checkin", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 key: code=%d want 401", rec.Code)
	}
	rec := postTask(t, h, "checkin", "secret")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("正确 key: code=%d want 202, body=%s", rec.Code, rec.Body)
	}
	fake.waitCalls(t, 1)
}

// TestAdminTaskDispatchesEveryWhitelistedName 六个任务名各自派发到**对应**的方法。
// 表里写死「名字 → 期望被调用的方法名」，所以把 school 错接成 RunCatNow 这类
// 复制粘贴错误会被直接抓住。
func TestAdminTaskDispatchesEveryWhitelistedName(t *testing.T) {
	for _, name := range adminTaskNames {
		t.Run(name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1"})
			fake := &fakeTaskRunner{}
			h := NewHandler(Config{Pool: p, AdminEnabled: true, Tasks: fake})

			rec := postTask(t, h, name, "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("code=%d want 202, body=%s", rec.Code, rec.Body)
			}
			// 断言响应体回显了请求的任务名（面板/脚本据此确认自己触发的是哪个）。
			var body struct {
				Task   string `json:"task"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v body=%s", err, rec.Body)
			}
			if body.Task != name || body.Status != "started" {
				t.Fatalf("body=%+v want task=%q status=started", body, name)
			}
			// 必须恰好触发一次，且是名字对应的那一个。
			got := fake.waitCalls(t, 1)
			if len(got) != 1 || got[0] != name {
				t.Fatalf("派发错误: got %v want [%s]", got, name)
			}
		})
	}
}

// TestAdminTaskUnknownNameNotFound 非白名单名字回 404 且不触发任何任务
// （路径段是外部输入，只放行固定集合，杜绝任意派发）。
func TestAdminTaskUnknownNameNotFound(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	fake := &fakeTaskRunner{}
	h := NewHandler(Config{Pool: p, AdminEnabled: true, Tasks: fake})

	// 注意不要用 "checkin/../activity" 这类带 .. 的名字：ServeMux 会先 cleanPath
	// 再匹配（可能 301 重定向），那测的是路由器行为而不是白名单。
	// "Checkin" 特意大写：查表是大小写敏感的。
	for _, bad := range []string{"bogus", "Checkin", "checkin2", "all"} {
		rec := postTask(t, h, bad, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%q: code=%d want 404", bad, rec.Code)
		}
		// JSON 信封（不是 mux 纯文本）：路由是注册过的，只是名字不认。
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%q: 404 应为 JSON 信封, body=%q err=%v", bad, rec.Body, err)
		}
		// 文案要点出可用任务名，否则运维只能翻源码。
		if !strings.Contains(envelope.Error.Message, "checkin") {
			t.Errorf("%q: 404 文案应列举已知任务名, got %q", bad, envelope.Error.Message)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("未知名字不得触发任务, got %v", got)
	}
}

// TestAdminTaskBusyConflictsThenReleases 防重入：任务在跑时回 409，跑完释放后可再触发。
//
// 这个测试同时锁住「异步受理」——fake 阻塞在 block 上，若 handler 同步执行任务，
// 第一次 postTask 就会死锁（测试超时），而不是回 202。
func TestAdminTaskBusyConflictsThenReleases(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	block := make(chan struct{})
	fake := &fakeTaskRunner{block: block}
	h := NewHandler(Config{Pool: p, AdminEnabled: true, Tasks: fake})

	// 第一次：受理并返回（不阻塞）。
	if rec := postTask(t, h, "checkin", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("首次: code=%d want 202（异步受理，不得同步等任务跑完）", rec.Code)
	}
	fake.waitCalls(t, 1) // 任务已进入阻塞

	// 第二次：同一任务在跑 → 409，且**不得**再派发一次（防连点打上游）。
	rec := postTask(t, h, "checkin", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("重入: code=%d want 409, body=%s", rec.Code, rec.Body)
	}
	if got := fake.snapshot(); len(got) != 1 {
		t.Fatalf("重入不得重复派发, calls=%v", got)
	}

	// 另一个任务不受影响（防重入是按任务名隔离的，不是全局串行）。
	if rec := postTask(t, h, "travel", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("异任务: code=%d want 202（不同任务不应互相阻塞）", rec.Code)
	}

	// 放行 → 标记释放 → 可再次触发。释放发生在 goroutine 收尾，需轮询等它。
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := postTask(t, h, "checkin", "")
		if rec.Code == http.StatusAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务结束后标记未释放，最后一次 code=%d", rec.Code)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestAdminTaskRunnerNotWired503 配置开了 admin 但没注入 TaskRunner → 503，
// 而不是静默 404。「配置开了但没接线」要让运维看见，否则会被误判成路由写错。
func TestAdminTaskRunnerNotWired503(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true}) // Tasks 为 nil

	rec := postTask(t, h, "checkin", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}
}
