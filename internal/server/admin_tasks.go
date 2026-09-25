// admin_tasks.go 手动触发排程任务的运维端点。
//
// 动机：六个积分任务只在自己的整点窗口跑（签到 9/21、活跃 10、保活 22、开学 12、
// 夜猫子 1、旅行 9/21）。窗口被错过时（服务刚重启、上游当时抖动、刚加完号），
// 此前只能干等到下一个整点。本端点让人工立刻补跑一次，不必重启服务或改时钟。
//
// 设计要点：
//   - **异步受理**：这些任务遍历全池打上游、部分还起 python 子进程，耗时可达
//     分钟级。同步等待会让调用方超时，且无法区分「卡住了」与「正在跑」——故立即
//     回 202 Accepted，任务在后台 goroutine 里跑完，进度看服务日志。
//   - **白名单派发**：路径段是外部输入，只放行固定六个任务名，不做任意派发。
//   - **同任务防重入**：同一任务已有一次手动触发在跑时回 409，避免连点对上游
//     重复写（对 WAF 不友好）。注意这与 scheduler 内部的 checkinMu 是两层：
//     那层管「手动 vs 定时」撞车，这层管「手动 vs 手动」连点。
//   - **零上游增量**：人工触发不新增任何自动上游请求，只是把既有任务提前跑一次。
//   - 默认关闭（config admin.enabled），与账号管理端点共用同一把 api_key 鉴权。
package server

import (
	"net/http"
	"strings"
)

// TaskRunner 手动触发排程任务的窄接口（server.Config.Tasks）。
//
// 用接口而非直接依赖 internal/scheduler：server 包不反向 import scheduler
// （避免 server → scheduler → pool 的环），同时测试可注入假实现来验证派发与
// 防重入，不必起真调度器。*scheduler.Scheduler 结构上即满足本接口——六个
// Run*Now 都是无参无返回的 func()，无需为接线改动 scheduler。
type TaskRunner interface {
	RunCheckinNow()
	RunActivityNow()
	RunKeepaliveNow()
	RunTravelNow()
	RunSchoolNow()
	RunCatNow()
}

// adminTaskNames 可手动触发的任务名白名单（稳定顺序，供 404 文案列举）。
// 与 config.Schedule 的六个 *_enabled 开关一一对应。
var adminTaskNames = []string{"checkin", "activity", "keepalive", "travel", "school", "cat"}

// adminTaskState 手动触发任务的响应体。只回「已受理」，不含执行结果——任务
// 异步执行（见文件头），结果看服务日志；这里回显任务名与状态便于脚本断言。
type adminTaskState struct {
	Task   string `json:"task"`
	Status string `json:"status"` // 固定 "started"
}

// taskRunners 把白名单任务名映射到触发函数。Tasks 未注入时返回 nil，
// handler 据此回 503 而不是静默 404——「配置开了但没接线」是运维应当看见的信号，
// 静默 404 会让它误判成「路由写错了」而白查一轮。
func (h *Handler) taskRunners() map[string]func() {
	t := h.cfg.Tasks
	if t == nil {
		return nil
	}
	return map[string]func(){
		"checkin":   t.RunCheckinNow,
		"activity":  t.RunActivityNow,
		"keepalive": t.RunKeepaliveNow,
		"travel":    t.RunTravelNow,
		"school":    t.RunSchoolNow,
		"cat":       t.RunCatNow,
	}
}

// adminTaskRun 手动触发一个排程任务：POST /admin/tasks/{name}/run。
// 受理成功回 202 + {"task":"...","status":"started"}。
func (h *Handler) adminTaskRun(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	runners := h.taskRunners()
	if runners == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unavailable", "task runner not wired")
		return
	}
	run, ok := runners[name]
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"unknown task: "+name+"; known: "+strings.Join(adminTaskNames, ", "))
		return
	}
	// 防重入是 TryLock 语义：已在跑就立刻 409，不排队。排队会让连点积压成一串
	// 对上游的重复写，正是这里要防的。
	h.taskMu.Lock()
	if h.taskRunning[name] {
		h.taskMu.Unlock()
		writeOpenAIError(w, http.StatusConflict, "busy", "task already running: "+name)
		return
	}
	h.taskRunning[name] = true
	h.taskMu.Unlock()

	go func() {
		// 无论正常结束还是提前 return 都要清标记，否则该任务的手动触发会被
		// 永久锁死（只能靠重启解）。
		defer func() {
			h.taskMu.Lock()
			delete(h.taskRunning, name)
			h.taskMu.Unlock()
		}()
		run()
	}()

	writeJSON(w, http.StatusAccepted, adminTaskState{Task: name, Status: "started"})
}
