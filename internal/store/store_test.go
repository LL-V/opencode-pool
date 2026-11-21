package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ts(minute int) time.Time {
	return time.Unix(1763700000, 0).Add(time.Duration(minute) * time.Minute)
}

// —— 窗口单调更新（±5min 容差 + 提前收窗）——

func TestObserveWindowMonotonic(t *testing.T) {
	s := New("")
	s.EnsureAccount("a1")
	s.ObserveWindowSource("a1", "five_hour", "api", 40, ts(300), ts(0))     // until=+300min
	s.ObserveWindowSource("a1", "five_hour", "passive", 50, ts(298), ts(0)) // 容差内(≥old-5min)→接受
	w := s.Snapshot().Accounts["a1"].Windows["five_hour"]
	if w.UsedPct != 50 {
		t.Fatalf("容差内更新应接受：got %.1f", w.UsedPct)
	}
	s.ObserveWindowSource("a1", "five_hour", "api", 30, ts(200), ts(0)) // 远旧 until → 拒绝
	w = s.Snapshot().Accounts["a1"].Windows["five_hour"]
	if w.UsedPct != 50 {
		t.Fatalf("远旧观测应拒绝（防时间回退）：got %.1f", w.UsedPct)
	}
}

func TestObserveWindowEarlyResetAdoption(t *testing.T) {
	s := New("")
	s.EnsureAccount("a1")
	s.ObserveWindowSource("a1", "weekly", "429", 100, ts(5000), ts(0))
	// /usage 显示窗口已重置（新 until 远早于旧值）→ 立即采纳（允许提前恢复）
	s.ObserveWindowSource("a1", "weekly", "api", 5, ts(60), ts(10))
	w := s.Snapshot().Accounts["a1"].Windows["weekly"]
	if w.UsedPct != 5 || !w.Until.Equal(ts(60)) {
		t.Fatalf("提前收窗应立即采纳：got %+v", w)
	}
}

func TestWindowExhaustedAndRecovered(t *testing.T) {
	s := New("")
	s.EnsureAccount("a1")
	s.ObserveWindowSource("a1", "five_hour", "429", 100, ts(300), ts(0))
	if !s.WindowExhausted("a1", "five_hour", ts(100)) {
		t.Fatal("耗尽窗口在 until 前应判 exhausted")
	}
	if s.WindowExhausted("a1", "five_hour", ts(301)) {
		t.Fatal("until 之后应自动视为未耗尽（惰性恢复）")
	}
}

// —— 绑定表（TTL 滑动仅成功续命 + LRU）——

func TestBindingTTL(t *testing.T) {
	s := New("")
	s.Bind("sid-1", "a1", ts(0))
	if id, ok := s.LookupBinding("sid-1", ts(30)); !ok || id != "a1" {
		t.Fatalf("TTL 内应命中：got %q ok=%v", id, ok)
	}
	if _, ok := s.LookupBinding("sid-1", ts(61)); ok {
		t.Fatal("超过 1h 未成功续命应过期")
	}
}

func TestBindingOverwriteMigrates(t *testing.T) {
	s := New("")
	s.Bind("sid-1", "a1", ts(0))
	s.Bind("sid-1", "a2", ts(10)) // 换号成功 → 覆盖迁移
	if id, _ := s.LookupBinding("sid-1", ts(20)); id != "a2" {
		t.Fatalf("最近成功覆盖写：got %q", id)
	}
}

func TestBindingLRUCap(t *testing.T) {
	s := New("").WithBindingCap(3)
	s.Bind("s1", "a", ts(0))
	s.Bind("s2", "a", ts(1))
	s.Bind("s3", "a", ts(2))
	s.Bind("s4", "a", ts(3)) // 驱逐最旧 s1
	if _, ok := s.LookupBinding("s1", ts(4)); ok {
		t.Fatal("LRU 应驱逐最旧绑定")
	}
	if _, ok := s.LookupBinding("s4", ts(4)); !ok {
		t.Fatal("最新绑定应在")
	}
}

func TestPickHitDoesNotRenewBinding(t *testing.T) {
	s := New("")
	s.Bind("sid-1", "a1", ts(0))
	_, _ = s.LookupBinding("sid-1", ts(50)) // Pick 命中不续命
	if _, ok := s.LookupBinding("sid-1", ts(61)); ok {
		t.Fatal("Lookup（Pick 侧）不得刷新 last_seen")
	}
}

// —— inflight ——
func TestInflightClamp(t *testing.T) {
	s := New("")
	s.EnsureAccount("a1")
	s.InflightDec("a1") // 无配对的递减
	if n := s.Snapshot().Inflight["a1"]; n != 0 {
		t.Fatalf("下限钳 0：got %d", n)
	}
	s.InflightInc("a1")
	s.InflightInc("a1")
	if n := s.Snapshot().Inflight["a1"]; n != 2 {
		t.Fatalf("inc×2：got %d", n)
	}
	s.InflightDec("a1")
	s.InflightDec("a1")
	s.InflightDec("a1")
	if n := s.Snapshot().Inflight["a1"]; n != 0 {
		t.Fatalf("重复递减不得为负：got %d", n)
	}
}

// —— 账号标记 ——
func TestAccountFlagsAndLearnLimit(t *testing.T) {
	s := New("")
	s.EnsureAccount("a1")
	s.SetRequiresSignIn("a1", true)
	s.LearnLimit("a1", 6)
	snap := s.Snapshot()
	acc := snap.Accounts["a1"]
	if !acc.RequiresSignIn || acc.LearnedConcurrencyLimit != 6 {
		t.Fatalf("标记/学习上限：got %+v", acc)
	}
}

// —— 持久化往返 ——
func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.EnsureAccount("a1")
	s.ObserveWindowSource("a1", "five_hour", "api", 40, ts(300), ts(0))
	s.LearnLimit("a1", 6)
	s.Bind("sid-1", "a1", ts(0))
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	path := filepath.Join(dir, "state.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state.json 未生成: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := loaded.Snapshot().Accounts["a1"].Windows["five_hour"]
	if w.UsedPct != 40 || loaded.Snapshot().Accounts["a1"].LearnedConcurrencyLimit != 6 {
		t.Fatalf("往返丢失：got %+v", loaded.Snapshot().Accounts["a1"])
	}
	if id, ok := loaded.LookupBinding("sid-1", ts(10)); !ok || id != "a1" {
		t.Fatal("绑定往返丢失")
	}
	if g1, g2 := s.Snapshot().Generation, loaded.Snapshot().Generation; g1 != g2 {
		t.Fatalf("generation 往返：%d vs %d", g1, g2)
	}
	// inflight 不落盘（重启归零）
	if n := loaded.Snapshot().Inflight["a1"]; n != 0 {
		t.Fatalf("inflight 应纯内存：got %d", n)
	}
}

func TestReconcileDropsOrphanAccounts(t *testing.T) {
	s := New("")
	s.EnsureAccount("gone")
	s.EnsureAccount("kept")
	s.Reconcile(map[string]bool{"kept": true, "new1": true})
	snap := s.Snapshot()
	if _, exists := snap.Accounts["gone"]; exists {
		t.Fatal("宿主已删账号应清除记录")
	}
	if _, exists := snap.Accounts["new1"]; !exists {
		t.Fatal("宿主新账号应初始化记录")
	}
}
