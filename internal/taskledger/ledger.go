// Package taskledger 定时任务的执行台账：每类任务「最近一轮」的结构化结果，
// 外加当日失败重试的排期状态，可选落盘（tmp + rename）以便重启后仍能对账。
//
// 为什么单独成包：台账要被 internal/server 读到（/status 的 task_ledger 段与
// /metrics 的任务指标），而 server 包不反向 import internal/scheduler
// （理由见 server/admin_tasks.go 的 TaskRunner 注释）。把「数据结构 + 存储」
// 放在这里，scheduler 负责写入与重试策略、server 负责只读渲染，两边都只依赖
// 本包，不产生环。
//
// 与 internal/server 的 MetricsSnapshot 不重叠：那份是**请求维度**的累计统计，
// 本份是**任务维度**的逐轮结果——台账是唯一事实来源，metrics 不再另存一份累加器。
//
// 为什么需要它（此前只能 grep 日志）：六类任务的执行结果此前只散在日志行里，
// 于是「昨晚 21 点签到跑了吗、成功几个」要么靠人肉翻日志、要么靠猜；服务重启
// （自动更新/改配置）之后连日志都断了一截。台账把每类任务的最近一轮固化成一份
// 可读快照，并给出「本轮是否全灭」这个判据——它同时也是当日失败重试的触发依据。
package taskledger

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定
// +8 即可，不依赖容器 tzdata。口径必须与 scheduler.travelDay 一致——重试计数与
// 上游的「每日」边界对齐，否则会出现「上游已翻篇、我们还在算昨天」的错位。
var cstZone = time.FixedZone("CST", 8*60*60)

// day 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func day(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// 任务轮次的触发来源。值是稳定字符串（会落盘、会出现在 /status 里），不要改。
const (
	TriggerSchedule = "schedule" // 定时排程到点
	TriggerRetry    = "retry"    // 当日失败重试
	TriggerManual   = "manual"   // 人工触发（/admin/tasks/{name}/run 或一次性工具）
)

// Run 一类任务一轮执行的台账记录。
//
// 计数口径（各任务按自己能诚实填的填，不编造）：
//   - Total   本轮涉及的账号数（脚本类任务为命令数）
//   - OK      真正做成的动作数
//   - Already 幂等成功（如「今天已签到」「已领过」）——与 OK 分开是为了区分
//     「20 个号都真签到了」和「12 个真签到 + 8 个早就签过」
//   - Fail    上游报错的账号/命令数
//   - Skipped 有意跳过的（禁用号、无凭证、global 域、门槛未达、在途、已达上限）
type Run struct {
	Kind      string    `json:"kind"`
	Trigger   string    `json:"trigger"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Elapsed   string    `json:"elapsed"`
	Total     int       `json:"total"`
	OK        int       `json:"ok"`
	Already   int       `json:"already"`
	Fail      int       `json:"fail"`
	Skipped   int       `json:"skipped"`
	AllFailed bool      `json:"all_failed"`
	Note      string    `json:"note,omitempty"`
}

// RetryState 一类任务的当日重试状态（按 CST 自然日惰性滚动）。
type RetryState struct {
	Day  string `json:"day"`
	Used int    `json:"used"`
	// Next 下一次重试的计划时刻；nil = 当前无待重试。用指针而非 time.Time：
	// time.Time 是结构体，omitempty 对它无效，零值会渲染成 "0001-01-01T00:00:00Z"。
	Next *time.Time `json:"next,omitempty"`
}

// 不排定重试的原因（Decision.Reason）。稳定字符串，会出现在日志与 /status 里。
const (
	// ReasonDisabled 重试开关关闭（schedule.retry_delay_minutes = 0，缺省）。
	ReasonDisabled = "disabled"
	// ReasonMaxReached 当日重试次数已用尽（schedule.retry_max_per_day）。
	ReasonMaxReached = "max_reached"
	// ReasonNextDay 计划时刻已跨到下一个自然日——不排期。跨日重试会与次日正常
	// 排程撞车，且「当日失败重试」这个语义本身就止于当日。
	ReasonNextDay = "next_day"
)

// Decision 重试排期结论。Reason 为空表示已排期（At 有效）。
//
// 为什么把「为什么不重试」做成返回值而不是只打日志：运维看到「全灭但没重跑」
// 时必须能立刻分辨是配置关着、次数用尽还是跨日了，而不是去翻代码猜。
type Decision struct {
	At     time.Time
	Used   int
	Reason string
}

// file 台账落盘形态。SavedAt 便于人工确认文件的新旧。
type file struct {
	Runs    map[string]Run         `json:"runs"`
	Retry   map[string]*RetryState `json:"retry,omitempty"`
	SavedAt time.Time              `json:"saved_at"`
}

// persistLogEvery 连续落盘失败每 N 次打一条提醒，避免磁盘满/权限丢失时刷屏
// （与 pool.notePersistFail 同口径）。
const persistLogEvery = 100

// Store 台账存储。零值不可用，必须经 New 构造。
//
// 并发：全部状态在 mu 之下；Record / PlanRetry / ConsumeRetry 会落盘。
type Store struct {
	mu    sync.Mutex
	path  string // 空 = 纯内存（不落盘）
	runs  map[string]Run
	retry map[string]*RetryState

	fails int // 连续落盘失败次数（仅用于节流日志）
}

// New 构造台账并尝试载入 path 处的历史（path 为空则纯内存）。
//
// 载入失败（文件不存在/损坏/不可读）一律静默按空台账启动：台账是**观测**，
// 不是关键路径，不该让服务起不来（与 admin 审计的 fail-fast 是刻意相反的选择——
// 那份是安全特性，空着等于安全承诺失效；这份只是少一段历史）。
func New(path string) *Store {
	s := &Store{
		path:  path,
		runs:  map[string]Run{},
		retry: map[string]*RetryState{},
	}
	s.load()
	return s
}

// Runs 返回各类任务最近一轮记录的副本（调用方可安全读，不受后续写入影响）。
func (s *Store) Runs() map[string]Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Run, len(s.runs))
	for k, v := range s.runs {
		out[k] = v
	}
	return out
}

// RetryStates 返回各类任务的当日重试状态副本。
func (s *Store) RetryStates() map[string]RetryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RetryState, len(s.retry))
	for k, v := range s.retry {
		out[k] = *v
	}
	return out
}

// Record 记录一类任务的一轮执行结果（覆盖该类上一轮），并落盘。
func (s *Store) Record(r Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(time.Now())
	s.runs[r.Kind] = r
	s.saveLocked()
}

// PlanRetry 为 kind 排定一次当日失败重试，delay 为延迟，max 为当日上限。
//
// 排期成功时 Used 递增（**排期即计数**，不是执行才计数）：这样即便服务在重试
// 触发前被重启，也不会因为「还没跑」而反复补排、把上游多打几轮。
//
// 拒绝排期的三种情形见 Reason* 常量，都会在 Reason 里如实说明。
func (s *Store) PlanRetry(kind string, now time.Time, delay time.Duration, max int) Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	st := s.slotLocked(kind, now)
	switch {
	case delay <= 0 || max <= 0:
		return Decision{Used: st.Used, Reason: ReasonDisabled}
	case st.Used >= max:
		return Decision{Used: st.Used, Reason: ReasonMaxReached}
	}
	at := now.Add(delay)
	if day(at) != day(now) {
		return Decision{Used: st.Used, Reason: ReasonNextDay}
	}
	st.Used++
	st.Next = &at
	s.saveLocked()
	return Decision{At: at, Used: st.Used}
}

// ArmedRetries 返回**所有已排期的重试**：kind → 计划时刻（含尚未到点的）。
//
// 为什么连未到点的也要返回（而不是只给「到点」的）：调用方拿它参与「下一个唤醒
// 时刻」的计算，若只给到点的，未到点的重试就不会成为唤醒源——主循环会一直睡到
// 下一个**正常**时点，那次重试要么被推迟到那个时点（迟到）、要么与正常轮次撞在
// 一起。把它当成普通唤醒源，重试才会在自己的时刻准点触发。
//
// 计划时刻可能已过（进程忙、刚启动）：返回计划时刻而非 now，让上层算出的唤醒
// 时刻落在过去，timer 立即到期 —— 等价于「立刻补跑」。
//
// 不消费：消费由 ConsumeRetry 负责，且它**只认已到点**的重试——两者判据不同是
// 刻意的：唤醒计算要看见未来，消费只能吃到现在（否则正常槽位先到时会把一次
// 尚未到点的重试误当成已执行）。跨日滚动是这里唯一的写动作，只重置计数、
// 不影响「是否已排期」的判定。
func (s *Store) ArmedRetries(now time.Time) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	out := map[string]time.Time{}
	for k, st := range s.retry {
		if st.Next == nil {
			continue
		}
		out[k] = *st.Next
	}
	return out
}

// ConsumeRetry 把 kind 已到点的重试标记为已消费，返回是否确实消费了一次。
//
// 判据刻意比 ArmedRetries 严：**只认已到点**（Next 非零且不晚于 now）。两者
// 分工不同——ArmedRetries 供唤醒计算（要看见未来），本函数供「本轮是不是一次
// 重试」的判定（只能吃到现在）。若这里也认未来的重试，正常槽位先到时会把一次
// 尚未到点的重试误消费掉，那次重试就静默消失了。
//
// **不在 nextWake 里消费**：主循环拿到唤醒时刻后可能因 ctx 取消而放弃派发
// （优雅停机），那时重试应保持待命、下一轮再补——在 nextWake 里消费会让这次
// 重试静默丢失。
func (s *Store) ConsumeRetry(kind string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	st := s.retry[kind]
	if st == nil || st.Next == nil || st.Next.After(now) {
		return false
	}
	st.Next = nil
	s.saveLocked()
	return true
}

// slotLocked 取（必要时创建）kind 的当日重试状态。新建时用 now 定日期，与调用方
// 刚刚 rollLocked 用的时刻同源（不另取 time.Now()，避免跨零点时两处日期不一致）。
// 调用方必须已持 mu。
func (s *Store) slotLocked(kind string, now time.Time) *RetryState {
	st := s.retry[kind]
	if st == nil {
		st = &RetryState{Day: day(now)}
		s.retry[kind] = st
	}
	return st
}

// rollLocked 惰性跨日滚动：CST 自然日变了就把各类任务的 Used/Next 清零。
//
// 用惰性滚动而非 00:00 定时器：定时器会在重启后丢失，且多一个 goroutine 要管；
// 惰性滚动的语义是「下一次访问时按当前日期算」，重启后第一次访问自然对齐。
// 调用方必须已持 mu。
func (s *Store) rollLocked(now time.Time) {
	today := day(now)
	for _, st := range s.retry {
		if st.Day != today {
			*st = RetryState{Day: today}
		}
	}
}

// load 从 path 读回台账（无文件/解析失败静默跳过，按空台账启动）。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var f file
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	for k, v := range f.Runs {
		s.runs[k] = v
	}
	for k, v := range f.Retry {
		if v == nil {
			continue
		}
		s.retry[k] = v
	}
}

// saveLocked 原子落盘（tmp + rename，0o600）。path 为空时是空操作。
// 失败只记节流日志、不向上抛——台账写不进去不该影响任务执行本身。
// 调用方必须已持 mu。
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(file{Runs: s.runs, Retry: s.retry, SavedAt: time.Now()}, "", "  ")
	if err != nil {
		s.noteFail(err)
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		s.noteFail(err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.noteFail(err)
		return
	}
	if s.fails > 0 {
		log.Printf("[taskledger] 台账落盘恢复（此前连续失败 %d 次）", s.fails)
		s.fails = 0
	}
}

// noteFail 记录一次落盘失败并按节流规则决定是否打日志：首败详报（含路径与错误），
// 随后每 persistLogEvery 次再复报一条。与 pool.notePersistFail 同范式。
func (s *Store) noteFail(err error) {
	if s.fails == 0 {
		log.Printf("WARN: [taskledger] 台账落盘失败（首次详报）: path=%s err=%v（台账退化为内存态，任务执行不受影响）", s.path, err)
	} else if s.fails%persistLogEvery == 0 {
		log.Printf("ERR: [taskledger] 台账连续落盘失败 %d 次: path=%s err=%v", s.fails, s.path, err)
	}
	s.fails++
}
