// admin_audit_config_test.go 管理操作审计（admin.audit_enabled / admin.audit_file）
// 的配置契约测试。
//
// 覆盖六步法里的校验与缺省语义：
//  1. 缺省关闭，且**老 config（只有 admin.enabled）行为逐字不变**；
//  2. env 覆盖（WB2A_ADMIN_AUDIT_ENABLED / WB2A_ADMIN_AUDIT_FILE），非法值忽略；
//  3. 两处 fail-fast：开了审计却没开 admin、开了审计却没给路径；
//  4. config.example.json 仍能被 Load 解析——Dockerfile 把它 COPY 成容器默认
//     /app/config.json，示例文件语法坏掉等于新容器起不来。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg 把 content 写进临时 config 文件并返回路径。
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestAdminAuditDefaultsOff 缺省关闭，且路径有默认值（与 state_file 同目录约定）。
func TestAdminAuditDefaultsOff(t *testing.T) {
	c, err := Load(writeCfg(t, `{"api_key":"k","admin":{"enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.AuditEnabled {
		t.Error("admin.audit_enabled 应缺省 false（老部署不该被动多出一个磁盘文件）")
	}
	if c.Admin.AuditFile != "./data/admin_audit.log" {
		t.Errorf("audit_file 默认值 = %q, want ./data/admin_audit.log", c.Admin.AuditFile)
	}
}

// TestAdminAuditLegacyConfigUnchanged 只含旧键的 config 与引入本功能前的加载结果
// 逐字一致：审计关着、路径惰性，不产生任何可观测差异。
func TestAdminAuditLegacyConfigUnchanged(t *testing.T) {
	// 完全不提 admin 段
	c, err := Load(writeCfg(t, `{"api_key":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.Enabled || c.Admin.AuditEnabled {
		t.Errorf("空 admin 段应全关, got %+v", c.Admin)
	}

	// 只提 enabled（本功能引入前的形态）
	c, err = Load(writeCfg(t, `{"api_key":"k","admin":{"enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.AuditEnabled {
		t.Error("只有 admin.enabled 时审计必须保持关闭")
	}
}

// TestAdminAuditExplicitConfigPreserved 显式配置的开关与路径要原样保留。
func TestAdminAuditExplicitConfigPreserved(t *testing.T) {
	c, err := Load(writeCfg(t,
		`{"api_key":"k","admin":{"enabled":true,"audit_enabled":true,"audit_file":"/var/log/wb2a/audit.jsonl"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.AuditEnabled {
		t.Error("audit_enabled=true 应生效")
	}
	if c.Admin.AuditFile != "/var/log/wb2a/audit.jsonl" {
		t.Errorf("audit_file=%q", c.Admin.AuditFile)
	}
}

// TestAdminAuditRequiresAdminEnabled fail-fast：开了审计却没开 admin。
// 审计的对象就是 /admin 端点，端点不存在时审计永远不会写一行——这种空配置比报错
// 更危险，因为运维会以为「审计已经在跑了」。
func TestAdminAuditRequiresAdminEnabled(t *testing.T) {
	_, err := Load(writeCfg(t, `{"api_key":"k","admin":{"enabled":false,"audit_enabled":true}}`))
	if err == nil {
		t.Fatal("audit_enabled=true + admin.enabled=false 应拒绝启动")
	}
	if !strings.Contains(err.Error(), "audit_enabled") || !strings.Contains(err.Error(), "admin.enabled") {
		t.Errorf("错误文案应同时点到两个键, got %q", err.Error())
	}
}

// TestAdminAuditFileRequiredWhenEnabled fail-fast：开了审计但路径被显式置空。
func TestAdminAuditFileRequiredWhenEnabled(t *testing.T) {
	_, err := Load(writeCfg(t,
		`{"api_key":"k","admin":{"enabled":true,"audit_enabled":true,"audit_file":"   "}}`))
	if err == nil {
		t.Fatal("audit_enabled=true + 空 audit_file 应拒绝启动")
	}
	if !strings.Contains(err.Error(), "audit_file") {
		t.Errorf("错误文案应点到 audit_file, got %q", err.Error())
	}
}

// TestAdminAuditEnvOverride env 覆盖两个键（对齐 WB2A_ADMIN_ENABLED 风格：
// ParseBool，非法值忽略不报错）。
func TestAdminAuditEnvOverride(t *testing.T) {
	fp := writeCfg(t, `{"api_key":"k","admin":{"enabled":true}}`)

	t.Setenv("WB2A_ADMIN_AUDIT_ENABLED", "true")
	t.Setenv("WB2A_ADMIN_AUDIT_FILE", "/tmp/x.jsonl")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.AuditEnabled || c.Admin.AuditFile != "/tmp/x.jsonl" {
		t.Fatalf("env 未生效: %+v", c.Admin)
	}

	// env 关闭覆盖 config 的 true
	fp2 := writeCfg(t, `{"api_key":"k","admin":{"enabled":true,"audit_enabled":true}}`)
	t.Setenv("WB2A_ADMIN_AUDIT_ENABLED", "false")
	c, err = Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.AuditEnabled {
		t.Error("WB2A_ADMIN_AUDIT_ENABLED=false 应覆盖 config 的 true")
	}

	// 非法 bool 忽略：保持 config 值
	t.Setenv("WB2A_ADMIN_AUDIT_ENABLED", "yes-please")
	c, err = Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.AuditEnabled {
		t.Error("非法 env 值应被忽略（保持 config 值）")
	}
}

// TestAdminAuditEnvEnabledFailsFastWithoutAdmin env 开启审计、config 未开 admin →
// 同样 fail-fast（env 覆盖先于 normalize，两条入口一致拦截）。
func TestAdminAuditEnvEnabledFailsFastWithoutAdmin(t *testing.T) {
	t.Setenv("WB2A_ADMIN_AUDIT_ENABLED", "1")
	_, err := Load(writeCfg(t, `{"api_key":"k","admin":{"enabled":false}}`))
	if err == nil {
		t.Fatal("env 开审计 + admin 关闭也应 fail-fast")
	}
}

// TestConfigExampleLoads config.example.json 必须仍能被 Load 完整解析。
// Dockerfile 把它 COPY 成 /app/config.json，语法坏掉等于新容器起不来；
// 而它是人手维护的，加配置键时最容易只改结构体忘了改它。
func TestConfigExampleLoads(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json 应能被 Load 解析: %v", err)
	}
	// 示例文件里的值要与实现默认值一致，否则「照抄示例」与「不写该键」会得到
	// 两种行为，示例就失去了参照意义。
	if c.Admin.AuditEnabled {
		t.Error("示例文件里 audit_enabled 应为 false（缺省关闭）")
	}
	if c.Admin.AuditFile != "./data/admin_audit.log" {
		t.Errorf("示例文件 audit_file=%q 与默认值不一致", c.Admin.AuditFile)
	}
}
