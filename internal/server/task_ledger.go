// task_ledger.go 任务执行台账在 server 侧的只读渲染：/status 的 task_ledger 段
// 与 /metrics 的任务指标。
//
// 台账本身（数据结构、存储、当日失败重试排期）在 internal/taskledger，写入方是
// internal/scheduler；这里只负责读出来给人看、给 Prometheus 抓。
package server

import (
	"workbuddy2api/internal/taskledger"
)

// TaskLedgerReader 任务执行台账的只读视图（*taskledger.Store 结构上即满足本接口）。
//
// 用接口而非直接依赖 internal/scheduler：server 包不反向 import scheduler
// （同 admin_tasks.go 的 TaskRunner 理由）。台账类型来自 internal/taskledger，
// 两包都只依赖它，不产生环；测试可注入假台账，不必起真调度器。
type TaskLedgerReader interface {
	// Runs 各类任务「最近一轮」的记录（kind → 记录）。
	Runs() map[string]taskledger.Run
	// RetryStates 各类任务的当日重试状态（kind → 状态）。
	RetryStates() map[string]taskledger.RetryState
}

// promTaskKinds 任务指标输出的 kind 维度固定顺序（与 admin_tasks.go 的白名单
// 同名同序）。顺序写死而非遍历 map：map 迭代序随机会让两次抓取输出不同，
// 破坏本仓库对「稳定输出」的既有要求。
var promTaskKinds = []string{"checkin", "activity", "keepalive", "travel", "school", "cat"}

// promTaskResults wb2api_task_last_run_accounts 的 result 维度固定顺序。
// total 也在其中——它是本轮涉及量，与 ok/fail/skipped 一起构成完整分解，
// 单独列一个指标反而要多一条 HELP/TYPE。
var promTaskResults = []string{"total", "ok", "already", "fail", "skipped"}

// promTaskResultValue 按 result 维度取最近一轮的计数。
func promTaskResultValue(r taskledger.Run, result string) int {
	switch result {
	case "total":
		return r.Total
	case "ok":
		return r.OK
	case "already":
		return r.Already
	case "fail":
		return r.Fail
	case "skipped":
		return r.Skipped
	}
	return 0
}

// taskLedgerStatus 组装 /status 的 task_ledger 段（未接线时返回 nil，调用方据此
// 不写该键）。runs 与 retry 直接交给 encoding/json——台账的契约就是这两个 map
// 的字段名（见 internal/taskledger 的 Run / RetryState 标签），server 侧不再
// 拆字段重组，避免两处字段集漂移。
func (h *Handler) taskLedgerStatus() map[string]any {
	if h.cfg.TaskLedger == nil {
		return nil
	}
	return map[string]any{
		"runs":  h.cfg.TaskLedger.Runs(),
		"retry": h.cfg.TaskLedger.RetryStates(),
	}
}
