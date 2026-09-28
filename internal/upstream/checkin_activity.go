// checkin_activity.go 签到活动状态（只读）。
//
// 定位：daily-checkin 只回答「今天签没签」，不回答「这个签到活动还能签多久」。
// 上游一旦结束活动，签到会直接开始报错——那时才发现就晚了。
// checkin-activity-status 是唯一能提前拿到活动结束时间（end_time）的途径。
//
// 契约（实测 2026-09-28，CN 与 global 同形）：
//
//	POST /v2/billing/meter/checkin-activity-status   body: {}
//	→ data.active / today_checked_in / streak_days / daily_credit / total_credits
//	  / start_time / end_time / theme_name / activity_name / season
//
// 两个易错点（本文件曾因此失效，勿回退）：
//   - 必须 POST：同路径 GET 返回 404（上游路由只挂 POST），body 传空对象 {}。
//   - 时间不是 RFC3339：形如 "2026-09-29 23:59:59"，无时区后缀，按 CST 解析。
//
// 路径与 daily-checkin 同族（/billing/meter/*）：CN 走 /v2 前缀；
// global 首选无 /v2、404 再回落（与 checkinMeterPaths 同口径）。
// 本方法只读、无副作用，不参与账号惩罚（调用方失败只记日志）。
package upstream

import (
	"encoding/json"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// billing/meter 域「签到活动状态」路径候选（R9：国际版无 /v2 前缀）。
const (
	checkinActivityPath   = "/billing/meter/checkin-activity-status"    // global 首选
	checkinActivityPathV2 = "/v2/billing/meter/checkin-activity-status" // CN 现状 / global fallback
)

// checkinActivityTimeLayout 上游活动起止时间的格式：无时区后缀，按 CST 解释。
const checkinActivityTimeLayout = "2006-01-02 15:04:05"

// checkinActivityZone 活动时间的隐含时区（上游增长体系按 CST 自然日刷新）。
var checkinActivityZone = time.FixedZone("CST", 8*3600)

// CheckinActivity 签到活动状态（实测 2026-09-28）。
type CheckinActivity struct {
	Active         bool   `json:"active"`           // 活动是否进行中
	TodayCheckedIn bool   `json:"today_checked_in"` // 今日是否已签
	StreakDays     int    `json:"streak_days"`      // 连续签到天数
	DailyCredit    int    `json:"daily_credit"`     // 每日签到面额
	TodayCredit    int    `json:"today_credit"`     // 今日已得
	TotalCredits   int    `json:"total_credits"`    // 本轮累计
	StartTime      string `json:"start_time"`       // 形如 "2026-09-16 00:00:00"
	EndTime        string `json:"end_time"`         // 形如 "2026-09-29 23:59:59"
	ThemeName      string `json:"theme_name"`       // 如 "Buddy加油站"
	ActivityName   string `json:"activity_name"`    // 如 "高校新生攻略"
	Season         int    `json:"season"`
}

// parseCheckinActivityTime 解析活动起止时间（非 RFC3339，无时区后缀，按 CST）。
// 字段缺失或不可解析返回零值 + false——调用方据此跳过预警，绝不猜。
func parseCheckinActivityTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(checkinActivityTimeLayout, s, checkinActivityZone)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// StartsAt 解析 start_time；不可解析返回零值 + false。
func (c *CheckinActivity) StartsAt() (time.Time, bool) { return parseCheckinActivityTime(c.StartTime) }

// EndsAt 解析 end_time；不可解析返回零值 + false。
func (c *CheckinActivity) EndsAt() (time.Time, bool) { return parseCheckinActivityTime(c.EndTime) }

// CheckinActivityStatus 查询签到活动状态（POST + 空对象 body；只读）。
func (c *Client) CheckinActivityStatus(a *auth.Auth) (*CheckinActivity, error) {
	paths := []string{checkinActivityPathV2}
	if c.globalOn(a) {
		paths = []string{checkinActivityPath, checkinActivityPathV2}
	}
	data, err := c.billingMeterJSON(a, paths, http.MethodPost, map[string]any{})
	if err != nil {
		return nil, err
	}
	var st CheckinActivity
	if len(data) > 0 {
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, err
		}
	}
	return &st, nil
}
