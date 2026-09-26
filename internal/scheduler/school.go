// school.go 开学季任务与夜猫子任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
package scheduler

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// repoRoot 定位仓库根（容器内 /app、宿主 /root/workbuddy2api）。
// 策略：从当前工作目录逐级向上找 scripts/school_open_day_2026.py，
// 找不到回落 os.Getwd()（此时 Run 会因脚本缺失打 WARN，不 panic）。
// 注意：Go scheduler 在 cmd/server 内以工作目录启动（容器 WORKDIR /app），
// 若进程以别的工作目录拉起（如 systemd/裸 binary），上溯穷尽后仍以
// os.Getwd() 兜底，把缺失暴露成 WARN 而非静默。
func repoRoot() string {
	start, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
}

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }
func (c *scriptCmd) Run() error        { return c.cmd.Run() }

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	return &scriptCmd{cmd: exec.Command(program, args...)}
}

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 默认 "python3"，与容器/Linux 现状完全一致，行为零变更；WB2A_PYTHON
// 显式指定时优先，供解释器不叫 python3 的环境使用（命名对齐仓库 Go 侧
// WB2A_* env 约定，如 WB2A_AUTH_DIR / WB2A_LISTEN）。
//
// 需要该开关的原因：Windows 官方安装器只提供 python.exe，且 PATH 上常存在
// Microsoft Store 的 python3.exe App Execution Alias 存根——exec.Command 能找到
// 它却无法真正执行，脚本类任务统一报 `exit status 9009`。
// 设 WB2A_PYTHON=python 即可绕过。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	return "python3"
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
//
// 返回成功/失败命令数与最后一条失败摘要，供台账计数（note 取最后一条：多命令
// 全挂时最有价值的是「最近一次为什么挂」，逐条细节已在日志里）。
func runScript(name, root string, commands [][]string) (ok, fail int, note string) {
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if err := c.Run(); err != nil {
			fail++
			note = fmt.Sprintf("%s: %v", cmdArgs[1], err)
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		ok++
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
	return ok, fail, note
}

// runScriptTask 跑一组脚本命令并记台账：脚本类任务（school/cat）只有「命令级」
// 粒度——一条命令要么成功要么失败，没有账号维度的分解。
//
// 退出码非 0 即算 Fail（脚本自己会把「活动下线」「非窗口期」等正常态处理成 0 退出，
// 见 RunSchoolNow/RunCatNow 的注释），所以这里的 Fail 就是真的失败了。
func (s *Scheduler) runScriptTask(kind taskKind, trigger, name string, commands [][]string) {
	started := time.Now()
	okN, failN, note := runScript(name, repoRoot(), commands)
	s.recordRun(kind, trigger, started, runTally{
		total: okN + failN, ok: okN, fail: failN, note: note,
	})
}

// RunSchoolNow 立即执行开学季任务（人工入口）。
func (s *Scheduler) RunSchoolNow() { s.runSchool(triggerManual) }

// runSchool 执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) runSchool(trigger string) {
	s.runScriptTask(taskSchool, trigger, "school", [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
	})
}

// RunCatNow 立即执行夜猫子任务（人工入口）。
func (s *Scheduler) RunCatNow() { s.runCat(triggerManual) }

// runCat 执行夜猫子任务：task_runner.py ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（task_runner 内部
// 判定，非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) runCat(trigger string) {
	s.runScriptTask(taskCat, trigger, "cat", [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"},
	})
}
