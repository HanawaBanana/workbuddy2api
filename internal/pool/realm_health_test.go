package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// RealmHealth 是 /metrics 的只读数据源（Prometheus 导出）。本文件锁定它的两条语义：
//
//	R1 五个共享字段（Total/Healthy/Cooling/Disabled/InFlightFull）与
//	   CountsDetailed(ForRealm) 逐字同口径——后者已委托本方法，二者不得漂移，
//	   否则 /status 与 /metrics 会报出不一致的账号数，运维据此误判池健康。
//	R2 四个细分字段（Breaker/Degraded/ManualDisabled/ModelCooled）是**原因分解**
//	   而非互斥分类：熔断/降权号既计入 Cooling 也计入对应原因位；ManualDisabled
//	   是 Disabled 的子集；ModelCooled 只计**未过期**的模型级冷却。
//
// 测试用硬编码期望值断言，不与 CountsDetailedForRealm 互比——后者已委托本方法，
// 互比会退化成恒真断言（空洞测试）。跨入口一致性只在最后一条断言里作为**漂移护栏**
// 出现（若将来有人把 countsDetailedForRealm 改回独立实现且口径不同，该断言立即失败）。

// TestRealmHealthBreakdownMatchesHardcodedCounts 混合池按域分解正确，全池 = 分域之和。
func TestRealmHealthBreakdownMatchesHardcodedCounts(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	// cn 域：cn1 healthy、cn2 冷却、cn3 自动禁用
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn3", Domain: "www.codebuddy.cn", AccessToken: "at"})
	// global 域：g1 healthy、g2 手动停用
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at"})

	p.Cooldown("cn2", CoolSoft, time.Hour, "429 rate limit")
	p.Disable("cn3", "session dead")
	if _, changed := p.SetManualDisabled("g2", true, "观察几天"); !changed {
		t.Fatal("SetManualDisabled(g2) 应生效")
	}

	cn := p.RealmHealth("cn")
	if cn.Total != 3 || cn.Healthy != 1 || cn.Cooling != 1 || cn.Disabled != 1 || cn.InFlightFull != 0 {
		t.Errorf("RealmHealth(cn) = %+v want total=3 healthy=1 cooling=1 disabled=1 in_flight_full=0", cn)
	}
	if cn.ManualDisabled != 0 {
		t.Errorf("RealmHealth(cn).ManualDisabled = %d want 0（cn 无手动停用号）", cn.ManualDisabled)
	}

	g := p.RealmHealth("global")
	if g.Total != 2 || g.Healthy != 1 || g.Cooling != 0 || g.Disabled != 1 {
		t.Errorf("RealmHealth(global) = %+v want total=2 healthy=1 cooling=0 disabled=1", g)
	}
	if g.ManualDisabled != 1 {
		t.Errorf("RealmHealth(global).ManualDisabled = %d want 1（g2 是手动停用）", g.ManualDisabled)
	}

	// 全池口径 = 分域之和（R1 的闭合性）。
	all := p.RealmHealth("")
	if all.Total != 5 || all.Healthy != 2 || all.Cooling != 1 || all.Disabled != 2 || all.ManualDisabled != 1 {
		t.Errorf("RealmHealth(\"\") = %+v want total=5 healthy=2 cooling=1 disabled=2 manual_disabled=1", all)
	}

	// 漂移护栏：两个入口必须逐字段一致。
	ct, ch, cc, cd, cf := p.CountsDetailedForRealm("cn")
	if ct != cn.Total || ch != cn.Healthy || cc != cn.Cooling || cd != cn.Disabled || cf != cn.InFlightFull {
		t.Errorf("CountsDetailedForRealm(cn)=(%d,%d,%d,%d,%d) 与 RealmHealth %+v 口径漂移",
			ct, ch, cc, cd, cf, cn)
	}
}

// TestRealmHealthBreakerDegradedBreakdown 熔断与连败降权是「为什么在冷却」的原因分解，
// 与 Cooling 有交集：同一账号既计入 Cooling 也计入原因位。若有人把原因位误实现为
// 互斥分类（"进了 Breaker 就不算 Cooling"），本测试的 Cooling 断言会失败。
func TestRealmHealthBreakerDegradedBreakdown(t *testing.T) {
	p := New("")
	p.SetBreaker(1, time.Hour, time.Hour) // 阈值降到 1：一次 NoteError 即熔断
	p.SetDegrade(1, time.Hour, time.Hour)

	p.Add(&auth.Auth{UID: "b1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "d1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at"})

	p.NoteError("b1")    // 达阈 → breakerUntil
	p.NoteFailures("d1") // 达阈 → degradeUntil

	h := p.RealmHealth("")
	if h.Breaker != 1 {
		t.Errorf("Breaker = %d want 1（b1 熔断中）", h.Breaker)
	}
	if h.Degraded != 1 {
		t.Errorf("Degraded = %d want 1（d1 连败降权中）", h.Degraded)
	}
	if h.Cooling != 2 {
		t.Errorf("Cooling = %d want 2（熔断号与降权号都算非健康，与原因位不互斥）", h.Cooling)
	}
	if h.Healthy != 1 {
		t.Errorf("Healthy = %d want 1（只剩 ok）", h.Healthy)
	}
}

// setModelCooldownUntil 白盒写入一条模型级冷却（仅测试用）。
//
// 为什么需要白盒：CooldownSoftForModel 永远写不出**已过期**条目——它内部把上游
// resetAt 经 cappedSoftUntilLocked 钳制，过去时刻会被钳到 now+1ms（见
// TestRealmHealthModelCooledPastResetAtClamped）。但生产上"map 里躺着过期条目"
// 是真实状态：条目到期后到下一次 pruneExpiredModelCooldowns（pick 写锁路径）之间，
// 或 status 只读遍历跳过的那些条目，都还留在表里。RealmHealth 必须自行过滤。
func (p *Pool) setModelCooldownUntil(uid, model string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{Until: until, Reason: "test"}
}

// TestRealmHealthModelCooledCountsOnlyActive 模型级冷却（6004/11102）只计**未过期**
// 条目：过期条目不得让账号被算作「模型级冷却中」——否则 /metrics 的该 gauge 会随
// 历史模型冷却长期虚高，告警与看板失真。
func TestRealmHealthModelCooledCountsOnlyActive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})

	// u1 走真实入口写有效冷却（6004 带上游重置时间）。
	p.CooldownSoftForModel("u1", time.Hour, time.Now().Add(time.Hour), "glm-5.3", "6004 模型限流")
	// u2 白盒写入一条已过期条目（生产上真实存在的中间态）。
	p.setModelCooldownUntil("u2", "glm-5.3", time.Now().Add(-time.Hour))

	if got := p.RealmHealth("").ModelCooled; got != 1 {
		t.Errorf("ModelCooled = %d want 1（只计未过期条目）", got)
	}
	// 模型级冷却不影响账号级健康：两者账号对其他模型仍可选（issue #31）。
	if h := p.RealmHealth(""); h.Healthy != 2 {
		t.Errorf("Healthy = %d want 2（模型级冷却不摘账号级健康）", h.Healthy)
	}
	// 全部过期 → 归零（反向断言，防止实现退化为"只看 len(map)>0"）。
	p.setModelCooldownUntil("u1", "glm-5.3", time.Now().Add(-time.Hour))
	if got := p.RealmHealth("").ModelCooled; got != 0 {
		t.Errorf("ModelCooled = %d want 0（全部条目已过期）", got)
	}
}

// TestRealmHealthModelCooledPastResetAtClamped 锁定 cappedSoftUntilLocked 的钳制语义：
// 上游下发的 resetAt 已经过期（时钟偏差 / 回放旧响应）时，写出的 Until 是 now+1ms
// 而非过去时刻——即"一写入就过期"的条目不会被造出来，模型级冷却表里不存在
// 从未生效的条目。这条不变量是上面那个测试的前提，单独锁住以免将来被改掉。
func TestRealmHealthModelCooledPastResetAtClamped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	p.CooldownSoftForModel("u1", time.Hour, time.Now().Add(-time.Hour), "glm-5.3", "6004 模型限流")

	if got := p.RealmHealth("").ModelCooled; got != 1 {
		t.Errorf("ModelCooled = %d want 1（过去的 resetAt 被钳到 now+1ms，条目仍短暂有效）", got)
	}
}

// TestRealmHealthInFlightSum InFlight 是各账号在途请求数之和（运行态观测），
// 与 InFlightFull（"已达上限的 healthy 账号数"）是两个维度：max_in_flight=0（不限）时
// Acquire 恒成功、计数仍累加，故 InFlight 可以大于 0 而 InFlightFull 恒为 0。
func TestRealmHealthInFlightSum(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(0) // 0 = 不限
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})

	if !p.Acquire("u1") || !p.Acquire("u1") || !p.Acquire("u2") {
		t.Fatal("max_in_flight=0 时 Acquire 应全部成功")
	}
	defer p.Release("u1")
	defer p.Release("u2")

	h := p.RealmHealth("")
	if h.InFlight != 3 {
		t.Errorf("InFlight = %d want 3（u1×2 + u2×1）", h.InFlight)
	}
	if h.InFlightFull != 0 {
		t.Errorf("InFlightFull = %d want 0（不限在途时永不满载）", h.InFlightFull)
	}

	// 加上限后满载才计入 InFlightFull，且仍是 Healthy 的子集（状态机语义不变）。
	p.SetMaxInFlight(1)
	h = p.RealmHealth("")
	if h.InFlightFull != 2 {
		t.Errorf("InFlightFull = %d want 2（u1/u2 各占满 1）", h.InFlightFull)
	}
	if h.Healthy != 2 {
		t.Errorf("Healthy = %d want 2（占满仍 healthy）", h.Healthy)
	}
}
