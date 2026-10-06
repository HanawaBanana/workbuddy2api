// checkin_history.go 签到历史的落盘与只读查询。
//
// 动机：号池的签到结果此前**只存在于 POST /v1/checkin 的 HTTP 响应里**——进程内不缓存、
// 磁盘无文件，日志只有一行聚合计数（且「签到成功」与「今日已签」两条路径**都不打日志**）。
// 于是控制台无法回答「今天签到了吗 / 哪个号还没签」：刷新页面即失忆，自动 9/21 那次的
// 结果更是完全看不到（详见 2026-09-29 任务书）。
//
// 本文件补上这一环：
//   - CheckinHistoryStore：把每次全量签到的结果**原子落盘**到 data/checkin.json，
//     滚动保留最近 N 次且不超过 7 天。
//   - GET /v1/checkin/history：只读回吐历史 + 下一个自动签到时点（含 jitter 派生，
//     复用 scheduler 的同一份算法，避免前端另写一套倒计时逻辑而漂移）。
//
// ★ 为什么端点挂在 withOps（非分组密钥）而不是 admin 闸下 ★
//
//	admin.enabled 当前为 false（生产实测 admin 段为 null），挂 admin 闸会让功能直接不可用；
//	而签到历史含**全部账号的昵称与 UID**，属运维信息，不能开给分组密钥（那是发给外部
//	调用方的受限凭证）。withAuth + withOps = 「主密钥可用、分组密钥 403」，既不依赖
//	admin 开关、也不泄露账号清单。
//
// 注意与 POST /v1/checkin 的分工：那个是**动作**（会打上游），本文件是**只读回放**。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// checkinHistoryMaxAge 历史保留窗口。签到每天 2 次（9/21 点），7 天 ≈ 14 条；
	// 留 7 天足够回答「今天/昨天签了吗」，又不至于让文件无限增长。
	checkinHistoryMaxAge = 7 * 24 * time.Hour
	// checkinHistoryMaxKeep 条数上限（双保险：手动补跑频繁时按条数兜底裁剪）。
	checkinHistoryMaxKeep = 200
	// checkinHistoryVersion 文件格式版本，便于将来迁移。
	checkinHistoryVersion = 1
)

// CheckinRecord 一次全量签到的完整快照（含逐账号结果）。
//
// 字段与 CheckinReport 刻意保持同构：Results 直接复用 CheckinResult，前端两处
// （手动签到的即时回执 / 历史回放）可以用同一套渲染逻辑，不必维护两份状态映射。
type CheckinRecord struct {
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Total      int             `json:"total"`
	OK         int             `json:"ok"`
	Already    int             `json:"already"`
	Fail       int             `json:"fail"`
	Skipped    int             `json:"skipped"`
	Results    []CheckinResult `json:"results"`
}

// checkinHistoryFile 落盘结构。用对象包一层而非裸数组：将来加字段（如「手动/定时」
// 来源标记）时不必破坏兼容。
type checkinHistoryFile struct {
	Version int             `json:"version"`
	Records []CheckinRecord `json:"records"`
}

// CheckinHistoryStore 签到历史的进程内副本 + 落盘。零值不可用，用 NewCheckinHistoryStore。
//
// 线程安全：Append 由调度器回调调用（可能来自多个任务 goroutine），Snapshot 由
// HTTP handler 并发调用，故全部经 mu 串行。落盘在锁外做（先取快照再写），
// 避免慢盘阻塞读取方。
type CheckinHistoryStore struct {
	mu   sync.Mutex
	path string
	recs []CheckinRecord // 按时间升序（旧 → 新）
}

// NewCheckinHistoryStore 构造。path 为空 = 禁用落盘（纯内存，供测试）。
func NewCheckinHistoryStore(path string) *CheckinHistoryStore {
	return &CheckinHistoryStore{path: path}
}

// Load 启动时载入已有历史。文件缺失视为「还没有历史」（不报错）；解析失败时把
// 损坏文件改名留证再从空开始——**绝不静默覆盖**，否则一次写入抖动会永久丢掉历史。
func (s *CheckinHistoryStore) Load() {
	if s == nil || s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[checkin-history] 读取 %s 失败：%v（从空历史开始）", s.path, err)
		}
		return
	}
	var f checkinHistoryFile
	if err := json.Unmarshal(b, &f); err != nil {
		bak := s.path + ".corrupt-" + time.Now().Format("20060102-150405")
		if rerr := os.Rename(s.path, bak); rerr != nil {
			log.Printf("[checkin-history] 解析 %s 失败：%v（改名留证也失败：%v）", s.path, err, rerr)
		} else {
			log.Printf("[checkin-history] 解析 %s 失败：%v（已改名留证为 %s，从空历史开始）", s.path, err, bak)
		}
		return
	}
	s.mu.Lock()
	s.recs = f.Records
	s.mu.Unlock()
	log.Printf("[checkin-history] 已载入 %d 条签到历史（%s）", len(f.Records), s.path)
}

// Append 追加一次签到记录并落盘。rep 来自 buildCheckinReport（与手动入口同一份口径，
// 保证「回执」与「历史」永远一致）；started/finished 为本次签到起止时刻。
func (s *CheckinHistoryStore) Append(rep CheckinReport, started, finished time.Time) {
	if s == nil {
		return
	}
	rec := CheckinRecord{
		StartedAt:  started,
		FinishedAt: finished,
		Total:      rep.Total,
		OK:         rep.OK,
		Already:    rep.Already,
		Fail:       rep.Fail,
		Skipped:    rep.Skipped,
		Results:    rep.Results,
	}
	if rec.Results == nil {
		// 与 HTTP 侧同口径：JSON 里是 [] 而不是 null，前端直接 .map 不会炸。
		rec.Results = []CheckinResult{}
	}
	s.mu.Lock()
	s.recs = append(s.recs, rec)
	s.recs = pruneCheckinRecords(s.recs, finished)
	snapshot := append([]CheckinRecord(nil), s.recs...)
	s.mu.Unlock()

	if s.path == "" {
		return
	}
	if err := s.writeAll(snapshot); err != nil {
		log.Printf("[checkin-history] 落盘 %s 失败：%v（历史仅存内存，重启即失）", s.path, err)
	}
}

// Snapshot 返回历史副本（调用方随意改，不影响内部状态）。无记录时返回空切片而非 nil。
func (s *CheckinHistoryStore) Snapshot() []CheckinRecord {
	if s == nil {
		return []CheckinRecord{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CheckinRecord, len(s.recs))
	copy(out, s.recs)
	return out
}

// pruneCheckinRecords 按「不超过 7 天」+「不超过 checkinHistoryMaxKeep 条」双条件裁剪。
// recs 按时间升序，故过期项必在头部、超额项在头部，两步都从头部丢。
func pruneCheckinRecords(recs []CheckinRecord, now time.Time) []CheckinRecord {
	cut := now.Add(-checkinHistoryMaxAge)
	kept := make([]CheckinRecord, 0, len(recs))
	for _, r := range recs {
		if r.FinishedAt.After(cut) {
			kept = append(kept, r)
		}
	}
	if len(kept) > checkinHistoryMaxKeep {
		kept = kept[len(kept)-checkinHistoryMaxKeep:]
	}
	return kept
}

// writeAll 原子写整份历史：同目录 tmp → chmod 0600 → rename。
// 0600 是硬要求：文件含全部账号的昵称与 UID（与 state.json / auths 同级敏感）。
func (s *CheckinHistoryStore) writeAll(recs []CheckinRecord) error {
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(checkinHistoryFile{Version: checkinHistoryVersion, Records: recs}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// WriteFile 的权限受 umask 影响，显式再 chmod 一次确保 0600。
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---------------------------------------------------------------------------
// HTTP 只读端点
// ---------------------------------------------------------------------------

// CheckinScheduleProvider 只读排程快照 + 下一个签到时点（由 *scheduler.Scheduler 实现）。
//
// 为什么把「下一个时点」交给调度器算而不是前端算：排程带**确定性 jitter 派生**
// （FNV-1a(taskKind|YYYY-MM-DDTHH) % jitter_minutes 分钟）。前端自己复刻这套算法迟早
// 漂移——一旦有人开了 jitter_minutes，倒计时就会算错且没人发现。让唯一实现方给答案，
// 前端只做「倒计时 = next_fire_at - now」这一件事。
type CheckinScheduleProvider interface {
	// CheckinSchedule 返回 (是否启用, 触发小时, jitter 分钟)。
	CheckinSchedule() (enabled bool, hours []int, jitterMinutes int)
	// NextCheckinAt 返回 now 之后最近的一个自动签到时点；已禁用/无排程返回零值。
	NextCheckinAt(now time.Time) time.Time
}

// CheckinHistoryResponse GET /v1/checkin/history 的响应体。
//
// 刻意带上 server_now / tz / tz_offset_seconds：倒计时的正确基准是**服务端时区**
// （排程小时就是服务端本地小时），客户端可能在其他时区。前端用 server_now 建立
// 时钟偏移后再算，跨时区也不会算错。
type CheckinHistoryResponse struct {
	ServerNow       string          `json:"server_now"`        // RFC3339，服务端当前时刻
	TZ              string          `json:"tz"`                // 服务端时区缩写，如 CST
	TZOffsetSeconds int             `json:"tz_offset_seconds"` // 服务端相对 UTC 的偏移秒数
	Today           string          `json:"today"`             // 服务端本地自然日 YYYY-MM-DD
	Enabled         bool            `json:"enabled"`
	Hours           []int           `json:"hours"`
	JitterMinutes   int             `json:"jitter_minutes"`
	NextFireAt      *time.Time      `json:"next_fire_at,omitempty"` // 下一个自动签到时点
	Records         []CheckinRecord `json:"records"`
}

// checkinHistory 处理 GET /v1/checkin/history：只读回放签到历史，不触发任何上游请求。
func (h *Handler) checkinHistory(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	_, off := now.Zone()
	resp := CheckinHistoryResponse{
		ServerNow:       now.Format(time.RFC3339),
		TZ:              now.Format("MST"),
		TZOffsetSeconds: off,
		Today:           now.Format("2006-01-02"),
		Hours:           []int{},
		Records:         []CheckinRecord{},
	}
	if st := h.cfg.CheckinHistory; st != nil {
		resp.Records = st.Snapshot()
	}
	if sp := h.cfg.CheckinSchedule; sp != nil {
		enabled, hours, jitter := sp.CheckinSchedule()
		resp.Enabled, resp.JitterMinutes = enabled, jitter
		if hours != nil {
			resp.Hours = hours
		}
		if nf := sp.NextCheckinAt(now); !nf.IsZero() {
			resp.NextFireAt = &nf
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
