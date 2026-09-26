// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import "fmt"

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 六类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	CheckinHours   []int `json:"checkin_hours"`   // [9,21]
	TravelHours    []int `json:"travel_hours"`    // [9,21]
	ActivityHours  []int `json:"activity_hours"`  // [10]
	KeepaliveHours []int `json:"keepalive_hours"` // [22]
	SchoolHours    []int `json:"school_hours"`    // [12] 开学季任务（迁移自 school/cat 两条系统 crontab）
	CatHours       []int `json:"cat_hours"`       // [1] 夜猫窗口 23-08 CST，01:00 窗口内补 1 次
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/SchoolEnabled/CatEnabled
	// 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
	TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
	SchoolEnabled    bool `json:"school_enabled"`    // 缺省 true；false = 停开学季任务
	CatEnabled       bool `json:"cat_enabled"`       // 缺省 true；false = 停夜猫子任务
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`
	// JitterMinutes 触发时刻抖动窗口（分钟，默认 0 = 关闭）。
	//
	// 六类任务的触发时刻默认精确落在整点（迁移自系统 crontab 的 `0 9 * * *` 语义），
	// 于是所有部署都在同一秒打上游，形成对齐的突发——对 WAF 不友好。设为正值后，
	// 每类任务的触发时刻在该窗口内取一个**确定性**偏移（同一任务、同一天、同一小时
	// 永远得到同一个偏移，见 scheduler.jitterOffset），把负载摊开。
	//
	// 为什么 0 是"关闭"而不是"回落默认"：它是这里的缺省值本身，也是「保持与引入前
	// 逐字一致」的开关，不是笔误——与 *_enabled 缺省 true 是两回事。
	JitterMinutes int `json:"jitter_minutes"`
	// LedgerFile 任务执行台账落盘路径（空 = 纯内存，默认）。
	//
	// 台账记每类任务「最近一轮」的结果（总/成功/幂等成功/失败/跳过、是否全灭）
	// 与当日重试计数，经 /status 的 task_ledger 段与 /metrics 的任务指标透出。
	// 落盘是为了**重启后还能对账**——服务自动更新/改配置重启后，此前只能看到
	// 一截日志。空值不落盘：老部署不该被动多出一个文件。
	//
	// 落盘失败只记日志、不影响任务执行（观测组件不该拖垮关键路径），因此这里
	// 不做启动期可写性 fail-fast——与 admin.audit_file 刻意相反：那份是安全特性，
	// 空着等于安全承诺失效；这份只是少一段历史。
	LedgerFile string `json:"ledger_file"`
	// RetryDelayMinutes 当日失败重试延迟（分钟）。**0 = 关闭**（缺省，与引入前逐字一致）。
	//
	// 某类任务一轮「全灭」（有失败且没有任何账号做成，见 scheduler.recordRun）
	// 时，在该延迟后再跑一次。判据刻意不是「失败率超阈值」：只要还有账号成功，
	// 就说明上游是通的，失败是账号级的，重试只是对同一批注定失败的号再打一遍
	// 上游（零收益 + 撞风控）；而「一个都没成」几乎只能解释为网络未就绪 / 上游
	// 抖动 / WAF 拦截——那才是重试能救的。这条同时保证了成功路径零新增上游请求。
	RetryDelayMinutes int `json:"retry_delay_minutes"`
	// RetryMaxPerDay 每类任务每日最多重试次数（CST 自然日）。仅
	// RetryDelayMinutes>0 时生效；**0 = 不允许重试**（哨兵值，normalize 不回落）。
	RetryMaxPerDay int `json:"retry_max_per_day"`
	// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
	// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
//
// RetryDelayMinutes 默认 0 = 当日失败重试关闭（老 config 行为逐字不变）；
// RetryMaxPerDay 默认 1——它在开关关着时是惰性的，一旦用户只写了 delay，就自动
// 得到「每天最多补跑一次」这个最保守也最有用的取值（与 ActivityReportCount 的
// 「缺省 5」同一套路：靠 Unmarshal 前先置默认值实现，键缺席才保留）。
// LedgerFile 默认空 = 不落盘（老部署不该被动多出一个文件）。
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []int{9, 21},
		TravelHours:         []int{9, 21},
		ActivityHours:       []int{10},
		KeepaliveHours:       []int{22},
		SchoolHours:          []int{12},
		CatHours:             []int{1},
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		SchoolEnabled:       true,
		CatEnabled:          true,
		ActivityReportCount: 5, // 领猫前置需 5 次对话，5 连发刷满 chat_5
		RetryMaxPerDay:      1, // 只写了 retry_delay_minutes 时的保守默认：每天补跑一次
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，ActivityReportCount 归一，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），在此补齐。
// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
//
// ActivityReportCount：0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
// 注意这是「显式配 0 = 旧行为」的兼容语义，与 scheduler.New 的 <=0 → 1 归一一致；
// 「缺省 = 5」由 DefaultSchedule 在 Unmarshal 前置入，是另一条路径，两者不合并。
//
// JitterMinutes：0 = 关闭（缺省，保持精确整点）；负值/超上限直接报错，不静默回落。
//
// RetryDelayMinutes：0 = 关闭（缺省，与引入前逐字一致）；负值/超上限报错。
// RetryMaxPerDay：0 = 不允许重试（合法哨兵，不回落）；负值报错；缺省 1 由
// DefaultSchedule 前置入。LedgerFile 不校验——台账落盘失败只记日志，不做 fail-fast。
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{9, 21}
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = []int{9, 21}
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = []int{10}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []int{22}
	}
	if len(s.SchoolHours) == 0 {
		s.SchoolHours = []int{12}
	}
	if len(s.CatHours) == 0 {
		s.CatHours = []int{1}
	}
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	// JitterMinutes：0 = 关闭（缺省，精确整点）；负值报错（无合理语义）；
	// 上限 1440（一天）——偏移必须小于 24h，否则触发时刻会漂到名义时点之后一整天，
	// 当天那次实际上被跳过。注意窗口大于相邻槽位间隔（如 9 点与 10 点相隔 60 分钟）时，
	// 任务顺序可能与配置的小时顺序不一致：这是摊开负载的必然代价，不是 bug。
	if s.JitterMinutes < 0 {
		return fmt.Errorf("schedule.jitter_minutes: %d 不得为负；0 = 关闭抖动（精确整点）", s.JitterMinutes)
	}
	if s.JitterMinutes > 1440 {
		return fmt.Errorf("schedule.jitter_minutes: %d 过大（上限 1440 分钟 = 一天）；"+
			"抖动窗口必须小于 24h，否则当天那次任务会被整体推到次日", s.JitterMinutes)
	}
	// RetryDelayMinutes：0 = 关闭（缺省，行为与引入前一致）；负值报错（无合理语义）。
	// 上限 1440：延迟 >= 24h 必然跨到次日，那时 taskledger 会判 next_day 而不排期
	// （「当日失败重试」的语义止于当日），配置即等于没写——不如直接报错说清。
	if s.RetryDelayMinutes < 0 {
		return fmt.Errorf("schedule.retry_delay_minutes: %d 不得为负；0 = 关闭当日失败重试", s.RetryDelayMinutes)
	}
	if s.RetryDelayMinutes > 1440 {
		return fmt.Errorf("schedule.retry_delay_minutes: %d 过大（上限 1440 分钟 = 一天）；"+
			"延迟 >= 24h 时重试必然跨到次日，会被判为 next_day 而不排期——当日失败重试只在本自然日内补跑", s.RetryDelayMinutes)
	}
	// RetryMaxPerDay：**0 = 不允许重试**是合法哨兵（与 alerting.min_healthy_* 同风格），
	// 不回落、不报错；只有负值报错。缺省 1 由 DefaultSchedule 在 Unmarshal 前置入。
	if s.RetryMaxPerDay < 0 {
		return fmt.Errorf("schedule.retry_max_per_day: %d 不得为负；0 = 不允许重试（当日失败重试仍由 retry_delay_minutes 总开关控制）", s.RetryMaxPerDay)
	}
	return s.validateHours()
}

// validateHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.school_hours", "school_enabled", s.SchoolHours); err != nil {
		return err
	}
	return checkHourRange("schedule.cat_hours", "cat_enabled", s.CatHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
