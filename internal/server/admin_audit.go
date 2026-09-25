// admin_audit.go /admin 操作审计日志。
//
// 动机：/admin 下的四个动作（账号 disable/enable/revive、手动触发任务）都是**变更
// 状态**的运维操作，但此前只在 stdout 留一行请求日志，进程一重启就散。事后要回答
// 「这个号是谁停的、什么时候停的、给的理由是什么」，只能翻终端回滚缓冲。本模块把
// 每次管理操作追加一行 JSONL 到 data/ 下的文件，供事后追溯。
//
// 设计要点：
//   - **默认关闭**（config admin.audit_enabled）：审计会凭空多出一个磁盘文件，老部署
//     即便开了 admin 也不该被动产生新文件，故与 admin.enabled 拆成两个开关。
//   - **每次操作独立开-写-关**，不长期持有 fd。管理操作是人手动触发的低频事件，开
//     文件的代价可忽略；换来的是与外部 logrotate（mv 走旧文件后进程自建新的）天然
//     兼容——若长期持有 fd，轮转后日志会继续写进已被移走的旧文件而静默丢失。
//   - **写失败不失败请求**：磁盘满 / 权限错时只往 stderr 喊一声，管理操作照常完成。
//     反过来的「审计写不进去就拒绝操作」会把一次磁盘故障升级成「坏账号摘不掉」的
//     可用性事故；而操作结果本身已落在 pool 状态里，不会因此丢失。
//   - **不记 key 原文**：只记 api_key 的 sha256 前 8 hex，用于区分「轮换前后」。
//     审计文件虽然在运维自己的盘上，但一份被误发出去的日志不该等于一次凭证泄露。
//   - **只记通过鉴权的请求**：中间件挂在 withAuth **之内**（见 audit 注释），匿名
//     探测不落盘——否则等于给未鉴权方开了一个「写你的磁盘」的口子。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AuditLog 把 /admin 的每次操作以一行 JSONL 追加到文件。
//
// 零值不可用（path 为空会让 record 每次都开文件失败）；经 NewAuditLog 构造。
// 并发安全：record 内部持锁，多个管理请求同时到达时不会把两行 JSON 交错写进
// 同一个偏移。
type AuditLog struct {
	path  string
	keyFP string
	mu    sync.Mutex
}

// NewAuditLog 构造审计接收器，并做一次「可写性预检」。
//
// 预检的理由：路径不可写（目录只读、父路径是文件、属主不对）在运维上是最常见的
// 审计失效原因，且完全可以在启动时发现。放到第一次管理操作才暴露，等于把一次
// 「配置错」推迟成「真出事时才发现审计是空的」——那正是审计最没用的时刻。
// 预检会创建父目录（与 state_file 同约定：./data/ 由进程自建）并创建/打开文件。
func NewAuditLog(path, apiKey string) (*AuditLog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("审计日志路径为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建审计日志目录 %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志 %s: %w", path, err)
	}
	_ = f.Close()
	return &AuditLog{path: path, keyFP: keyFingerprint(apiKey)}, nil
}

// Path 返回审计文件路径（供启动日志与测试断言）。
func (a *AuditLog) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// keyFingerprint 返回 api_key 的 sha256 前 8 hex（32 bit）；空 key 返回 "-"。
//
// 8 hex 足够区分「换 key 之前/之后」两批记录，又不足以反推原文——审计要的是
// 「哪把凭证」，不是凭证本身。
func keyFingerprint(apiKey string) string {
	if apiKey == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:4])
}

// auditEntry 一行审计记录。
//
// 字段声明顺序即 JSON 输出顺序：ts/action/target 在最前，方便人眼直接扫一列
// 「什么时候、对谁、做了什么」。
type auditEntry struct {
	TS     string `json:"ts"`     // RFC3339（本地时区，带偏移）
	Action string `json:"action"` // 稳定标识，如 "account.disable" / "task.run"
	Target string `json:"target"` // 被操作对象：账号 uid 或任务名
	Status int    `json:"status"` // 该次操作的 HTTP 响应码
	Remote string `json:"remote"` // TCP 对端 host:port
	Key    string `json:"key"`    // api_key 指纹（非原文）
	// Reason 运维给的理由（仅 disable 会带）。这是审计独有的信息：pool 状态里
	// 的 manual_reason 只保留**最新**一条，改一次就覆盖，历史只在这里。
	Reason string `json:"reason,omitempty"`
}

// record 追加一行 JSONL。任何失败都只报 stderr，绝不返回错误给调用方——
// 调用方是已生效的管理操作，不该被审计侧的问题拖下水（见文件头）。
func (a *AuditLog) record(e auditEntry) {
	if a == nil {
		return
	}
	e.Key = a.keyFP
	line, err := json.Marshal(e)
	if err != nil {
		// 结构体全是有序标量字段，实际不可达；留着是为了万一将来加了
		// 不可序列化字段时不至于静默丢记录。
		log.Printf("[audit] 序列化失败，本条未落盘（操作已生效）：%v", err)
		return
	}
	line = append(line, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("[audit] 打开 %s 失败，本条未落盘（操作已生效）：%v", a.path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		log.Printf("[audit] 写入 %s 失败，本条未落盘（操作已生效）：%v", a.path, err)
	}
}

// auditBodyPeek 审计读取请求体的上限。取 4KB 与 adminReasonFromBody 的读取上限
// 一致：读到这个量足以取出 reason，而真实管理请求体（一个 reason 字符串）远小于它。
const auditBodyPeek = 4 << 10

// audit 包装一个 admin handler，在它返回后追加一行审计记录。
//
// 调用形态是 h.withAuth(h.audit(...)) —— 中间件在**鉴权之内**：
//   - 未通过鉴权的探测不是「管理操作」，记进审计文件反而是噪音；
//   - 更重要的是，若把匿名请求也记进去，任何人都能靠刷 /admin 把运维的磁盘写满，
//     等于用一个安全特性换来一个 DoS 面。
//
// 另外：handler 若 panic 则不落行。panic 是 bug，由 http 服务端自身的 recover
// 报错；为它编造一条状态码不明的审计行只会误导事后排查。
func (h *Handler) audit(action string, target func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	if h.cfg.Audit == nil {
		return next // 未开启：零开销，且与引入前的行为逐字一致
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// 先取 reason 并复原 body，再交给 handler——审计读 body 不能改变请求语义。
		reason := peekReason(r)
		rec := &auditResponseWriter{ResponseWriter: w}
		next(rec, r)
		h.cfg.Audit.record(auditEntry{
			TS:     time.Now().Format(time.RFC3339),
			Action: action,
			Target: target(r),
			Status: rec.statusCode(),
			Remote: r.RemoteAddr,
			Reason: reason,
		})
	}
}

// auditPathValue 造一个从路径段取审计目标的提取器。
// 显式传字段名而不是猜「uid 还是 name」：审计记错对象是静默的语义错误，
// 写清楚比省几行更重要。
func auditPathValue(field string) func(*http.Request) string {
	return func(r *http.Request) string { return r.PathValue(field) }
}

// auditResponseWriter 捕获响应状态码供审计。
//
// 不透传 http.Flusher / http.Hijacker：admin handler 只写 JSON、从不流式也不
// 劫持连接，包一层不会改变任何被包装方依赖的能力。
type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code // 只认第一次：重复 WriteHeader 时后续调用本就无效
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // 隐式 200（handler 只 Write 未 WriteHeader）
	}
	return w.ResponseWriter.Write(b)
}

// statusCode 返回捕获到的状态码；handler 什么都没写时按 200 计
// （admin handler 的每条路径都会写响应，故这只在异常情况下兜底）。
func (w *auditResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// peekReason 读出请求体前 auditBodyPeek 字节里的 reason 字段，并把 body 复原。
//
// 用 MultiReader 拼回「已读前缀 + 原 body 剩余」而不是「读满后换成新 reader」：
// body 超过上限时，未读部分仍挂在原 body 上，handler 拿到的是字节与顺序都完整的
// 流——审计读一眼不该让 handler 少读一个字节。
//
// 读取错误一律忽略：拿到多少算多少，复原后交给 handler 自己去碰真正的错误
// （handler 的 io.ReadAll 会把 err 如实反映出来，审计不该抢先吞掉或放大它）。
func peekReason(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(r.Body, auditBodyPeek))
	r.Body = auditBody{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}

	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(buf, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// auditBody 把「已缓冲的前缀」与「原 body 剩余」拼成一个仍可 Close 的流。
// 嵌入两个接口而不是自写方法：Read 来自 MultiReader，Close 转交原 body，
// 语义一眼可见。
type auditBody struct {
	io.Reader
	io.Closer
}
