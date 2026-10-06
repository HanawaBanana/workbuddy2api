package main

import "testing"

// TestDeliberateFailure 故意失败：作为 PR CI Notify 的端到端验证输入。
// 预期：PR CI 红 → PR CI Notify 在 PR 上评论并 @ 作者（同一次运行只评论一次）。
func TestDeliberateFailure(t *testing.T) {
	t.Fatal("deliberate failure to verify PR CI Notify")
}
