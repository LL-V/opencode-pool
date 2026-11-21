package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 过期窗口被低权威 429 延长后，Source 必须改成真正确立该截止时间的来源；
// 否则后续 /usage 的更早 reset 会被当成时钟漂移拒绝，账号被多封数小时。
func TestExpiredWindowExtensionTakesNewAuthority(t *testing.T) {
	s := New("")
	now := time.Now()
	past := now.Add(-time.Minute)
	s.EnsureAccount("a")
	s.ObserveWindowSource("a", "weekly", "api", 100, past, now)
	guess := now.Add(24 * time.Hour)
	s.ObserveWindowSource("a", "weekly", "429", 100, guess, now)

	snap := s.Snapshot()
	w := snap.Accounts["a"].Windows["weekly"]
	if !w.Until.Equal(guess) {
		t.Fatalf("429 应延长已过期窗口：until=%v", w.Until)
	}
	if w.Source != "429" {
		t.Fatalf("延长截止时间的来源必须记录为 429，got %q", w.Source)
	}
	// /usage 知道真实更早的 reset：必须被采纳，而不是当作漂移拒绝。
	real := now.Add(time.Hour)
	s.ObserveWindowSource("a", "weekly", "api", 100, real, now.Add(time.Second))
	w = s.Snapshot().Accounts["a"].Windows["weekly"]
	if !w.Until.Equal(real) || w.Source != "api" {
		t.Fatalf("更高权威的更早 reset 应被采纳：until=%v source=%q", w.Until, w.Source)
	}
}

// 高权威未过期截止时间不能被低权威延长（既有保护不得回退）。
func TestLiveAuthoritativeUntilNotExtendedByLowerSource(t *testing.T) {
	s := New("")
	now := time.Now()
	live := now.Add(time.Hour)
	s.EnsureAccount("a")
	s.ObserveWindowSource("a", "weekly", "api", 100, live, now)
	s.ObserveWindowSource("a", "weekly", "429", 100, now.Add(5*time.Hour), now)
	w := s.Snapshot().Accounts["a"].Windows["weekly"]
	if !w.Until.Equal(live) || w.Source != "api" {
		t.Fatalf("未过期的 api 截止不应被 429 延长：until=%v source=%q", w.Until, w.Source)
	}
}

// Reconcile 删除账号后，指向它的绑定与 inflight 必须一并清理。
func TestReconcileDropsOrphanBindings(t *testing.T) {
	s := New("")
	now := time.Now()
	s.EnsureAccount("gone")
	s.EnsureAccount("kept")
	s.Bind("s-gone", "gone", now)
	s.Bind("s-kept", "kept", now)
	s.InflightInc("gone")
	s.Reconcile(map[string]bool{"kept": true})
	snap := s.Snapshot()
	if _, ok := snap.Bindings["s-gone"]; ok {
		t.Fatal("已删账号的绑定应清理")
	}
	if _, ok := snap.Bindings["s-kept"]; !ok {
		t.Fatal("存活账号的绑定应保留")
	}
	if snap.Inflight["gone"] != 0 {
		t.Fatalf("已删账号 inflight 应清理：%v", snap.Inflight)
	}
}

// 畸形/旧版 state.json：null 账号、null windows、null binding 不得 panic。
func TestLoadHardensMalformedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw := `{"version":2,"accounts":{"a":null,"b":{"windows":null},"c":{"windows":{"weekly":null}}},"bindings":{"s":null}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if _, ok := snap.Accounts["a"]; ok {
		t.Fatal("null 账号应被丢弃")
	}
	if acc := snap.Accounts["b"]; acc == nil || acc.Windows == nil {
		t.Fatalf("windows 应规范化为空 map：%+v", acc)
	}
	if _, ok := snap.Bindings["s"]; ok {
		t.Fatal("null 绑定应被丢弃")
	}
	// 之后的写路径不得因 nil 解引用崩溃
	s.ObserveWindow("b", "rolling", 10, time.Now().Add(time.Hour), time.Now())
	if _, ok := s.LookupBinding("s", time.Now()); ok {
		t.Fatal("null 绑定不应命中")
	}
}

// StartAutoFlush(0) 不得 panic（time.NewTicker 要求正数）。
func TestStartAutoFlushNonPositiveInterval(t *testing.T) {
	s := New("")
	stop := s.StartAutoFlush(0)
	stop() // 不 panic 即通过
}

// 绑定计数不得把过期绑定算进去。
func TestBindingCountsIgnoreExpired(t *testing.T) {
	s := New("")
	now := time.Now()
	s.EnsureAccount("a")
	s.Bind("fresh", "a", now)
	s.Bind("stale", "a", now.Add(-2*BindingTTL))
	counts := s.BindingCounts(now)
	if counts["a"] != 1 {
		t.Fatalf("过期绑定不应计数：%v", counts)
	}
}
