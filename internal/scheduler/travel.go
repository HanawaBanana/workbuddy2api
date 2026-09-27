// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelOutcome 单账号单趟巡检的结果分类（台账计数用，见 ledger.go 的 runTally）。
//
// 为什么要把「跳过」与「失败」分开：跳过是**预期**的正常态（在途、已达当日上限、
// 领养对话量未达门槛、防抖挡下），把它们算成失败会让台账天天报「全灭」并触发
// 毫无意义的当日重试；只有真正打上游报错才算失败。
type travelOutcome int

const (
	travelOK      travelOutcome = iota // 做成了一个动作：领养 / 派出 / 领奖
	travelSkipped                      // 有意跳过（预期正常态）
	travelFailed                       // 上游报错
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// activityReportGap 同一账号内连续上报之间的间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var activityReportGap = 1500 * time.Millisecond

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等 sleep 醒来），
// 等满返回 true。d<=0 立即放行（测试把延迟置 0 时不白等）。
// 替换 time.Sleep：账号间/账号内限速值不变，只换等待方式。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。口径必须与 internal/taskledger 的 day() 一致——两处都表示
// 「上游的今天」，不一致会让重试计数与上游的每日边界错位。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检（无 ctx 的外部入口：
// 测试与潜在的手动触发——cmd 侧从未接线，不存在 cmd/travel 入口，部署验证
// 场景由 RunActivityNow 覆盖）。内部走 runTravel，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunTravelNow() {
	s.runTravel(context.Background(), triggerManual)
}

// runTravel 旅行巡检遍历，随 ctx 取消立即退出。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay（sleepCtx：取消时立即放弃后续账号）。
//
// 计数口径：Total 为真正发起巡检的账号数，其余按 travelOutcome 归入 ok/skipped/fail。
// 被 ctx 取消打断时标 interrupted——计数是部分的，不判全灭（见 recordRun）。
func (s *Scheduler) runTravel(ctx context.Context, trigger string) {
	started := time.Now()
	var t runTally
	defer func() { s.recordRun(taskTravel, trigger, started, t) }()

	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			t.skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			t.skipped++
			continue
		}
		if a.IsGlobal() {
			t.skipped++ // D4 门控：global 无猫猫旅行体系，不发起任何上游调用
			continue
		}
		if !first {
			if !sleepCtx(ctx, travelAccountDelay) {
				t.interrupted = true
				return // 优雅停机：不等限速睡满，剩余账号下轮再巡
			}
		}
		first = false
		t.total++
		switch s.travelOne(a) {
		case travelOK:
			t.ok++
		case travelFailed:
			t.fail++
		default:
			t.skipped++
		}
	}
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
// 返回结果分类供台账计数（不改变任何上游调用序列）。
func (s *Scheduler) travelOne(a *auth.Auth) travelOutcome {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelFailed
	}
	if buddy == nil {
		return s.travelAdopt(a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelFailed
	}
	switch ts.State {
	case travelStateArrived:
		return s.travelClaim(a, ts)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
		return travelSkipped
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
		return travelSkipped
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) travelOutcome {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return travelSkipped
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelFailed
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
	return travelOK
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) travelOutcome {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return travelSkipped
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return travelFailed
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
	return travelOK
}

// travelAdopt 旅行巡检时领养：受 adoptTriedToday 当日防抖约束。
func (s *Scheduler) travelAdopt(a *auth.Auth) travelOutcome {
	return s.adoptBuddy(a, false)
}

// travelAdoptForce 活跃上报补满对话量后领养：豁免 adoptTriedToday 当日防抖。
// 背景：旅行排程 09 点已领养且因对话量未达 skip，10 点活跃上报 5 连发把
// 对话量补满——此时是「门槛刚达成」的新状态，不算对上游重试轰炸，放行重试。
// 有猫账号 BuddyInfo 非空时直接跳过（不重复领养）。
//
// 结果不进活跃上报的台账（那是活动上报自己的账），故丢弃返回值。
func (s *Scheduler) travelAdoptForce(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("activity %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if buddy != nil {
		return // 已有猫，无需领养
	}
	s.adoptBuddy(a, true) // force=true 豁免当日防抖
}

// adoptBuddy 无猫时领养：先同意协议（幂等）再 buddy/first。
// conversation 门槛未达标（HTTP 400 first_buddy task not completed yet）属预期行为，
// 记一次当日已试后静默跳过，不再重试。force=true 时豁免当日防抖（活跃上报补满对话量后重试）。
func (s *Scheduler) adoptBuddy(a *auth.Auth, force bool) travelOutcome {
	if !force && s.adoptTriedToday(a.UID) {
		return travelSkipped
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelFailed
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
		return travelOK
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
		return travelSkipped
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelFailed
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
