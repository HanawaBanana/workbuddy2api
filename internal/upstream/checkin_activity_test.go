package upstream

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 契约断言：必须 POST（同路径 GET 返回 404 —— 本文件曾因误用 GET 而整个功能静默失效），
// 路径正确，且带上认证头。
func assertCheckinActivityRequest(r *http.Request, wantPath string) error {
	if r.Method != http.MethodPost {
		return errors.New("want POST, got " + r.Method)
	}
	if r.URL.Path != wantPath {
		return errors.New("wrong path: " + r.URL.Path)
	}
	if r.Header.Get("Authorization") != "Bearer at" {
		return errors.New("missing Authorization")
	}
	if r.Header.Get("X-User-Id") != "u1" {
		return errors.New("missing X-User-Id")
	}
	return nil
}

// checkinActivityBody 上游实测响应（2026-09-28）。
const checkinActivityBody = `{"code":0,"msg":"OK","data":{` +
	`"active":true,"today_checked_in":true,"streak_days":13,` +
	`"daily_credit":100,"today_credit":100,"total_credits":1300,` +
	`"start_time":"2026-09-16 00:00:00","end_time":"2026-09-29 23:59:59",` +
	`"theme_name":"Buddy加油站","activity_name":"高校新生攻略","season":9}}`

func TestCheckinActivityStatusParsesFields(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if err := assertCheckinActivityRequest(r, "/v2/billing/meter/checkin-activity-status"); err != nil {
			return nil, err
		}
		return jsonResp(200, checkinActivityBody), nil
	})
	st, err := c.CheckinActivityStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("checkin activity status: %v", err)
	}
	if !st.Active || !st.TodayCheckedIn || st.StreakDays != 13 || st.DailyCredit != 100 {
		t.Errorf("unexpected fields: %+v", st)
	}
	if st.ActivityName != "高校新生攻略" || st.Season != 9 {
		t.Errorf("unexpected meta: name=%q season=%d", st.ActivityName, st.Season)
	}
	end, ok := st.EndsAt()
	if !ok {
		t.Fatalf("EndsAt: want ok, got false (end_time=%q)", st.EndTime)
	}
	if end.Year() != 2026 || end.Month() != time.September || end.Day() != 29 {
		t.Errorf("EndsAt = %v, want 2026-09-29", end)
	}
	// 上游时间串无时区后缀，必须按 CST(+8) 解释：23:59:59 CST == 15:59:59 UTC。
	if h := end.UTC().Hour(); h != 15 {
		t.Errorf("EndsAt UTC hour = %d, want 15（应按 +8 时区解析，而非 UTC 或本地时区）", h)
	}
}

// TestCheckinActivityStatusBusinessError 上游业务错误应归一为 *Error，调用方据此只记日志、不罚号。
func TestCheckinActivityStatusBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":400,"msg":"activity not found","data":null}`), nil
	})
	_, err := c.CheckinActivityStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T (%v)", err, err)
	}
	if ue.Status != 400 {
		t.Errorf("status = %d, want 400", ue.Status)
	}
}

// TestCheckinActivityTimeParsing 表驱动覆盖时间解析边界。
// 特别收录 RFC3339 形态 —— 本文件曾误以为上游用 RFC3339，实际是 "2006-01-02 15:04:05"；
// 该用例固化「RFC3339 必须解析失败」，防止再退回错格式。
func TestCheckinActivityTimeParsing(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		wantOK bool
		wantY  int
	}{
		{"上游实际格式", "2026-09-29 23:59:59", true, 2026},
		{"上游实际格式-起始", "2026-09-16 00:00:00", true, 2026},
		{"空串", "", false, 0},
		{"RFC3339 带偏移（上游并不用）", "2026-09-29T23:59:59+08:00", false, 0},
		{"只有日期", "2026-09-29", false, 0},
		{"斜杠分隔", "2026/09/29 23:59:59", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &CheckinActivity{StartTime: tc.value, EndTime: tc.value}
			for _, fn := range []func() (time.Time, bool){st.StartsAt, st.EndsAt} {
				got, ok := fn()
				if ok != tc.wantOK {
					t.Fatalf("ok = %v, want %v (value=%q)", ok, tc.wantOK, tc.value)
				}
				if ok && got.Year() != tc.wantY {
					t.Errorf("year = %d, want %d", got.Year(), tc.wantY)
				}
			}
		})
	}
}
