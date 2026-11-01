// Package record 记录定时任务的执行结果（签到 / 猫猫旅行 / token 保活）。
//
// 为什么需要它：这些任务的成败原先只打到 stdout（log.Printf），容器日志一滚动
// 就查不到历史，面板也无从展示。用户在网页上想确认「昨天签到成功了吗」
// 「这趟旅行领了多少积分」时，只能去 docker logs 里翻，且重启即丢。
//
// 设计取态：
//   - 内存环形缓冲（默认保留最近 N 条）：面板只关心近期，无需数据库。
//   - 异步落盘到 JSON：重启后仍能看到最近的执行历史。
//   - 只记录结果摘要，不记录凭证/响应体全文。
package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Kind 任务类型。
type Kind string

const (
	// KindCheckin 每日签到（含余额刷新）。
	KindCheckin Kind = "checkin"
	// KindTravel 猫猫旅行（领养 / 派出 / 领奖）。
	KindTravel Kind = "travel"
	// KindKeepalive token 保活。
	KindKeepalive Kind = "keepalive"
)

// Status 执行结果。
type Status string

const (
	// StatusOK 成功。
	StatusOK Status = "ok"
	// StatusSkipped 跳过（非错误，如需重新登录、已达每日上限、条件未满足）。
	StatusSkipped Status = "skipped"
	// StatusFailed 失败。
	StatusFailed Status = "failed"
)

// Entry 一条执行记录。
type Entry struct {
	// At 执行时刻。
	At time.Time `json:"at"`
	// Kind 任务类型。
	Kind Kind `json:"kind"`
	// UID 账号 UID（完整，面板自行截断展示）。
	UID string `json:"uid"`
	// Nickname 账号昵称（冗余存储：账号删除后历史记录仍可读）。
	Nickname string `json:"nickname,omitempty"`
	// Status 结果。
	Status Status `json:"status"`
	// Action 具体动作（如 "depart" / "claim" / "adopt"），非必需。
	Action string `json:"action,omitempty"`
	// Message 面向用户的一句话说明。
	Message string `json:"message,omitempty"`
	// Reward 本次获得的积分（旅行领奖 / 领养 +300），0 表示无。
	Reward int64 `json:"reward,omitempty"`
	// Credits 任务后的账号余额（签到后的 remain），-1 表示未知。
	Credits int64 `json:"credits"`
}

// Collector 执行记录收集器（线程安全）。
type Collector struct {
	mu    sync.Mutex
	items []Entry
	max   int
	file  string
	dirty bool
	// nowFunc 便于测试注入时钟。
	nowFunc func() time.Time
}

// defaultMax 默认保留条数。
//
// 取值权衡：11 个账号 × 3 类任务 × 每天 2 次签到 = 约 60 条/天。
// 保留 2000 条 ≈ 一个多月，JSON 约几百 KB，足够回溯。
const defaultMax = 2000

// New 构建收集器；file 为空表示纯内存（不持久化）。
func New(file string) *Collector {
	c := &Collector{max: defaultMax, file: file}
	if file != "" {
		c.load()
	}
	return c
}

// SetMax 设置保留条数上限（<=0 用默认）。
func (c *Collector) SetMax(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 {
		n = defaultMax
	}
	c.max = n
	c.trimLocked()
}

// now 返回当前时间（测试可注入）。
func (c *Collector) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

// Add 追加一条记录。
func (c *Collector) Add(e Entry) {
	if e.At.IsZero() {
		e.At = c.now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// credits 缺省用 -1 表示未知（0 是合法余额，不能当缺省）。
	if e.Credits == 0 && e.Message == "" {
		e.Credits = -1
	}
	c.items = append(c.items, e)
	c.trimLocked()
	c.dirty = true
	c.saveLocked()
}

// trimLocked 裁剪到上限（保留最新的）。调用方须持锁。
func (c *Collector) trimLocked() {
	if len(c.items) <= c.max {
		return
	}
	drop := len(c.items) - c.max
	c.items = append([]Entry(nil), c.items[drop:]...)
}

// Query 查询记录。
type Query struct {
	// Kind 为空表示全部。
	Kind Kind
	// UID 非空时只看该账号。
	UID string
	// Since/Until 时间范围（零值不限制）。
	Since time.Time
	Until time.Time
	// Limit 最多返回条数（<=0 用 200）；返回的是**最新**的 N 条。
	Limit int
}

// Query 返回符合条件的记录，按时间**倒序**（最新在前）。
func (c *Collector) Query(q Query) []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]Entry, 0, len(c.items))
	for _, e := range c.items {
		if q.Kind != "" && e.Kind != q.Kind {
			continue
		}
		if q.UID != "" && e.UID != q.UID {
			continue
		}
		if !q.Since.IsZero() && e.At.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !e.At.Before(q.Until) {
			continue
		}
		out = append(out, e)
	}
	// 倒序：最新在前。
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })

	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Summary 汇总统计（面板顶部卡片用）。
type Summary struct {
	// Total 记录总数。
	Total int `json:"total"`
	// ByKind 各任务类型的条数。
	ByKind map[string]int `json:"by_kind"`
	// ByStatus 各结果的条数。
	ByStatus map[string]int `json:"by_status"`
	// LatestAt 最近一条记录的时刻。
	LatestAt *time.Time `json:"latest_at,omitempty"`
	// TodayReward 今日获得的积分合计。
	TodayReward int64 `json:"today_reward"`
	// TodayOK 今日成功次数。
	TodayOK int `json:"today_ok"`
	// TodayFailed 今日失败次数。
	TodayFailed int `json:"today_failed"`
}

// Summary 生成汇总（today 以本地时区的自然日为准）。
func (c *Collector) Summary() Summary {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := Summary{ByKind: map[string]int{}, ByStatus: map[string]int{}}
	now := c.now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	for i, e := range c.items {
		s.Total++
		s.ByKind[string(e.Kind)]++
		s.ByStatus[string(e.Status)]++
		if i == len(c.items)-1 {
			at := e.At
			s.LatestAt = &at
		}
		if !e.At.Before(todayStart) {
			switch e.Status {
			case StatusOK:
				s.TodayOK++
				s.TodayReward += e.Reward
			case StatusFailed:
				s.TodayFailed++
			}
		}
	}
	return s
}

// Len 返回当前记录条数。
func (c *Collector) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// saveLocked 原子落盘。调用方须持锁。
//
// 每条都落盘而不是攒批：这些任务一天只跑几次（签到 2 次 + 保活 1 次 + 旅行），
// 写入频率极低，攒批带来的复杂度不值得。落盘失败只忽略（记录是观测功能，
// 不该因磁盘问题影响任务本身）。
func (c *Collector) saveLocked() {
	if c.file == "" {
		return
	}
	c.dirty = false
	raw, err := json.Marshal(c.items)
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.file); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.file)
}

// load 启动时恢复记录（文件缺失/损坏时从空开始）。
func (c *Collector) load() {
	raw, err := os.ReadFile(c.file)
	if err != nil {
		return
	}
	var items []Entry
	if json.Unmarshal(raw, &items) != nil {
		return
	}
	c.mu.Lock()
	c.items = items
	c.trimLocked()
	c.mu.Unlock()
}

// Flush 强制落盘（进程退出前调用）。
func (c *Collector) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirty {
		c.saveLocked()
	}
}
