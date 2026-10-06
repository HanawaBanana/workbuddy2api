package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCheckinSchedule 固定答案的排程快照，用来把 handler 与真实调度器解耦。
type fakeCheckinSchedule struct {
	enabled bool
	hours   []int
	jitter  int
	next    time.Time
}

func (f fakeCheckinSchedule) CheckinSchedule() (bool, []int, int) {
	return f.enabled, f.hours, f.jitter
}
func (f fakeCheckinSchedule) NextCheckinAt(time.Time) time.Time { return f.next }

// recAt 造一条只有 finished_at 有意义的记录（裁剪只看它）。
func recAt(t time.Time, ok int) CheckinRecord {
	return CheckinRecord{FinishedAt: t, Total: ok, OK: ok, Results: []CheckinResult{}}
}

// TestPruneCheckinRecords 双条件裁剪：过期（>7 天）先丢，再按条数上限兜底。
func TestPruneCheckinRecords(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	// 8 天前 → 过期；7 天前少 1 分钟 → 过期；7 天前 + 1 分钟 → 保留。
	recs := []CheckinRecord{
		recAt(now.Add(-8*24*time.Hour), 1),
		recAt(now.Add(-7*24*time.Hour-time.Minute), 2),
		recAt(now.Add(-7*24*time.Hour+time.Minute), 3),
		recAt(now.Add(-1*time.Hour), 4),
	}
	got := pruneCheckinRecords(recs, now)
	if len(got) != 2 {
		t.Fatalf("保留条数=%d want 2: %+v", len(got), got)
	}
	if got[0].OK != 3 || got[1].OK != 4 {
		t.Errorf("保留内容不符（应按时间升序留最新的）: %+v", got)
	}

	// 条数上限：造 205 条全在窗口内的记录 → 只留最后 checkinHistoryMaxKeep 条。
	// 按**升序**构造（i 越大越新），与 store 的追加顺序一致（裁剪假定升序）。
	var many []CheckinRecord
	n := checkinHistoryMaxKeep + 5
	for i := 0; i < n; i++ {
		many = append(many, recAt(now.Add(-time.Duration(n-i)*time.Minute), i))
	}
	got = pruneCheckinRecords(many, now)
	if len(got) != checkinHistoryMaxKeep {
		t.Fatalf("条数上限未生效：%d want %d", len(got), checkinHistoryMaxKeep)
	}
	if got[0].OK != n-checkinHistoryMaxKeep || got[len(got)-1].OK != n-1 {
		t.Errorf("应保留最新的 %d 条（OK %d..%d）: 首=%d 末=%d",
			checkinHistoryMaxKeep, n-checkinHistoryMaxKeep, n-1, got[0].OK, got[len(got)-1].OK)
	}
}

// TestCheckinHistoryStoreRoundTrip Append 后内存可见 + 落盘可重载（含 0600 权限）。
func TestCheckinHistoryStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "checkin.json")
	s := NewCheckinHistoryStore(path)
	s.Load() // 文件不存在：静默，空历史

	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("空历史应为 0 条，得 %d", len(got))
	}

	credits := int64(1234)
	started := time.Date(2026, 9, 29, 9, 0, 3, 0, time.UTC)
	finished := started.Add(42 * time.Second)
	s.Append(CheckinReport{
		Total: 1, OK: 1,
		Results: []CheckinResult{{UID: "u1", Nickname: "nick", Realm: "cn", Status: "ok", Credits: &credits}},
	}, started, finished)

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("内存副本应有 1 条，得 %d", len(got))
	}
	if !got[0].FinishedAt.Equal(finished) || got[0].OK != 1 {
		t.Errorf("记录内容不符: %+v", got[0])
	}

	// 权限：含账号昵称/UID，必须 0600（与 state.json 同级敏感）。
	//
	// 只在非 Windows 断言：Windows 的 os.Chmod 只能切只读位，拿不到 0600
	// （实测报 666）。生产是 linux/amd64，Linux 上 WriteFile(0600)+Chmod 生效。
	// 不做「Windows 上放宽断言」以外的处理——放宽到这里就够，不放宽实现。
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("文件权限=%o want 600", perm)
		}
	}

	// 新实例 Load：从磁盘恢复同一条。
	s2 := NewCheckinHistoryStore(path)
	s2.Load()
	got2 := s2.Snapshot()
	if len(got2) != 1 || got2[0].Results[0].UID != "u1" {
		t.Fatalf("重载失败: %+v", got2)
	}
	if got2[0].Results[0].Credits == nil || *got2[0].Results[0].Credits != 1234 {
		t.Errorf("余额指针未往返: %+v", got2[0].Results[0])
	}
}

// TestCheckinHistoryStoreResultsNeverNull Results 为 nil 时落盘必须是 []（前端 .map 不炸）。
func TestCheckinHistoryStoreResultsNeverNull(t *testing.T) {
	s := NewCheckinHistoryStore("")
	s.Append(CheckinReport{Total: 0}, time.Now(), time.Now())
	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("应记录一次空签到，得 %d 条", len(got))
	}
	if got[0].Results == nil {
		t.Error("Results 不应为 nil")
	}
}

// TestCheckinHistoryStoreCorruptFile 损坏文件必须改名留证、从空开始，绝不静默覆盖。
func TestCheckinHistoryStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkin.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewCheckinHistoryStore(path)
	s.Load()

	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("损坏文件应从空开始，得 %d 条", len(got))
	}
	// 原文件已被改名（不再存在于原路径）。
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("损坏文件应被改名移走，stat err=%v", err)
	}
	// 留证文件存在。
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Errorf("应留 1 个 .corrupt-* 证据文件，得 %v", matches)
	}
}

// TestCheckinHistoryEndpoint 端点回吐：服务端时钟/时区 + 排程快照 + 历史记录。
func TestCheckinHistoryEndpoint(t *testing.T) {
	next := time.Date(2026, 9, 29, 21, 0, 5, 0, time.Local)
	s := NewCheckinHistoryStore("")
	s.Append(CheckinReport{
		Total: 2, OK: 1, Already: 1,
		Results: []CheckinResult{
			{UID: "u1", Nickname: "a", Status: "ok"},
			{UID: "u2", Nickname: "b", Status: "already"},
		},
	}, time.Now().Add(-time.Minute), time.Now())

	h := NewHandler(Config{
		APIKey:          "secret",
		CheckinHistory:  s,
		CheckinSchedule: fakeCheckinSchedule{enabled: true, hours: []int{9, 21}, jitter: 0, next: next},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/checkin/history", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d want 200, body=%s", rec.Code, rec.Body)
	}
	var got CheckinHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body)
	}
	if got.ServerNow == "" || got.Today == "" || got.TZ == "" {
		t.Errorf("服务端时钟字段缺失: %+v", got)
	}
	if !got.Enabled || len(got.Hours) != 2 {
		t.Errorf("排程快照不符: %+v", got)
	}
	if got.NextFireAt == nil || !got.NextFireAt.Equal(next) {
		t.Errorf("next_fire_at 不符: %+v", got.NextFireAt)
	}
	if len(got.Records) != 1 || got.Records[0].Total != 2 || got.Records[0].Already != 1 {
		t.Errorf("历史记录不符: %+v", got.Records)
	}
	// 本地时区偏移：让前端能把 server_now 对齐到自己的时钟。
	if got.TZOffsetSeconds != func() int { _, off := time.Now().Zone(); return off }() {
		t.Errorf("tz_offset_seconds 不符: %d", got.TZOffsetSeconds)
	}
}

// TestCheckinHistoryEndpointNotWired 未接线时仍可用，但字段是空态而非 panic。
func TestCheckinHistoryEndpointNotWired(t *testing.T) {
	h := NewHandler(Config{}) // 不鉴权 + 无 store + 无 provider
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/checkin/history", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d want 200, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// records / hours 必须是 [] 而不是 null。
	if !strings.Contains(body, `"records":[]`) || !strings.Contains(body, `"hours":[]`) {
		t.Errorf("空态应为 [] 而非 null: %s", body)
	}
	var got CheckinHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Enabled || got.NextFireAt != nil {
		t.Errorf("未接线时不该有 enabled/next_fire_at: %+v", got)
	}
}

// TestCheckinHistoryAuthAndOps 鉴权口径：错 key 401；分组密钥 403（含账号清单，
// 不能开给外部调用方）；主密钥（Groups 为空）放行。
func TestCheckinHistoryAuthAndOps(t *testing.T) {
	newH := func() *Handler {
		return NewHandler(Config{
			AuthKeys: []AuthKey{
				{Key: "sk-main", Name: "主密钥"},
				{Key: "sk-ext", Name: "external", Groups: []string{"external"}},
			},
			CheckinHistory: NewCheckinHistoryStore(""),
		})
	}
	get := func(h *Handler, authz string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/checkin/history", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get(newH(), "Bearer nope"); code != http.StatusUnauthorized {
		t.Errorf("错 key: code=%d want 401", code)
	}
	if code := get(newH(), ""); code != http.StatusUnauthorized {
		t.Errorf("无 key: code=%d want 401", code)
	}
	if code := get(newH(), "Bearer sk-ext"); code != http.StatusForbidden {
		t.Errorf("分组密钥: code=%d want 403（不得回吐账号清单）", code)
	}
	if code := get(newH(), "Bearer sk-main"); code != http.StatusOK {
		t.Errorf("主密钥: code=%d want 200", code)
	}
}
