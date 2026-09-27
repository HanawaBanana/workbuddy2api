// ledger.go 调度器侧的台账接线与当日失败重试策略。
//
// 分工：internal/taskledger 只管「数据结构 + 存储 + 排期机械动作」，
// 本文件管**策略**——什么算全灭、什么轮次才允许排重试、重试怎么进主循环。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/taskledger"
)

// 轮次触发来源（值是稳定字符串，会落盘并出现在 /status 里）。
const (
	triggerSchedule = taskledger.TriggerSchedule
	triggerRetry    = taskledger.TriggerRetry
	triggerManual   = taskledger.TriggerManual
)

// runTally 一轮执行的计数收集器：台账的唯一数据来源。
//
// 各任务在自己的遍历里累加，结束后交给 recordRun 一次性落账——不在这里做任何
// 二次推断（比如从日志行反推成功数），避免口径分叉。
type runTally struct {
	total, ok, already, fail, skipped int
	note                              string
	// interrupted 本轮被 ctx 取消打断（优雅停机），计数是**部分**的。
	// 此时不判全灭、不排重试：一轮只跑了 3 个号就停机，不代表上游全挂。
	interrupted bool
}

// recordRun 结束一轮执行：写台账，必要时排定当日失败重试，并返回这条记录。
//
// 全灭判据（AllFailed）：**有失败，且没有任何一个账号真正做成**（ok+already==0）。
//
// 为什么不是「失败率超阈值」：只要还有账号成功，就证明上游是通的，失败是账号级
// 的（token 过期、单号被限流），重试只会对同一批注定失败的账号再打一遍上游——
// 既无收益，又正好撞在本仓库一直避免的风控雷区上。反过来「一个都没成」几乎只
// 有一个解释：网络/DNS 未就绪、上游整体抖动、WAF 拦截——这正是重试能救的场景。
// 这条判据同时保证了「成功路径零新增上游请求」。
//
// 人工触发（triggerManual）**不排重试**：运维刚点过一次，自己会判断要不要再点；
// 自动重跑会让一次人工操作静默变成两次上游写。
func (s *Scheduler) recordRun(kind taskKind, trigger string, started time.Time, t runTally) taskledger.Run {
	finished := time.Now()
	r := taskledger.Run{
		Kind:    kind.String(),
		Trigger: trigger,
		Started: started, Finished: finished,
		Elapsed: finished.Sub(started).Round(time.Millisecond).String(),
		Total:   t.total, OK: t.ok, Already: t.already, Fail: t.fail, Skipped: t.skipped,
		Note: t.note,
	}
	r.AllFailed = !t.interrupted && r.Fail > 0 && r.OK+r.Already == 0
	if t.interrupted {
		r.Note = joinDetail(r.Note, "interrupted")
	}
	if s.ledger == nil {
		return r // 手搓的零值 Scheduler（测试）：无台账，也排不了重试
	}
	s.ledger.Record(r)

	if trigger == triggerManual || !r.AllFailed {
		return r
	}
	delay := time.Duration(s.cfg.RetryDelayMinutes) * time.Minute
	d := s.ledger.PlanRetry(kind.String(), finished, delay, s.cfg.RetryMaxPerDay)
	if d.Reason == "" {
		log.Printf("WARN: %s 本轮全灭（fail=%d ok=%d already=%d）—— %s 自动重试（当日第 %d 次，schedule.retry_delay_minutes）",
			kind, r.Fail, r.OK, r.Already, d.At.Format("15:04"), d.Used)
		return r
	}
	// 全灭但没排上：必须说清是哪一种，否则运维只会看到「全灭」然后困惑为什么没补跑。
	log.Printf("WARN: %s 本轮全灭（fail=%d）但未安排重试：%s（schedule.retry_delay_minutes / schedule.retry_max_per_day）",
		kind, r.Fail, d.Reason)
	return r
}

// kindByName 把任务名映射回 taskKind（台账以字符串为键，主循环需要枚举值）。
func kindByName(name string) (taskKind, bool) {
	switch name {
	case "checkin":
		return taskCheckin, true
	case "travel":
		return taskTravel, true
	case "activity":
		return taskActivity, true
	case "keepalive":
		return taskKeepalive, true
	case "school":
		return taskSchool, true
	case "cat":
		return taskCat, true
	}
	return 0, false
}

// kindDisabled 该类任务是否已被显式禁用。
func (s *Scheduler) kindDisabled(k taskKind) bool {
	switch k {
	case taskCheckin:
		return s.cfg.CheckinDisabled
	case taskTravel:
		return s.cfg.TravelDisabled
	case taskActivity:
		return s.cfg.ActivityDisabled
	case taskKeepalive:
		return s.cfg.KeepaliveDisabled
	case taskSchool:
		return s.cfg.SchoolDisabled
	case taskCat:
		return s.cfg.CatDisabled
	}
	return true
}

// takeRetryTrigger 判定本轮 dispatch 是不是一次已到点的当日重试，并消费掉它。
//
// 消费判据（ConsumeRetry：Next 非零且不晚于 now）刻意比 nextWake 用的
// ArmedRetries 严：唤醒计算要看见未来（否则未到点的重试没有唤醒源），而消费只能
// 吃到现在——否则某个正常槽位先到时，会把一次尚未到点的重试误当成已执行。
// 反过来，若本轮确实是重试时刻（或与正常槽位重合），这里必然消费成功。
func (s *Scheduler) takeRetryTrigger(k taskKind, now time.Time) bool {
	if s.ledger == nil || !s.ledger.ConsumeRetry(k.String(), now) {
		return false
	}
	log.Printf("retry %s: 当日失败重试补跑（schedule.retry_delay_minutes）", k)
	return true
}
