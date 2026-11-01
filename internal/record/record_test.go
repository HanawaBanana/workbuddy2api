package record

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func at(y int, mo time.Month, d, h int) time.Time {
	return time.Date(y, mo, d, h, 0, 0, 0, time.Local)
}

// TestAddAndQuery 基本写入与倒序查询。
func TestAddAndQuery(t *testing.T) {
	c := New("")
	now := at(2026, 10, 25, 9)
	c.nowFunc = func() time.Time { return now }

	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK, Credits: 100})
	now = at(2026, 10, 25, 21)
	c.Add(Entry{Kind: KindTravel, UID: "u1", Status: StatusOK, Reward: 50})

	got := c.Query(Query{})
	if len(got) != 2 {
		t.Fatalf("应有 2 条, 得到 %d", len(got))
	}
	// 最新在前。
	if got[0].Kind != KindTravel {
		t.Errorf("应最新在前, 得到 %v", got[0].Kind)
	}
}

// TestQueryFilters kind / uid 过滤。
func TestQueryFilters(t *testing.T) {
	c := New("")
	now := at(2026, 10, 25, 9)
	c.nowFunc = func() time.Time { return now }

	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})
	c.Add(Entry{Kind: KindTravel, UID: "u1", Status: StatusOK})
	c.Add(Entry{Kind: KindCheckin, UID: "u2", Status: StatusOK})

	if got := c.Query(Query{Kind: KindCheckin}); len(got) != 2 {
		t.Errorf("kind 过滤应得 2 条, 得到 %d", len(got))
	}
	if got := c.Query(Query{UID: "u1"}); len(got) != 2 {
		t.Errorf("uid 过滤应得 2 条, 得到 %d", len(got))
	}
	if got := c.Query(Query{Kind: KindCheckin, UID: "u2"}); len(got) != 1 {
		t.Errorf("组合过滤应得 1 条, 得到 %d", len(got))
	}
}

// TestQueryTimeRange 时间范围过滤（含 Since、不含 Until）。
func TestQueryTimeRange(t *testing.T) {
	c := New("")
	now := at(2026, 10, 24, 9)
	c.nowFunc = func() time.Time { return now }
	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})

	now = at(2026, 10, 25, 9)
	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})

	got := c.Query(Query{Since: at(2026, 10, 25, 0), Until: at(2026, 10, 26, 0)})
	if len(got) != 1 {
		t.Fatalf("时间过滤应得 1 条, 得到 %d", len(got))
	}
	if got[0].At.Day() != 25 {
		t.Errorf("应是 25 号, 得到 %v", got[0].At)
	}
}

// TestQueryLimit 限制条数且保留最新的。
func TestQueryLimit(t *testing.T) {
	c := New("")
	base := at(2026, 10, 25, 0)
	for i := 0; i < 50; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		c.nowFunc = func() time.Time { return now }
		c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})
	}
	got := c.Query(Query{Limit: 10})
	if len(got) != 10 {
		t.Fatalf("应得 10 条, 得到 %d", len(got))
	}
	// 保留的应是最新的（第 49 分钟那条）。
	if got[0].At != base.Add(49*time.Minute) {
		t.Errorf("应保留最新记录, 得到 %v", got[0].At)
	}
}

// TestTrimToMax 超过上限时裁掉最旧的。
func TestTrimToMax(t *testing.T) {
	c := New("")
	c.SetMax(5)
	for i := 0; i < 12; i++ {
		now := at(2026, 10, 25, 0).Add(time.Duration(i) * time.Minute)
		c.nowFunc = func() time.Time { return now }
		c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})
	}
	if c.Len() != 5 {
		t.Fatalf("应裁剪到 5 条, 得到 %d", c.Len())
	}
	if got := c.Query(Query{}); got[0].At != at(2026, 10, 25, 0).Add(11*time.Minute) {
		t.Error("应保留最新 5 条")
	}
}

// TestSummary 汇总统计（今日成功/失败/奖励）。
func TestSummary(t *testing.T) {
	c := New("")
	now := at(2026, 10, 25, 9)
	c.nowFunc = func() time.Time { return now }

	// 昨天的一条（不计入今日）
	now = at(2026, 10, 24, 9)
	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})

	// 今天：2 成功（含奖励）+ 1 失败
	now = at(2026, 10, 25, 9)
	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})
	c.Add(Entry{Kind: KindTravel, UID: "u1", Status: StatusOK, Reward: 50})
	c.Add(Entry{Kind: KindCheckin, UID: "u2", Status: StatusFailed})

	s := c.Summary()
	if s.Total != 4 {
		t.Errorf("Total = %d, want 4", s.Total)
	}
	if s.TodayOK != 2 {
		t.Errorf("TodayOK = %d, want 2", s.TodayOK)
	}
	if s.TodayFailed != 1 {
		t.Errorf("TodayFailed = %d, want 1", s.TodayFailed)
	}
	if s.TodayReward != 50 {
		t.Errorf("TodayReward = %d, want 50", s.TodayReward)
	}
	if s.ByKind[string(KindCheckin)] != 3 {
		t.Errorf("ByKind[checkin] = %d, want 3", s.ByKind[string(KindCheckin)])
	}
}

// TestPersistence 落盘后重新加载。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "records.json")

	c := New(fp)
	c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK, Credits: 123})
	c.Flush()

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("应生成文件: %v", err)
	}

	c2 := New(fp)
	got := c2.Query(Query{})
	if len(got) != 1 {
		t.Fatalf("重启后应恢复 1 条, 得到 %d", len(got))
	}
	if got[0].Credits != 123 {
		t.Errorf("Credits = %d, want 123", got[0].Credits)
	}
}

// TestLoadCorruptFile 文件损坏时从空开始，不 panic。
func TestLoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "records.json")
	os.WriteFile(fp, []byte("{not json"), 0o600)

	c := New(fp)
	if c.Len() != 0 {
		t.Errorf("损坏文件应忽略, 得到 %d 条", c.Len())
	}
}

// TestConcurrentAdd 并发写入不丢数据、无竞态。
func TestConcurrentAdd(t *testing.T) {
	c := New("")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Add(Entry{Kind: KindCheckin, UID: "u1", Status: StatusOK})
		}(i)
	}
	wg.Wait()
	if c.Len() != 100 {
		t.Errorf("应 100 条, 得到 %d", c.Len())
	}
}

// TestUnknownNilCollectorNilSafe 空 collector 的 Query 不 panic。
func TestQueryOnEmpty(t *testing.T) {
	c := New("")
	if got := c.Query(Query{}); len(got) != 0 {
		t.Errorf("空 collector 应返回 0 条, 得到 %d", len(got))
	}
	if s := c.Summary(); s.Total != 0 {
		t.Errorf("空 collector Summary.Total 应为 0, 得到 %d", s.Total)
	}
}
