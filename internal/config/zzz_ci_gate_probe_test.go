package config

import "testing"

// CI 门禁探针：故意失败，用于验证「AI 打了 ai-approved 但 CI 红时 auto-merge 不会合并」。
func TestCIGateProbeDeliberateFailure(t *testing.T) {
	if 1 != 2 {
		t.Fatal("deliberate failure for CI-gate verification")
	}
}
