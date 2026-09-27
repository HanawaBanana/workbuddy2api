// checkin_activity.go 签到活动到期预警。
//
// 每轮签到后顺带取一次活动状态（只读），把「签到活动结束」从"事后发现"变成"提前知道"：
// 距 end_at 不足 checkinActivityWarnDays 天时打 WARN，活动已关闭时也打 WARN。
//
// 取值策略：活动状态是**活动级**的（同一 realm 下所有账号一致），因此只取第一个可用
// CN 账号，不遍历全池——避免用一个只读探测把上游请求量放大 N 倍。
//
// 失败语义：只读探测，失败仅记日志，**不罚账号、不计入签到台账**（它不属于签到链路，
// 失败不该让一轮签到被判"部分失败"）。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// checkinActivityWarnDays 距活动结束多少天内开始预警。
const checkinActivityWarnDays = 7

// warnCheckinActivity 取一次签到活动状态并做临近到期预警。
func (s *Scheduler) warnCheckinActivity(statuses []pool.Status) {
	var a *auth.Auth
	for _, st := range statuses {
		if st.Disabled {
			continue
		}
		if au := s.cfg.Pool.AuthByUID(st.UID); au != nil && !au.IsGlobal() {
			a = au
			break
		}
	}
	if a == nil {
		return // 无可用 CN 账号：静默跳过（不是错误）
	}
	st, err := s.cfg.Upstream.CheckinActivityStatus(a)
	if err != nil {
		log.Printf("WARN: checkin-activity-status 查询失败（忽略，不影响签到）: %v", err)
		return
	}
	if !st.Enabled {
		log.Printf("WARN: 签到活动已关闭（enabled=false）——签到将不再发积分")
		return
	}
	end, ok := st.EndTime()
	if !ok {
		return // end_at 缺失或不可解析：不猜、不预警
	}
	if left := time.Until(end); left <= checkinActivityWarnDays*24*time.Hour {
		log.Printf("WARN: 签到活动将于 %s 结束（剩 %s）——到期后签到不再发积分",
			end.Format("2006-01-02"), left.Round(time.Hour))
	}
}
