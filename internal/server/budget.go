// budget.go 当日积分预算闸（admission control）。
//
// 动机：网关烧的是账号积分。一个跑飞的客户端、一个忘了关的脚本挂在网关上时，
// 积分可能在几小时内见底，而此前没有任何东西会踩刹车——只能事后从日报里发现。
// 本闸在当日累计扣费（上游 usage.credit）达到阈值后拒掉后续对话请求，把损失
// 截断在阈值附近。
//
// 设计要点：
//   - **默认关闭**（config budget.daily_credit_limit 为 0）：0 是「不限」的哨兵值，
//     不是「未设置」，normalize 不把它回落成默认。缺省行为与引入前逐字一致。
//   - **只统计观测到的扣费**：上游 usage 缺 credit 字段时不计数（缺失≠0，与成本
//     账本、/v1/stats 同一纪律）。把「没观测到」当 0 会让用量永远涨不上去、闸形同
//     虚设。代价是当日用量是一个**下界**——真实扣费只会更多，故阈值要按保守值设。
//   - **按 CST 自然日重置**：上游增长体系按 CST 自然日刷新（与 scheduler 的
//     travelDay 同口径）。中国无夏令时，固定 +8 偏移即可。
//   - **进程内计数、重启清零**：不落盘。重启会让当日已用归零、闸短暂失效，这是
//     刻意的取舍——落盘的计数器要么引入与 state.json 竞争的写路径，要么在崩溃后
//     不可信。宁可重启后少挡一会儿，也不要一个会算错的钱闸。
//   - **闸在读 body 之前**：被拒的请求不该先把几十 MB 请求体读进内存再丢掉。
//   - **不改变选号**：本闸只决定「这一枪开不开」，不做「换便宜号」的降级——后者
//     会动到选号主路径的语义（粘性/分层/加权），是另一件事。
package server

import (
	"sync"
	"time"
)

// cstZone CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可。
//
// 与 internal/scheduler 的 cstZone 是**刻意重复**：本项目约定 server 包不 import
// scheduler（见 admin_tasks.go 的 TaskRunner 注释）。一个不含夏令时的固定偏移，
// 两处各写一份的走样风险为零，为此新开一个共享包不划算。
var cstZone = time.FixedZone("CST", 8*60*60)

// cstDay 返回 t 落在哪个 CST 自然日（"2006-01-02"），用作计数器的归属日。
func cstDay(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// dailyBudget 当日积分预算闸。并发安全（对话请求会并发读它）。
type dailyBudget struct {
	mu       sync.Mutex
	limit    float64 // <=0 = 关闭（不限）
	day      string  // used/rejected 所属的 CST 自然日
	used     float64 // 当日累计观测到的 credit
	rejected int64   // 当日因超预算被拒的请求数
}

// newDailyBudget 构造预算闸。limit <=0 表示不限。
func newDailyBudget(limit float64) *dailyBudget {
	return &dailyBudget{limit: limit, day: cstDay(time.Now())}
}

// admit 判定是否放行一次对话请求，并对「拒绝」计数。
//
// 这是个有副作用的方法，不是纯查询：闸必须能回答「今天挡了多少次」，否则运维只
// 看到服务在拒请求、不知道是不是预算造成的（也可能是账号全挂了）。判据是**严格
// 小于**——used == limit 时已经到顶，不再放行。
func (b *dailyBudget) admit() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if b.limit <= 0 {
		return true
	}
	if b.used < b.limit {
		return true
	}
	b.rejected++
	return false
}

// add 累加一次请求观测到的扣费。
//
// observed=false（上游未给 credit）时不累加，但要照常滚日——否则跨天后第一条
// 无 credit 的请求不会触发重置，计数会一直挂在昨天。
func (b *dailyBudget) add(credit float64, observed bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if !observed {
		return
	}
	// 扣费恒 ≥0；上游字段异常给负值时宁可少算，也不让它倒扣当日用量（倒扣会把
	// 已经关上的闸重新打开）。
	if credit > 0 {
		b.used += credit
	}
}

// snapshot 返回当日已用、上限与拒绝次数（供 /status 透出）。
func (b *dailyBudget) snapshot() (used, limit float64, rejected int64) {
	if b == nil {
		return 0, 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.used, b.limit, b.rejected
}

// rollLocked 跨日则归零。调用方必须已持锁。
//
// 用**惰性滚动**（下次访问时发现日期变了就归零）而不是起一个 00:00 的定时器：
// 定时器在重启后会丢，要么得自己再补一套调度；而「读的时候对一下日期」在任何
// 访问路径上都成立，零后台开销、零新增 goroutine。
func (b *dailyBudget) rollLocked() {
	if today := cstDay(time.Now()); today != b.day {
		b.day = today
		b.used = 0
		b.rejected = 0
	}
}
