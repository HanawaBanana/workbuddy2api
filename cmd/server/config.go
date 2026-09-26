// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Server struct{} `json:"server"` // 已退役段：max_body_mb 移除后无字段；旧配置该段下任意键因 JSON 未知字段而自然忽略

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	// Admin 运维管理端点开关（issue #138/#118）。默认**关闭**：管理能力默认不暴露，
	// 避免「开了网关就等于开了账号管理面」。开启后
	// /admin/accounts/{uid}/{disable,enable,revive} 可用；鉴权与 /status 同源
	// （withAuth + 同一个 api_key，不另立管理密钥）。
	Admin struct {
		Enabled bool `json:"enabled"` // 默认 false
		// AuditEnabled 管理操作审计日志开关。默认**关闭**：审计会在磁盘上额外
		// 落一份文件，老部署即便开了 admin 也不该被动产生新文件，故与 enabled
		// 拆成两个开关。开启后 /admin 下每个动作（账号 disable/enable/revive、
		// 手动触发任务）追加一行 JSONL，含时间/动作/对象/结果/来源 IP 与
		// api_key 指纹（指纹而非原文）。
		// 要求 admin.enabled 同时开启——审计的对象就是这些端点，端点不存在时
		// 开着审计是个只会误导人的空配置，normalize 直接拒绝。
		AuditEnabled bool `json:"audit_enabled"` // 默认 false
		// AuditFile 审计日志路径。空 = 内置默认 ./data/admin_audit.log。
		// 与 state_file 同约定：父目录由进程自建（NewAuditLog 会 MkdirAll）。
		AuditFile string `json:"audit_file"`
	} `json:"admin"`

	// Metrics Prometheus 指标端点开关。默认**关闭**：开启后 GET /metrics 暴露
	// 进程内只读快照（账号池健康度 + 按模型的请求/延迟/token/积分），供 Prometheus
	// 抓取与 Grafana 看板。与 /status、/v1/stats 同走 withAuth（同一个 api_key，
	// 不另立指标密钥）；抓取端用 authorization/bearer_token 配置即可。
	// 数据源全是进程内状态，scrape 不产生任何上游请求。
	Metrics struct {
		Enabled bool `json:"enabled"` // 默认 false
	} `json:"metrics"`

	// Alerting 可用性阈值告警（webhook 推送）。默认**关闭**：关闭时不起 goroutine、
	// 不发任何 HTTP，行为与改动前逐字一致。目标 URL 由运维自备（自建接收端或
	// 企业微信/钉钉/Slack 的 incoming webhook）；网关从不因此调用上游。
	//
	// 阈值语义：min_healthy_* 是"最低可接受健康账号数"，**低于**该值才告警
	// （min=1 即"健康数为 0 时告警"）；**0 = 关闭该规则**——所以它是哨兵值而非
	// "未设置"，normalize 不会把 0 回落成默认（纯 CN 部署没有 global 账号，
	// min_healthy_global 默认 0 关闭，否则会常驻误报）。
	Alerting struct {
		Enabled             bool   `json:"enabled"`               // 默认 false
		WebhookURL          string `json:"webhook_url"`           // enabled 时必填，scheme 限 http/https
		Secret              string `json:"secret"`                // 非空则对 body 做 HMAC-SHA256 签名
		IntervalSeconds     int    `json:"interval_seconds"`      // 评估周期，默认 30
		TimeoutSeconds      int    `json:"timeout_seconds"`       // webhook 超时，默认 5
		StartupGraceSeconds int    `json:"startup_grace_seconds"` // 启动宽限（避开 auths 未 sync 的 0 健康态），默认 30
		MinHealthyCN        int    `json:"min_healthy_cn"`        // 默认 1；0 = 关闭该规则
		MinHealthyGlobal    int    `json:"min_healthy_global"`    // 默认 0 = 关闭（纯 CN 部署不误报）
		RecoverHealthy      int    `json:"recover_healthy"`       // 恢复阈值（迟滞上沿），默认 1
		BreakerThreshold    int    `json:"breaker_threshold"`     // 熔断账号数阈值，默认 0 = 关闭
		ForTicks            int    `json:"for_ticks"`             // 连续满足多少拍才触发，默认 2
		ClearTicks          int    `json:"clear_ticks"`           // 连续不满足多少拍才解除，默认 2
		SendResolve         bool   `json:"send_resolve"`          // 恢复时是否也发通知，默认 false
	} `json:"alerting"`

	// Budget 当日积分预算闸（admission control）。默认**关闭**：
	// daily_credit_limit 为 0 表示不限，行为与改动前逐字一致。
	// 当日累计扣费达到上限后，网关直接拒掉后续对话请求（429 + 明确错误码），
	// 把积分损失截断在阈值附近——防的是跑飞的客户端、忘了关的脚本把积分烧光。
	Budget struct {
		// DailyCreditLimit 当日累计 credit 上限。**0 = 关闭该闸**（不限）——这是
		// 哨兵值而非"未设置"，normalize 不会把它回落成默认值；负值报错。
		// 计数按 CST 自然日重置（上游增长体系按 CST 刷新），进程重启即清零。
		// 只统计上游 usage 给出 credit 的请求，故当日用量是**下界**、真实日耗只会
		// 更多，阈值宜按保守值设（可先把闸当观察模式跑几天，看 /status 的
		// daily_budget.used 再定）。
		DailyCreditLimit float64 `json:"daily_credit_limit"`
	} `json:"budget"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（D5，internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。默认值已对齐官方（A 段变更），用户仍可配完全
		// 自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与白名单
		// 头组 X-IDE-Version 的取值）。空 = 内置默认（对齐官方 5.5.4 分发包）；
		// 显式配置（如升级后的桌面包版本）则随配置走。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（对齐官方内置 CLI
		// 2.137.1）；显式配置则随配置走。
		CliVersion string `json:"cli_version"`

		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底。
		// 容器内无桌面端 Turing SDK，这是把外部生成的 token 注入的入口；空 = 不注入。
		// 每号覆盖优先级：auths 文件 device_token > 本全局值 > DeviceTokenFile（文件兜底）。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
		// 读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略（优雅降级不注入）。
		DeviceTokenFile string `json:"device_token_file"`
		// ClientName 用量归属头 X-Product/X-IDE-Name/X-IDE-Type 的取值。
		// 空（缺省）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
		// 上游用量归因不再出现 client/agentPurpose 为空的网关特征）。
		// 显式配 "SaaS" 还原旧行为（仅 X-Product="SaaS"，不设 X-IDE-*）。
		ClientName string `json:"client_name"`
		// PassthroughIP 是否透传客户端 IP（X-Forwarded-For/X-Real-IP 首段）给上游。
		// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）；true 才透传。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer（显式配置仍可覆盖回替换）；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "custom" / "append" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int     `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 403 风控分档），0 = 回落 max_in_flight
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）：
		// ErrClient/传输层这类「不罚号」失败连续计数，达阈临时出池。与冷却/熔断
		// 并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int    `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到查余额时，到期时间在此窗口内
		// 的积分被标记为"快过期"，选号优先消耗（issue:积分过期）。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	DegradeCooldownDur  time.Duration `json:"-"`
	DegradeCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	ExpiringSoonDur     time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	// 排程段默认值由 internal/config 集中维护（cmd/server 与 cmd/activity 共用，
	// 消除 issue #49 的默认值漂移）。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	// 出站指纹默认伪造官方 WorkBuddy 桌面端：UA 三段式 + X-IDE-* 头组
	// （upstream.Client 的 attributionClientName 空值也回落 WorkBuddy，双保险）；
	// 显式 client_name="SaaS" 还原旧行为。
	c.Upstream.ClientName = "WorkBuddy"
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system；显式配置 custom 仍可覆盖回替换
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403
	// 修复 P1-1）；0/负数 normalize 回落。显式 0 需配 -1 之外的方式关闭分档
	// ——这里约定 0 = 未设置回落默认 2（与 max_in_flight 的 0=不限语义不同，
	// 分档键的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	// Metrics.Enabled / Admin.Enabled 缺省 false（零值）：管理面与指标面都不默认暴露，
	// 老 config 不含这两个键时行为逐字不变。这里显式写出以明示默认值意图。
	c.Metrics.Enabled = false
	// 审计同样缺省关闭；路径给默认值（与 state_file 同目录约定）——开关关着时
	// 该路径是惰性的，写出来只是让 config.example.json 的取值与实现一致。
	c.Admin.AuditEnabled = false
	c.Admin.AuditFile = "./data/admin_audit.log"
	// 预算闸缺省关闭：daily_credit_limit 的 0 是"不限"哨兵，**故意不回落默认值**
	// （与 alerting.min_healthy_global 同风格）。这里显式写出以明示这一意图。
	c.Budget.DailyCreditLimit = 0
	// Alerting 缺省关闭；各阈值/周期给默认值，但 min_healthy_global 与
	// breaker_threshold 的 0 是"关闭该规则"的哨兵，不能改（见 Config.Alerting 注释）。
	c.Alerting.IntervalSeconds = 30
	c.Alerting.TimeoutSeconds = 5
	c.Alerting.StartupGraceSeconds = 30
	c.Alerting.MinHealthyCN = 1
	c.Alerting.MinHealthyGlobal = 0
	c.Alerting.RecoverHealthy = 1
	c.Alerting.BreakerThreshold = 0
	c.Alerting.ForTicks = 2
	c.Alerting.ClearTicks = 2
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_ADMIN_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_ADMIN_AUDIT_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.AuditEnabled = b
		}
	}
	if v := os.Getenv("WB2A_ADMIN_AUDIT_FILE"); v != "" {
		c.Admin.AuditFile = v
	}
	if v := os.Getenv("WB2A_BUDGET_DAILY_CREDIT_LIMIT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Budget.DailyCreditLimit = f
		}
	}
	// 排程段的台账与当日失败重试（schedule.*）。三个键都进 env：容器部署改这一组
	// 往往只为「补跑一次」或「把台账落到卷上」，不该逼着用户去改挂载的 config.json。
	if v := os.Getenv("WB2A_SCHEDULE_LEDGER_FILE"); v != "" {
		c.Schedule.LedgerFile = v
	}
	if v := os.Getenv("WB2A_SCHEDULE_RETRY_DELAY_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.RetryDelayMinutes = n
		}
	}
	if v := os.Getenv("WB2A_SCHEDULE_RETRY_MAX_PER_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.RetryMaxPerDay = n
		}
	}
	if v := os.Getenv("WB2A_METRICS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Metrics.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_ALERTING_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Alerting.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_ALERTING_WEBHOOK_URL"); v != "" {
		c.Alerting.WebhookURL = v
	}
	if v := os.Getenv("WB2A_ALERTING_SECRET"); v != "" {
		c.Alerting.Secret = v
	}
	for _, e := range []struct {
		key string
		dst *int
	}{
		{"WB2A_ALERTING_INTERVAL_SECONDS", &c.Alerting.IntervalSeconds},
		{"WB2A_ALERTING_TIMEOUT_SECONDS", &c.Alerting.TimeoutSeconds},
		{"WB2A_ALERTING_STARTUP_GRACE_SECONDS", &c.Alerting.StartupGraceSeconds},
		{"WB2A_ALERTING_MIN_HEALTHY_CN", &c.Alerting.MinHealthyCN},
		{"WB2A_ALERTING_MIN_HEALTHY_GLOBAL", &c.Alerting.MinHealthyGlobal},
		{"WB2A_ALERTING_RECOVER_HEALTHY", &c.Alerting.RecoverHealthy},
		{"WB2A_ALERTING_BREAKER_THRESHOLD", &c.Alerting.BreakerThreshold},
		{"WB2A_ALERTING_FOR_TICKS", &c.Alerting.ForTicks},
		{"WB2A_ALERTING_CLEAR_TICKS", &c.Alerting.ClearTicks},
	} {
		if v := os.Getenv(e.key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*e.dst = n
			}
		}
	}
	if v := os.Getenv("WB2A_ALERTING_SEND_RESOLVE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Alerting.SendResolve = b
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	// 快过期窗口：空值回落默认 168h（Default 已置；此兜底覆盖显式 ""）；显式 "0"/负值 = 禁用分桶。
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = "168h"
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0 // 负值视为禁用，避免 upstream 判定窗口反转
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	// （"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// fail-fast（设计 supplement §4.1）：admin.enabled=true 且 api_key 为空 = 未鉴权的
	// mutation 端点（disable/revive 是可用性操作，风险高于 /status 读泄漏），拒绝启动。
	// 校验放 applyEnv 之后：env 覆盖（WB2A_ADMIN_ENABLED / WB2A_API_KEY）与 config
	// 两条入口最终状态一致拦截。
	if c.Admin.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("admin.enabled=true 但 api_key 为空：请设置 api_key 或将 admin.enabled 置 false")
	}
	// 审计段的 fail-fast，与上面的 admin 校验并列（同属"开了功能就必须给齐参数"）：
	//   - audit_enabled=true 而 admin.enabled=false：审计的对象就是 /admin 端点，
	//     端点不存在时开着审计是个永远不会写一行的空配置——它比报错更危险，因为
	//     运维会以为"审计已经在跑了"。
	//   - 路径为空：无从落盘（Default 已给默认值，走到这里说明被显式置空了）。
	if c.Admin.AuditEnabled {
		if !c.Admin.Enabled {
			return fmt.Errorf("admin.audit_enabled=true 但 admin.enabled=false：审计的对象是 /admin 端点，请同时开启 admin.enabled")
		}
		if strings.TrimSpace(c.Admin.AuditFile) == "" {
			return fmt.Errorf("admin.audit_enabled=true 但 admin.audit_file 为空：请填写审计日志路径或将 admin.audit_enabled 置 false")
		}
	}
	// 预算闸：只有负值非法。0 是"不限"的合法哨兵，**不回落**——把 0 改写成某个
	// 默认上限，等于在老部署上凭空开始拒请求，正是"缺省 = 旧行为"要禁止的事。
	if c.Budget.DailyCreditLimit < 0 {
		return fmt.Errorf("budget.daily_credit_limit: %v 不得为负；0 = 关闭该闸（不限）", c.Budget.DailyCreditLimit)
	}
	// 告警段归一 + fail-fast（enabled=true 且 webhook_url 缺失/非法 → 拒绝启动）。
	// 位置与上面的 admin 校验并列：两者都是"开了功能就必须给齐参数"的启动期拦截。
	if err := c.normalizeAlerting(); err != nil {
		return err
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizeAlerting 归一并校验 alerting 段。
//
// 三件事：
//  1. 周期/宽限/拍数：0（未设置）回落默认；**负值报错**（负数无合理语义，静默回落
//     会掩盖配置笔误）。
//  2. 阈值：min_healthy_cn / min_healthy_global / breaker_threshold 的 **0 是合法
//     哨兵 = 关闭该规则**，不回落、不报错；只有负值报错。recover_healthy 的 0
//     回落默认 1（0 会让迟滞失效：healthy>=0 恒为真 → 告警立刻解除）。
//  3. enabled=true 时 webhook_url 必须非空且 scheme ∈ {http,https}、有主机名——
//     fail-fast 而非运行时空转（风格同 admin.enabled + 空 api_key）。
func (c *Config) normalizeAlerting() error {
	a := &c.Alerting

	for _, f := range []struct {
		name string
		val  int
	}{
		{"alerting.min_healthy_cn", a.MinHealthyCN},
		{"alerting.min_healthy_global", a.MinHealthyGlobal},
		{"alerting.breaker_threshold", a.BreakerThreshold},
		{"alerting.recover_healthy", a.RecoverHealthy},
		{"alerting.interval_seconds", a.IntervalSeconds},
		{"alerting.timeout_seconds", a.TimeoutSeconds},
		{"alerting.startup_grace_seconds", a.StartupGraceSeconds},
		{"alerting.for_ticks", a.ForTicks},
		{"alerting.clear_ticks", a.ClearTicks},
	} {
		if f.val < 0 {
			return fmt.Errorf("%s: 不得为负", f.name)
		}
	}

	if a.IntervalSeconds == 0 {
		a.IntervalSeconds = 30
	}
	if a.TimeoutSeconds == 0 {
		a.TimeoutSeconds = 5
	}
	if a.StartupGraceSeconds == 0 {
		a.StartupGraceSeconds = 30
	}
	if a.ForTicks == 0 {
		a.ForTicks = 2
	}
	if a.ClearTicks == 0 {
		a.ClearTicks = 2
	}
	if a.RecoverHealthy == 0 {
		a.RecoverHealthy = 1
	}

	if a.Enabled {
		raw := strings.TrimSpace(a.WebhookURL)
		if raw == "" {
			return fmt.Errorf("alerting.enabled=true 但 alerting.webhook_url 为空：请填写告警投递地址或将 alerting.enabled 置 false")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("alerting.webhook_url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("alerting.webhook_url: scheme 必须是 http 或 https，got %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("alerting.webhook_url: 缺少主机名")
		}
	}
	return nil
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 custom/append/passthrough）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / append / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}
