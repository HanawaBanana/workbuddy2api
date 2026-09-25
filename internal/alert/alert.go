// Package alert 账号池可用性阈值告警：周期性评估只读健康快照，越界时 POST 到
// 运维自有的 webhook（不涉及任何上游调用）。
//
// 设计纪律（与网关主流程隔离）：
//   - **默认关闭**：Enabled=false 时不起 goroutine、不发任何 HTTP，行为零变化。
//   - **不碰请求热路径**：只经 Source 的只读访问器取值，评估在独立 ticker 里跑
//     （范式同 pool.startFlusher / session.Router.StartGC）。
//   - **零新增上游请求**：webhook 目标是运维自有端点，Monitor 从不调用 CodeBuddy；
//     事件为边沿触发，外发频率上界 = 告警次数（远小于 tick 数），有界。
//   - **边沿触发 + 迟滞 + 重臂**：只在 pending→firing 跃迁发一次，firing 期间每拍
//     只更新不发送（天然去重，不刷屏）；恢复需连续 clear_ticks 次不满足才回 OK，
//     之后再次越界才会重新触发。抖动（未达 for_ticks 就回落）不告警。
//   - **零第三方依赖**：net/http + crypto/hmac 均为标准库。
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// 默认值（Config 零值/负值回落；与 config 层 normalize 的口径一致）。
const (
	defaultInterval     = 30 * time.Second
	defaultTimeout      = 5 * time.Second
	defaultForTicks     = 2
	defaultClearTicks   = 2
	defaultStartupGrace = 30 * time.Second
	defaultRecover      = 1
	defaultServiceName  = "workbuddy2api"
)

// Health 单个 realm 的健康快照（调用方从 pool.RealmHealth 转换而来）。
type Health struct {
	Total    int
	Healthy  int
	Cooling  int
	Disabled int
	Breaker  int
	Degraded int
	InFlight int
}

// Source 告警评估的数据源。实现方只需提供只读访问器。
type Source interface {
	// Health 返回该 realm 的健康快照；realm ∈ {"cn","global"}。
	Health(realm string) Health
	// WAFActive 报告 IP 级 WAF 拦截是否处于激活期。
	WAFActive() bool
}

// Config 告警配置。零值即"全部关闭"（Enabled=false）。
type Config struct {
	Enabled bool
	// WebhookURL 告警投递目标。Enabled 时必填，且 scheme 必须为 http/https
	// （校验在 config 层 fail-fast，本包只做兜底防御）。
	WebhookURL string
	// Secret 非空时对 body 做 HMAC-SHA256 签名，写入 X-WB2A-Signature 头。
	Secret string

	Interval     time.Duration // 评估周期，默认 30s
	Timeout      time.Duration // webhook 请求超时，默认 5s
	StartupGrace time.Duration // 启动宽限（避开 auths 尚未 sync 的 0 健康态），默认 30s

	// MinHealthyCN / MinHealthyGlobal realm 最低可接受健康账号数：**低于**该值才告警
	// （min=1 即"健康数为 0 时告警"）。**0 = 关闭该规则**——global 默认 0（关闭）：
	// 纯 CN 部署没有 global 账号，若默认开启会常驻误报。
	MinHealthyCN     int
	MinHealthyGlobal int
	// RecoverHealthy 恢复阈值（迟滞上沿），应 > MinHealthy*；默认 1。
	RecoverHealthy int
	// BreakerThreshold 熔断中账号数阈值；0 = 关闭该规则。
	BreakerThreshold int

	ForTicks   int // 连续满足 for_ticks 拍才触发，默认 2
	ClearTicks int // 连续不满足 clear_ticks 拍才解除，默认 2

	// SendResolve 恢复时是否也发一条 event=resolve 通知，默认 false。
	SendResolve bool
	// ServiceName 载荷里的服务标识，空则回落 "workbuddy2api"。
	ServiceName string
}

// normalize 补默认值（不改变显式配置的语义）。
func (c *Config) normalize() {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = defaultStartupGrace
	}
	if c.ForTicks <= 0 {
		c.ForTicks = defaultForTicks
	}
	if c.ClearTicks <= 0 {
		c.ClearTicks = defaultClearTicks
	}
	if c.RecoverHealthy <= 0 {
		c.RecoverHealthy = defaultRecover
	}
	if c.ServiceName == "" {
		c.ServiceName = defaultServiceName
	}
}

// ruleState 单条规则的状态机状态。
type ruleState int

const (
	stateOK ruleState = iota
	statePending
	stateFiring
)

// rule 一条告警规则 + 其状态机。
//
// 事件只在 statePending → stateFiring 的跃迁上产生一次；stateFiring 期间每拍只更新
// clearCount、不产生事件——这就是"不刷屏"的全部机制，无需额外的冷却计时器。
type rule struct {
	id       string
	severity string
	realm    string // 空 = 与 realm 无关（如 WAF / 熔断汇总）

	// sample 采样：返回（当前值, 是否处于告警区, 是否已恢复, 阈值, 可读文案）。
	sample func(src Source) (value float64, bad, recovered bool, threshold float64, msg string)

	state      ruleState
	badTicks   int
	clearCount int
	since      time.Time // 本次告警的起始时刻（载荷里的事件幂等时间戳）
}

// event 一次状态跃迁产生的事件类型。
type event int

const (
	eventNone event = iota
	eventAlert
	eventResolve
)

// step 推进一拍。返回本次产生的事件（无事件时 eventNone）。
func (r *rule) step(now time.Time, src Source, forTicks, clearTicks int) (event, float64, float64, string) {
	v, bad, recovered, th, msg := r.sample(src)
	switch r.state {
	case stateOK:
		if !bad {
			return eventNone, 0, 0, ""
		}
		// 首次越界 → 进入 pending。**必须 fallthrough**：for_ticks=1 时本拍就应
		// 满足阈值并触发；若在此 return，for_ticks=1 会退化成"需要连续两拍"。
		r.state = statePending
		r.badTicks = 0
		fallthrough
	case statePending:
		if !bad {
			// 抖动回落：未达 for_ticks 就不算"持续"，直接回 OK（不告警）。
			r.state = stateOK
			r.badTicks = 0
			return eventNone, 0, 0, ""
		}
		r.badTicks++
		if r.badTicks >= forTicks {
			r.state = stateFiring
			r.since = now
			r.clearCount = 0
			return eventAlert, v, th, msg
		}
	case stateFiring:
		if recovered {
			r.clearCount++
			if r.clearCount >= clearTicks {
				r.state = stateOK
				r.badTicks = 0
				r.clearCount = 0
				return eventResolve, v, th, msg
			}
		} else {
			// 仍在告警区：清零恢复计数并**不发送**（去重的唯一实现点）。
			r.clearCount = 0
		}
	}
	return eventNone, 0, 0, ""
}

// newRules 按配置构建规则集。阈值为 0 的规则不构建（= 关闭）。
func newRules(cfg Config) []*rule {
	var rs []*rule

	for _, rc := range []struct {
		realm string
		min   int
	}{
		{"cn", cfg.MinHealthyCN},
		{"global", cfg.MinHealthyGlobal},
	} {
		if rc.min <= 0 {
			continue // 0 = 关闭（纯 CN 部署默认关 global，避免常驻误报）
		}
		realm, min := rc.realm, rc.min
		rs = append(rs, &rule{
			id:       "realm_healthy_low",
			severity: "critical",
			realm:    realm,
			sample: func(src Source) (float64, bool, bool, float64, string) {
				v := float64(src.Health(realm).Healthy)
				// 严格小于：min=1 表示"健康数低于 1（即 0）才告警"。
				// 不能用 <=——那会让 healthy==1 也告警，与 min_healthy_cn 的
				// "最低可接受健康数"语义不符，且 min=1（推荐默认）会常驻误报。
				bad := v < float64(min)
				recovered := v >= float64(cfg.RecoverHealthy)
				return v, bad, recovered, float64(min),
					fmt.Sprintf("%s realm healthy accounts (%d) below threshold (%d)", realm, int(v), min)
			},
		})
	}

	if cfg.BreakerThreshold > 0 {
		th := cfg.BreakerThreshold
		rs = append(rs, &rule{
			id:       "breaker_high",
			severity: "warning",
			sample: func(src Source) (float64, bool, bool, float64, string) {
				v := float64(src.Health("cn").Breaker + src.Health("global").Breaker)
				return v, v >= float64(th), v < float64(th), float64(th),
					fmt.Sprintf("accounts in circuit-breaker (%d) reached threshold (%d)", int(v), th)
			},
		})
	}

	rs = append(rs, &rule{
		id:       "waf_ip_block",
		severity: "warning",
		sample: func(src Source) (float64, bool, bool, float64, string) {
			active := src.WAFActive()
			v := 0.0
			if active {
				v = 1
			}
			return v, active, !active, 1, "IP-level WAF block is active (chat rotation is failing fast)"
		},
	})

	return rs
}

// Snapshot 载荷里的池快照（不含任何凭证、token、uid、会话键）。
type Snapshot struct {
	CN        RealmSnapshot `json:"cn"`
	Global    RealmSnapshot `json:"global"`
	WAFActive bool          `json:"waf_active"`
}

// RealmSnapshot 单 realm 快照。
type RealmSnapshot struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Disabled int `json:"disabled"`
	Breaker  int `json:"breaker"`
	InFlight int `json:"in_flight"`
}

// Payload webhook 请求体。接收端幂等键 = rule + realm + since。
type Payload struct {
	Version   int       `json:"version"`
	Event     string    `json:"event"`
	Service   string    `json:"service"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Realm     string    `json:"realm,omitempty"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	Message   string    `json:"message"`
	Since     time.Time `json:"since"`
	FiredAt   time.Time `json:"fired_at"`
	Snapshot  Snapshot  `json:"snapshot"`
}

// Monitor 告警监控器。
type Monitor struct {
	cfg       Config
	src       Source
	rules     []*rule
	client    *http.Client
	startedAt time.Time

	mu       sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
}

// New 构造监控器。src 为 nil 时 Tick 为空操作（防御性）。
func New(cfg Config, src Source) *Monitor {
	cfg.normalize()
	return &Monitor{
		cfg:       cfg,
		src:       src,
		rules:     newRules(cfg),
		startedAt: time.Now(),
		client: &http.Client{
			Timeout: cfg.Timeout,
			// 目标 URL 由运维配置（可信），仍限制重定向：≤2 跳且不得跨 host，
			// 避免配置写错时把带签名的载荷转发到意外主机。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 2 {
					return fmt.Errorf("too many redirects")
				}
				if req.URL.Host != via[0].URL.Host {
					return fmt.Errorf("cross-host redirect refused")
				}
				return nil
			},
		},
	}
}

// Run 启动评估循环：立即评估一次，随后每 Interval 评估一次，直到 ctx 取消或 Stop。
// Enabled=false 时直接返回（不起 goroutine、不发任何请求）。
func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.Enabled || m.src == nil {
		return
	}
	m.mu.Lock()
	if m.stop != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.stop = stop
	m.mu.Unlock()

	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()

	m.Tick(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			m.Tick(time.Now())
		}
	}
}

// Stop 停止评估循环（幂等）。Run 未启动时为空操作。
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.stop != nil {
			close(m.stop)
			m.stop = nil
		}
	})
}

// Tick 评估一拍。导出以便测试用受控时钟推进状态机，也便于将来接入手动触发端点。
func (m *Monitor) Tick(now time.Time) {
	if !m.cfg.Enabled || m.src == nil {
		return
	}
	if now.Sub(m.startedAt) < m.cfg.StartupGrace {
		return // 启动宽限：auths 尚未 sync 时健康数为 0，不得据此告警
	}
	for _, r := range m.rules {
		ev, value, threshold, msg := r.step(now, m.src, m.cfg.ForTicks, m.cfg.ClearTicks)
		switch ev {
		case eventAlert:
			m.send("alert", r, now, value, threshold, msg)
		case eventResolve:
			if m.cfg.SendResolve {
				m.send("resolve", r, now, value, threshold, msg)
			}
		}
	}
}

// snapshot 采集当前池快照（用于载荷）。
func (m *Monitor) snapshot() Snapshot {
	toSnap := func(h Health) RealmSnapshot {
		return RealmSnapshot{
			Total:    h.Total,
			Healthy:  h.Healthy,
			Cooling:  h.Cooling,
			Disabled: h.Disabled,
			Breaker:  h.Breaker,
			InFlight: h.InFlight,
		}
	}
	return Snapshot{
		CN:        toSnap(m.src.Health("cn")),
		Global:    toSnap(m.src.Health("global")),
		WAFActive: m.src.WAFActive(),
	}
}

// send 投递一条事件。失败只记 WARN，不影响评估循环（告警通道故障不得拖垮网关）。
func (m *Monitor) send(eventName string, r *rule, now time.Time, value, threshold float64, msg string) {
	p := Payload{
		Version:   1,
		Event:     eventName,
		Service:   m.cfg.ServiceName,
		Rule:      r.id,
		Severity:  r.severity,
		Realm:     r.realm,
		Value:     value,
		Threshold: threshold,
		Message:   msg,
		Since:     r.since,
		FiredAt:   now,
		Snapshot:  m.snapshot(),
	}
	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("WARN: [alert] marshal payload rule=%s: %v", r.id, err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, m.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("WARN: [alert] build request rule=%s: %v", r.id, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "workbuddy2api-alert/1")
	if m.cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(m.cfg.Secret))
		mac.Write(body)
		req.Header.Set("X-WB2A-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := m.client.Do(req)
	if err != nil {
		log.Printf("WARN: [alert] webhook post failed rule=%s: %v", r.id, err)
		return
	}
	// 读干并关闭，避免连接无法复用；限制读取量防异常大响应体。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("WARN: [alert] webhook returned %d rule=%s", resp.StatusCode, r.id)
	}
}
