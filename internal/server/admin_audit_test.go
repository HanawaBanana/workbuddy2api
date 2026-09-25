// admin_audit_test.go 管理操作审计（admin_audit.go）的契约测试。
//
// 核心不变量：
//  1. **默认不审计**：Audit 为 nil 时不建文件、不写盘、零行为差异；
//  2. **每次操作恰好一行**，内容含 ts/action/target/status/remote/key；
//  3. **未通过鉴权的探测不落盘**（否则匿名方可以刷满运维磁盘）；
//  4. **key 原文永不落盘**，只有不可反推的指纹；
//  5. **审计读 body 不改变请求语义**：handler 仍能读到完整的、包括超过审计
//     读取上限的部分；
//  6. **审计写失败不失败请求**：磁盘侧出问题不该升级成「坏账号摘不掉」。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newTestAudit 造一个落在临时目录里的审计接收器，返回接收器与文件路径。
// 用子目录（data/）而不是临时目录根：顺带覆盖 NewAuditLog 自建父目录的行为。
func newTestAudit(t *testing.T, apiKey string) (*AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "admin_audit.log")
	al, err := NewAuditLog(path, apiKey)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return al, path
}

// auditLines 读出审计文件的全部记录。行不是合法 JSON 即失败——JSONL 的意义
// 就在于逐行可解析，写坏一行等于整份日志无法用脚本消费。
func auditLines(t *testing.T, path string) []auditEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件: %v", err)
	}
	var out []auditEntry
	for i, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if ln == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v (line=%q)", i+1, err, ln)
		}
		out = append(out, e)
	}
	return out
}

// doAudited 发一个管理请求；key 为空则不带头。
func doAudited(t *testing.T, h *Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAuditDisabledByDefault Audit 为 nil 时不产生任何文件——老部署（不含
// admin.audit_enabled 键）不该因为引入本功能而多出一个磁盘文件。
func TestAuditDisabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "admin_audit.log")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true}) // Audit 零值 nil

	rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"r"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable code=%d want 200（不审计不得影响操作本身）", rec.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("未开启审计时不应创建文件, stat err=%v", err)
	}
}

// TestAuditRecordsSuccessfulDisable 一次成功的停用应恰好落一行，且字段齐全：
// 谁（remote + key 指纹）、对谁（target）、做了什么（action）、结果如何（status）。
func TestAuditRecordsSuccessfulDisable(t *testing.T) {
	al, path := newTestAudit(t, "secret-key")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "secret-key", AdminEnabled: true, Audit: al})

	rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"观察几天"}`, "secret-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable code=%d body=%s", rec.Code, rec.Body)
	}

	got := auditLines(t, path)
	if len(got) != 1 {
		t.Fatalf("应恰好 1 行，实到 %d 行: %+v", len(got), got)
	}
	e := got[0]
	if e.Action != "account.disable" {
		t.Errorf("action=%q want account.disable", e.Action)
	}
	if e.Target != "u1" {
		t.Errorf("target=%q want u1", e.Target)
	}
	if e.Status != http.StatusOK {
		t.Errorf("status=%d want 200", e.Status)
	}
	// httptest.NewRequest 的默认 RemoteAddr；记 TCP 对端而非 X-Forwarded-For
	// （后者可伪造，写进审计会污染证据链）。
	if e.Remote != "192.0.2.1:1234" {
		t.Errorf("remote=%q want 192.0.2.1:1234", e.Remote)
	}
	if e.Key == "" || e.Key == "secret-key" {
		t.Errorf("key 应是指纹而非原文, got %q", e.Key)
	}
	if e.Reason != "观察几天" {
		t.Errorf("reason=%q want 观察几天", e.Reason)
	}
	if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
		t.Errorf("ts=%q 不是 RFC3339: %v", e.TS, err)
	}
}

// TestAuditRecordsFailedAction 失败的操作同样要留痕：404（uid 不存在）与 409
// （任务已在跑）本身就是「谁在什么时候试图做了什么」的有效证据。
func TestAuditRecordsFailedAction(t *testing.T) {
	al, path := newTestAudit(t, "k")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Audit: al})

	if rec := doAudited(t, h, "POST", "/admin/accounts/nope/disable", "", "k"); rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
	got := auditLines(t, path)
	if len(got) != 1 {
		t.Fatalf("失败操作也应落一行, 实到 %d", len(got))
	}
	if got[0].Status != http.StatusNotFound || got[0].Target != "nope" {
		t.Errorf("got %+v, want status=404 target=nope", got[0])
	}
}

// TestAuditSkipsUnauthenticatedRequests 未通过鉴权的探测不落盘。
// 这是安全不变量而非风格偏好：审计文件写在运维的磁盘上，若匿名请求也记，
// 任何人刷 /admin 就能把磁盘写满（用一个安全特性换来一个 DoS 面）。
func TestAuditSkipsUnauthenticatedRequests(t *testing.T) {
	al, path := newTestAudit(t, "k")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Audit: al})

	if rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key: code=%d want 401", rec.Code)
	}
	if rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", "", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 key: code=%d want 401", rec.Code)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("未鉴权请求不得落盘, 文件内容=%q", raw)
	}
}

// TestAuditNeverWritesRawAPIKey 反向断言：整个文件里不得出现 api_key 原文。
// 用一段独特的 key 文本，避免「恰好没出现」的假通过。
func TestAuditNeverWritesRawAPIKey(t *testing.T) {
	const key = "ZZTOP-SECRET-KEY-9f3a1b"
	al, path := newTestAudit(t, key)
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: key, AdminEnabled: true, Audit: al})

	if rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"r"}`, key); rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("审计文件不得含 api_key 原文: %s", raw)
	}
	if !strings.Contains(string(raw), keyFingerprint(key)) {
		t.Fatalf("审计文件应含 key 指纹 %q: %s", keyFingerprint(key), raw)
	}
}

// TestAuditBodyPreservedForHandler 审计读 body 取 reason 之后，handler 必须仍能
// 读到**完整**的 body，包括超过审计读取上限（4KB）的尾部。
// 用一个直连中间件的探针 handler，直接断言读到的字节与原 body 逐字相同。
func TestAuditBodyPreservedForHandler(t *testing.T) {
	al, _ := newTestAudit(t, "k")
	h := &Handler{cfg: Config{Audit: al}}

	var seen []byte
	probe := func(w http.ResponseWriter, r *http.Request) {
		var err error
		seen, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("handler 读 body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}
	wrapped := h.audit("probe", auditPathValue("uid"), probe)

	// 远超 auditBodyPeek（4KB）：若中间件「读满就换新 reader」而不是拼回剩余，
	// 尾部会丢失，这条会红。
	body := `{"reason":"r","pad":"` + strings.Repeat("x", 100_000) + `"}`
	req := httptest.NewRequest("POST", "/admin/accounts/u1/disable", strings.NewReader(body))
	wrapped(httptest.NewRecorder(), req)

	if string(seen) != body {
		t.Fatalf("handler 读到的 body 不完整: len=%d want %d", len(seen), len(body))
	}
}

// TestAuditReasonReachesBothSides reason 既要进审计行，也要照旧进 handler 的
// 状态变更——中间件插入不能把 handler 的入参吃掉。
func TestAuditReasonReachesBothSides(t *testing.T) {
	al, path := newTestAudit(t, "k")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Audit: al})

	if rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"上游抖动"}`, "k"); rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if _, reason, _ := p.ManualDisabledState("u1"); reason != "上游抖动" {
		t.Fatalf("handler 未收到 reason: %q", reason)
	}
	if got := auditLines(t, path); len(got) != 1 || got[0].Reason != "上游抖动" {
		t.Fatalf("审计行未记 reason: %+v", got)
	}
}

// TestAuditTaskRunRecordsTaskName 任务触发路由的目标是任务名（路径段 name），
// 不是 uid——两条路由的目标字段不同，取错字段是静默的语义错误。
func TestAuditTaskRunRecordsTaskName(t *testing.T) {
	al, path := newTestAudit(t, "k")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	fake := &fakeTaskRunner{}
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Tasks: fake, Audit: al})

	if rec := doAudited(t, h, "POST", "/admin/tasks/checkin/run", "", "k"); rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	fake.waitCalls(t, 1)

	got := auditLines(t, path)
	if len(got) != 1 {
		t.Fatalf("应 1 行, 实到 %d", len(got))
	}
	if got[0].Action != "task.run" || got[0].Target != "checkin" || got[0].Status != http.StatusAccepted {
		t.Fatalf("got %+v, want action=task.run target=checkin status=202", got[0])
	}
}

// TestAuditAppendsOneLinePerAction 追加而非覆盖：连续三次操作 → 三行，且顺序
// 与调用顺序一致（审计日志的价值全在历史，覆盖等于没审计）。
func TestAuditAppendsOneLinePerAction(t *testing.T) {
	al, path := newTestAudit(t, "k")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Audit: al})

	for _, op := range []string{"disable", "enable", "revive"} {
		if rec := doAudited(t, h, "POST", "/admin/accounts/u1/"+op, "", "k"); rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d body=%s", op, rec.Code, rec.Body)
		}
	}
	got := auditLines(t, path)
	if len(got) != 3 {
		t.Fatalf("应 3 行, 实到 %d: %+v", len(got), got)
	}
	want := []string{"account.disable", "account.enable", "account.revive"}
	for i, w := range want {
		if got[i].Action != w {
			t.Errorf("第 %d 行 action=%q want %q", i+1, got[i].Action, w)
		}
	}
}

// TestAuditWriteFailureDoesNotFailRequest 审计侧写不进去（这里把路径换成目录，
// 让 O_WRONLY 打开失败）时，管理操作本身仍必须成功返回。
// 反过来的「写不进审计就拒绝操作」会把一次磁盘故障升级成「坏账号摘不掉」。
func TestAuditWriteFailureDoesNotFailRequest(t *testing.T) {
	al, path := newTestAudit(t, "k")
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Audit: al})

	// 让后续写入必然失败：把文件换成同名目录。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := doAudited(t, h, "POST", "/admin/accounts/u1/disable", "", "k")
	if rec.Code != http.StatusOK {
		t.Fatalf("审计写入失败不得影响操作: code=%d body=%s", rec.Code, rec.Body)
	}
	if st, _, _ := p.ManualDisabledState("u1"); !st {
		t.Fatal("操作应已生效")
	}
}

// TestNewAuditLogFailsFast 启动期预检：空路径、以及父路径是个普通文件（无法
// 建目录）都要报错，而不是等到第一次管理操作才发现审计是空的。
func TestNewAuditLogFailsFast(t *testing.T) {
	if _, err := NewAuditLog("   ", "k"); err == nil {
		t.Error("空路径应报错")
	}

	// 造一个普通文件，再把它当目录用。
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuditLog(filepath.Join(blocker, "sub", "audit.log"), "k"); err == nil {
		t.Error("父路径是普通文件时应报错")
	}
}

// TestNewAuditLogCreatesParentDir 预检顺带把 ./data/ 建出来（与 state_file 同
// 约定：目录由进程自建，运维不必先 mkdir）。
func TestNewAuditLogCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "admin_audit.log")
	al, err := NewAuditLog(path, "k")
	if err != nil {
		t.Fatal(err)
	}
	if al.Path() != path {
		t.Errorf("Path()=%q want %q", al.Path(), path)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		t.Fatalf("父目录应被创建: fi=%v err=%v", fi, err)
	}
}

// TestAuditResponseWriterStatus 状态码捕获本身的行为：显式 WriteHeader 记该码，
// 只 Write 记隐式 200，重复 WriteHeader 只认第一次。
func TestAuditResponseWriterStatus(t *testing.T) {
	cases := []struct {
		name string
		run  func(w http.ResponseWriter)
		want int
	}{
		{"显式 WriteHeader", func(w http.ResponseWriter) { w.WriteHeader(http.StatusConflict) }, http.StatusConflict},
		{"只 Write 即隐式 200", func(w http.ResponseWriter) { _, _ = w.Write([]byte("{}")) }, http.StatusOK},
		{"重复 WriteHeader 只认第一次", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusAccepted)
			w.WriteHeader(http.StatusOK)
		}, http.StatusAccepted},
		{"什么都没写按 200 兜底", func(http.ResponseWriter) {}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w := &auditResponseWriter{ResponseWriter: rec}
			c.run(w)
			if got := w.statusCode(); got != c.want {
				t.Fatalf("statusCode()=%d want %d", got, c.want)
			}
		})
	}
}

// TestKeyFingerprintStableAndDistinct 指纹要稳定（同一 key 恒等，便于跨行归并）
// 且能区分不同 key（轮换前后可辨）。
func TestKeyFingerprintStableAndDistinct(t *testing.T) {
	if keyFingerprint("") != "-" {
		t.Errorf("空 key 应记 -, got %q", keyFingerprint(""))
	}
	a1, a2 := keyFingerprint("key-a"), keyFingerprint("key-a")
	if a1 != a2 {
		t.Errorf("同一 key 指纹应稳定: %q vs %q", a1, a2)
	}
	if len(a1) != 8 {
		t.Errorf("指纹应为 8 hex, got %q (len=%d)", a1, len(a1))
	}
	if a1 == keyFingerprint("key-b") {
		t.Error("不同 key 应得到不同指纹")
	}
}
