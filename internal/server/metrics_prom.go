// metrics_prom.go Prometheus 文本格式指标导出（GET /metrics）。
//
// 设计要点：
//   - **零第三方依赖**：手写 Prometheus 0.0.4 text exposition，只用标准库
//     （本仓库纪律：仅 go-redis/v9，见 logfmt 与 cmd/stats 的同类声明）。
//   - **零新增累加器**：所有指标在 scrape 时刻**现算**——请求维度复用
//     MetricsSnapshotOf()（metrics.go 的单一埋点产物），账号池维度走
//     Pool.RealmHealth（只读现算），任务维度读 taskledger 的既有台账
//     （scheduler 写入的同一份，不另存一份累加器），不维护第二份状态，
//     因此不存在双写一致性风险。
//   - **零新增上游请求**：全部读进程内状态，scrape 不触发任何网络调用。
//   - **确定性输出**：模型按字典序、realm/state 按固定序，同一状态下两次抓取
//     逐字节相等（项目对稳定输出的既有要求，见 List/rateLimitedModelsLocked）。
//   - **不暴露高基数/敏感维度**：不加 per-uid 标签（泄漏 uid 且基数不可控）；
//     逐账号台账由 /status 提供，此处不重复。输出不含凭证、token、会话键。
package server

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskledger"
)

// promRealms 导出顺序固定为 cn → global → all（label, realm 谓词）。
// 顺序写死而非遍历 map：map 迭代序随机会让两次抓取输出不同，破坏"稳定输出"。
var promRealms = []struct {
	label string
	realm string
}{
	{"cn", "cn"},
	{"global", "global"},
	{"all", ""},
}

// promPoolStates wb2api_pool_accounts 的 state 维度固定顺序。
var promPoolStates = []string{
	"healthy", "cooling", "disabled", "in_flight_full",
	"breaker", "degraded", "manual_disabled", "model_cooled",
}

// promRealmHealth 一个 realm 的标签与健康度分解（writePromMetrics 的输入）。
type promRealmHealth struct {
	label string
	h     pool.RealmHealth
}

// promWriter 极简 text exposition 写入器。
//
// seen 保证每个指标家族恰好写一次 HELP/TYPE——Prometheus 解析器对重复的
// HELP/TYPE 会报错，而本文件按"家族 → 逐样本"的循环顺序输出（池维度在外层、
// state 维度在内层），没有去重就会重复写。
type promWriter struct {
	sb   strings.Builder
	seen map[string]bool
}

func newPromWriter() *promWriter {
	return &promWriter{seen: map[string]bool{}}
}

// family 写 HELP/TYPE；同家族重复调用为空操作（幂等）。
func (w *promWriter) family(name, help, typ string) {
	if w.seen[name] {
		return
	}
	w.seen[name] = true
	w.sb.WriteString("# HELP ")
	w.sb.WriteString(name)
	w.sb.WriteByte(' ')
	w.sb.WriteString(escapePromHelp(help))
	w.sb.WriteByte('\n')
	w.sb.WriteString("# TYPE ")
	w.sb.WriteString(name)
	w.sb.WriteByte(' ')
	w.sb.WriteString(typ)
	w.sb.WriteByte('\n')
}

// sample 写一条样本。labels 是扁平的 name,value 序列（如 "realm","cn"），
// 为空则不输出花括号。
func (w *promWriter) sample(name string, value float64, labels ...string) {
	w.sb.WriteString(name)
	if len(labels) > 0 {
		w.sb.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.sb.WriteByte(',')
			}
			w.sb.WriteString(labels[i])
			w.sb.WriteString(`="`)
			w.sb.WriteString(escapePromLabel(labels[i+1]))
			w.sb.WriteByte('"')
		}
		w.sb.WriteByte('}')
	}
	w.sb.WriteByte(' ')
	w.sb.WriteString(formatPromValue(value))
	w.sb.WriteByte('\n')
}

// escapePromHelp HELP 文本转义：反斜杠与换行（Prometheus 规范只要求这两个）。
func escapePromHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// escapePromLabel label 值转义：反斜杠、双引号、换行。
// 模型名可能来自客户端（未知模型名也会进 metrics 键），含引号/反斜杠时
// 不转义会写出非法 exposition（解析失败 → 整个 scrape 丢弃）。
func escapePromLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// formatPromValue 数值格式化。整数形态不带小数点（'g' 对 3.0 输出 "3"），
// 非有限值按 Prometheus 约定输出 NaN/+Inf/-Inf——虽然 deriveModelStat 已用
// 零分母守卫排除了 NaN，这里仍防御，避免任何未来路径写坏整个 scrape。
func formatPromValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// promPoolStateValue 把 RealmHealth 的字段按 state 标签取值。
func promPoolStateValue(h pool.RealmHealth, state string) int {
	switch state {
	case "healthy":
		return h.Healthy
	case "cooling":
		return h.Cooling
	case "disabled":
		return h.Disabled
	case "in_flight_full":
		return h.InFlightFull
	case "breaker":
		return h.Breaker
	case "degraded":
		return h.Degraded
	case "manual_disabled":
		return h.ManualDisabled
	case "model_cooled":
		return h.ModelCooled
	}
	return 0
}

// writePromMetrics 生成完整 exposition 文本。纯函数（无 IO、无时间读取），
// 便于测试用固定输入做逐行断言。
func writePromMetrics(snap MetricsSnapshot, health []promRealmHealth, stickySessions int, costExploreEvents int64, wafActive bool, tasks map[string]taskledger.Run) string {
	w := newPromWriter()

	// ---------- 账号池（realm 维度） ----------
	w.family("wb2api_pool_accounts_total", "账号池内该 realm 的账号总数（realm=all 为全池）。", "gauge")
	for _, rh := range health {
		w.sample("wb2api_pool_accounts_total", float64(rh.h.Total), "realm", rh.label)
	}

	w.family("wb2api_pool_accounts", "账号池按 realm 与状态分解的账号数。状态维度是原因分解而非互斥分类：熔断/降权号同时计入 cooling；manual_disabled 是 disabled 的子集；model_cooled 与账号级健康正交（该号对触发模型不可用、对其他模型仍可选）。", "gauge")
	for _, rh := range health {
		for _, st := range promPoolStates {
			w.sample("wb2api_pool_accounts", float64(promPoolStateValue(rh.h, st)), "realm", rh.label, "state", st)
		}
	}

	w.family("wb2api_pool_in_flight", "该 realm 各账号在途请求数之和。", "gauge")
	for _, rh := range health {
		w.sample("wb2api_pool_in_flight", float64(rh.h.InFlight), "realm", rh.label)
	}

	w.family("wb2api_sticky_sessions", "当前会话粘性绑定数（粘性未启用时恒 0）。", "gauge")
	w.sample("wb2api_sticky_sessions", float64(stickySessions))

	w.family("wb2api_cost_explore_events_total", "costTier 条件探索累计触发次数（issue #136）。", "counter")
	w.sample("wb2api_cost_explore_events_total", float64(costExploreEvents))

	w.family("wb2api_waf_ip_block_active", "IP 级 WAF 拦截是否处于激活期（1=激活）。进程内状态，重启清零。", "gauge")
	w.sample("wb2api_waf_ip_block_active", boolToFloat(wafActive))

	// ---------- 定时任务台账（taskledger） ----------
	//
	// 只为**跑过至少一轮**的任务输出序列，不补 0 时刻的占位序列：从未跑过的任务
	// 没有序列，而不是「时刻为 0」——否则服务刚启动 / 台账刚清空时，
	// `time() - wb2api_task_last_run_timestamp_seconds > 86400` 这类告警会在
	// 每次部署时误报一轮。反过来，某类任务跑过之后停跑，它的 ts 会一直变旧，
	// 告警照常触发——这正是要抓的情形。
	//
	// 任务名维度固定顺序（promTaskKinds），保证同一状态下两次抓取逐字节相等。
	ranKinds := make([]string, 0, len(promTaskKinds))
	for _, kind := range promTaskKinds {
		if _, ok := tasks[kind]; ok {
			ranKinds = append(ranKinds, kind)
		}
	}
	if len(ranKinds) > 0 {
		w.family("wb2api_task_last_run_timestamp_seconds", "各类定时任务最近一轮的结束时刻（Unix 秒）。序列缺失表示该类任务尚未跑过，不是「时刻为 0」——告警规则用 absent() 或先确认序列存在。", "gauge")
		for _, kind := range ranKinds {
			w.sample("wb2api_task_last_run_timestamp_seconds", float64(tasks[kind].Finished.Unix()), "kind", kind)
		}

		w.family("wb2api_task_last_run_accounts", "各类定时任务最近一轮的计数分解：total=本轮涉及量，ok=做成，already=幂等成功（如今天已签到），fail=上游报错，skipped=有意跳过（禁用号/无凭证/global/门槛未达/在途）。", "gauge")
		for _, kind := range ranKinds {
			for _, res := range promTaskResults {
				w.sample("wb2api_task_last_run_accounts", float64(promTaskResultValue(tasks[kind], res)), "kind", kind, "result", res)
			}
		}

		w.family("wb2api_task_last_run_all_failed", "各类定时任务最近一轮是否「全灭」（有失败且没有任何账号做成，1=是）。判据刻意不是失败率：还有账号成功就说明上游是通的，失败是账号级的。为 1 且未启用重试时应告警。", "gauge")
		for _, kind := range ranKinds {
			w.sample("wb2api_task_last_run_all_failed", boolToFloat(tasks[kind].AllFailed), "kind", kind)
		}
	}

	since := float64(0)
	if !snap.Since.IsZero() {
		since = float64(snap.Since.Unix())
	}
	w.family("wb2api_stats_since_timestamp_seconds", "请求统计聚合的起点（进程启动或上次 /v1/stats/reset）的 Unix 秒。计数器随该点重置，看板据此识别 resets。", "gauge")
	w.sample("wb2api_stats_since_timestamp_seconds", since)

	// ---------- 请求 / 模型 ----------
	w.family("wb2api_requests_total", "网关累计处理的 chat 请求数。", "counter")
	w.sample("wb2api_requests_total", float64(snap.Total.Success), "status", "success")
	w.sample("wb2api_requests_total", float64(snap.Total.Failed), "status", "failed")

	// snap.Models 按请求数降序（面板默认序）；此处重排为字典序，保证输出稳定。
	models := make([]ModelStatPayload, len(snap.Models))
	copy(models, snap.Models)
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })

	w.family("wb2api_model_requests_total", "按模型与结果累计的 chat 请求数。", "counter")
	for _, m := range models {
		w.sample("wb2api_model_requests_total", float64(m.Success), "model", m.Model, "status", "success")
		w.sample("wb2api_model_requests_total", float64(m.Failed), "model", m.Model, "status", "failed")
	}

	w.family("wb2api_model_requests_streaming_total", "按模型累计的流式请求数。", "counter")
	for _, m := range models {
		w.sample("wb2api_model_requests_streaming_total", float64(m.Streaming), "model", m.Model)
	}

	w.family("wb2api_model_tokens_total", "按模型与类型累计的 token 数（仅采信上游 usage，缺失不计）。", "counter")
	for _, m := range models {
		w.sample("wb2api_model_tokens_total", float64(m.PromptTokens), "model", m.Model, "type", "prompt")
		w.sample("wb2api_model_tokens_total", float64(m.CompletionTokens), "model", m.Model, "type", "completion")
	}

	w.family("wb2api_model_cache_tokens_total", "按模型与缓存类型累计的 token 数。", "counter")
	for _, m := range models {
		w.sample("wb2api_model_cache_tokens_total", float64(m.CacheHitTokens), "model", m.Model, "type", "hit")
		w.sample("wb2api_model_cache_tokens_total", float64(m.CacheMissTokens), "model", m.Model, "type", "miss")
		w.sample("wb2api_model_cache_tokens_total", float64(m.CacheWriteTokens), "model", m.Model, "type", "write")
	}

	w.family("wb2api_model_credit_total", "按模型累计的上游积分消耗（usage.credit）。", "counter")
	for _, m := range models {
		w.sample("wb2api_model_credit_total", m.Credit, "model", m.Model)
	}

	w.family("wb2api_model_avg_ttfb_seconds", "按模型的首字节平均耗时（仅成功且有观测的请求计入分母）。", "gauge")
	for _, m := range models {
		w.sample("wb2api_model_avg_ttfb_seconds", m.AvgTTFBMS/1000, "model", m.Model)
	}

	w.family("wb2api_model_avg_latency_seconds", "按模型的端到端平均耗时。", "gauge")
	for _, m := range models {
		w.sample("wb2api_model_avg_latency_seconds", m.AvgLatencyMS/1000, "model", m.Model)
	}

	w.family("wb2api_model_tokens_per_second", "按模型的生成吞吐（completion token / 纯生成秒数）。", "gauge")
	for _, m := range models {
		w.sample("wb2api_model_tokens_per_second", m.TokensPerSec, "model", m.Model)
	}

	return w.sb.String()
}

// promMetrics 处理 GET /metrics：把进程内只读快照渲染为 Prometheus 文本。
//
// 与 /status、/v1/stats 同走 withAuth（空 api_key = 不鉴权）。Prometheus 抓取端
// 支持 authorization/bearer_token 配置，因此带鉴权不影响标准抓取链路。
func (h *Handler) promMetrics(w http.ResponseWriter, r *http.Request) {
	snap := MetricsSnapshotOf()

	health := make([]promRealmHealth, 0, len(promRealms))
	for _, rl := range promRealms {
		if h.cfg.Pool == nil {
			break
		}
		health = append(health, promRealmHealth{label: rl.label, h: h.cfg.Pool.RealmHealth(rl.realm)})
	}

	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}

	var exploreEvents int64
	if h.cfg.Pool != nil {
		exploreEvents, _ = h.cfg.Pool.CostExploreStatus()
	}

	// 任务台账：未接线（nil）时传 nil map，任务指标整段不输出。
	var tasks map[string]taskledger.Run
	if h.cfg.TaskLedger != nil {
		tasks = h.cfg.TaskLedger.Runs()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(writePromMetrics(snap, health, sticky, exploreEvents, h.wafIP.active(), tasks)))
}
