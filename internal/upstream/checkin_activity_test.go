package upstream

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// checkinActivityPath 断言打到 billing 域的正确路径（CN 走 /v2 前缀，与
// checkinMeterPaths 对 daily-checkin 的口径一致）。
func assertCheckinActivityPath(r *http.Request, want string) error {
	if r.Method != http.MethodGet {
		return errors.New("want GET, got " + r.Method)
	}
	if r.URL.Path != want {
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

func TestCheckinActivityStatusParsesFields(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if err := assertCheckinActivityPath(r, "/v2/billing/meter/checkin-activity-status"); err != nil {
			return nil, err
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{`+
			`"enabled":true,`+
			`"start_at":"2026-08-24T00:00:00+08:00",`+
			`"end_at":"2026-10-31T23:59:59+08:00",`+
			`"credits":100,`+
			`"validity_days":90,`+
			`"server_time":"2026-09-27T19:27:49+08:00"}}`), nil
	})
	st, err := c.CheckinActivityStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("checkin activity status: %v", err)
	}
	if !st.Enabled || st.Credits != 100 || st.ValidityDays != 90 {
		t.Errorf("unexpected fields: %+v", st)
	}
	end, ok := st.EndTime()
	if !ok {
		t.Fatalf("EndTime: want ok, got false (end_at=%q)", st.EndAt)
	}
	if end.Year() != 2026 || end.Month() != time.October || end.Day() != 31 {
		t.Errorf("EndTime = %v, want 2026-10-31", end)
	}
}

// TestCheckinActivityStatusBusinessError 上游业务错误（HTTP 400 + code!=0）应归一为
// *Error，调用方据此只记日志、不罚号（与 travel/growth 同口径）。
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

// TestCheckinActivityEndTime 表驱动覆盖解析边界：缺失/空串/非法格式一律返回 false
// （调用方约定：解析不出就不预警，绝不猜）。
func TestCheckinActivityEndTime(t *testing.T) {
	cases := []struct {
		name   string
		endAt  string
		wantOK bool
		wantY  int
	}{
		{"rfc3339 带偏移", "2026-10-31T23:59:59+08:00", true, 2026},
		{"rfc3339 UTC", "2027-03-31T23:59:59Z", true, 2027},
		{"空串", "", false, 0},
		{"非 RFC3339", "2026-10-31 23:59:59", false, 0},
		{"只有日期", "2026-10-31", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &CheckinActivity{EndAt: tc.endAt}
			got, ok := st.EndTime()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Year() != tc.wantY {
				t.Errorf("year = %d, want %d", got.Year(), tc.wantY)
			}
		})
	}
}
