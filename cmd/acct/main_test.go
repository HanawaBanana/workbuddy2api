// main_test.go acct 工具的契约：listen 归一化 + 状态文案 + 手动触发任务的客户端行为。
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeListen listen 的各种写法都要收敛成本机可访问的 http 基址。
// 通配地址（空 host / 0.0.0.0 / ::）必须收敛到回环——本工具总是与网关同机运行，
// 用 0.0.0.0 发请求在部分平台会失败。
func TestNormalizeListen(t *testing.T) {
	cases := []struct{ in, want string }{
		{":7863", "http://127.0.0.1:7863"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863"},
		{"::", "http://127.0.0.1:7863"},
		{"[::]:7863", "http://127.0.0.1:7863"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863"},
		{"localhost:9999", "http://localhost:9999"},
		{"", "http://127.0.0.1:7863"},           // 空 → 默认端口
		{"127.0.0.1:", "http://127.0.0.1:7863"}, // 有 host 无端口
		{"7863", "http://127.0.0.1:7863"},       // 只有端口（无冒号 host 段）
	}
	for _, c := range cases {
		if got := normalizeListen(c.in); got != c.want {
			t.Errorf("normalizeListen(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStateLabel 双位状态文案：叠加态两个都列，不合并。
func TestStateLabel(t *testing.T) {
	cases := []struct {
		name string
		in   accountStatus
		want string
	}{
		{"正常", accountStatus{}, "正常"},
		{"仅手动", accountStatus{ManualDisabled: true}, "手动停用"},
		{"仅自动", accountStatus{Disabled: true}, "自动禁用"},
		{"叠加", accountStatus{ManualDisabled: true, Disabled: true}, "手动停用 + 自动禁用"},
		{"带原因", accountStatus{ManualDisabled: true, ManualReason: "观察"}, "手动停用(观察)"},
		{"冷却", accountStatus{Cooling: true}, "冷却中"},
		{"三者", accountStatus{ManualDisabled: true, Disabled: true, Cooling: true},
			"手动停用 + 自动禁用 + 冷却中"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stateLabel(c.in); got != c.want {
				t.Errorf("stateLabel() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRunTask 手动触发任务的客户端契约：202 视为受理（命令不等任务跑完），
// 其余状态各自给出**可操作**的提示，而不是把状态码原样抛给运维。
// 用 httptest 起假网关，不起真服务、不打真上游。
func TestRunTask(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		body    string
		wantErr string // 空 = 期望成功
	}{
		{"受理", http.StatusAccepted, `{"task":"checkin","status":"started"}`, ""},
		{"任务名不认", http.StatusNotFound, `{"error":{"message":"unknown task: bogus"}}`, "任务名不存在"},
		{"已有一趟在跑", http.StatusConflict, `{"error":{"message":"task already running: checkin"}}`, "已有一趟在跑"},
		{"网关未接线", http.StatusServiceUnavailable, `{"error":{"message":"task runner not wired"}}`, "未接线"},
		{"key 不对", http.StatusUnauthorized, `{"error":{"message":"missing or invalid API key"}}`, "api_key 不对"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 路径与方法必须对：打错端点会静默 404，正是这里要锁的。
				if r.URL.Path != "/admin/tasks/checkin/run" {
					t.Errorf("path=%q want /admin/tasks/checkin/run", r.URL.Path)
				}
				if r.Method != http.MethodPost {
					t.Errorf("method=%q want POST", r.Method)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer k" {
					t.Errorf("Authorization=%q want Bearer k", got)
				}
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			err := runTask(srv.URL, "k", "checkin")
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err=%q want containing %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestAdminTaskNamesMatchServerWhitelist acct 侧任务名与 server 侧白名单必须一致。
// 两处各写一份是有意的（cmd 不该 import internal/server 的运行时表），但一旦漂移
// 就会变成「命令提示的名字网关不认」，所以用一条哨兵测试钉住。
func TestAdminTaskNamesMatchServerWhitelist(t *testing.T) {
	want := []string{"checkin", "activity", "keepalive", "travel", "school", "cat"}
	if strings.Join(adminTaskNames, ",") != strings.Join(want, ",") {
		t.Fatalf("acct 任务名 = %v, want %v（与 internal/server/admin_tasks.go 的 adminTaskNames 对齐）",
			adminTaskNames, want)
	}
}
