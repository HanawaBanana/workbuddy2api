// checkin_activity.go 签到活动状态（只读）。
//
// 定位：daily-checkin 只回答「今天签没签」，不回答「这个签到活动还能签多久」。
// 上游一旦结束活动，签到会直接开始报错——那时才发现就晚了。
// checkin-activity-status 是唯一能提前拿到活动结束时间（end_at）的途径。
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

// CheckinActivity 签到活动状态（实测 2026-09-27）。
//
// 字段口径：enabled 活动是否开启；start_at/end_at 活动起止（RFC3339 带时区）；
// credits 每日签到面额；validity_days 领取积分的有效期天数；server_time 服务端当前时间。
type CheckinActivity struct {
	Enabled      bool   `json:"enabled"`
	StartAt      string `json:"start_at"`
	EndAt        string `json:"end_at"`
	Credits      int    `json:"credits"`
	ValidityDays int    `json:"validity_days"`
	ServerTime   string `json:"server_time"`
}

// EndTime 解析 end_at（RFC3339）；字段缺失或不可解析返回零值 + false。
// 调用方据此决定是否预警——解析不出就不预警，绝不猜。
func (c *CheckinActivity) EndTime() (time.Time, bool) {
	if c.EndAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, c.EndAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CheckinActivityStatus 查询签到活动状态（只读）。
func (c *Client) CheckinActivityStatus(a *auth.Auth) (*CheckinActivity, error) {
	paths := []string{checkinActivityPathV2}
	if c.globalOn(a) {
		paths = []string{checkinActivityPath, checkinActivityPathV2}
	}
	data, err := c.billingMeterJSON(a, paths, http.MethodGet, nil)
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
