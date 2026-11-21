package store

import (
	"path/filepath"
	"testing"
	"time"
)

// P1 修复验收：Load 必须重建 bindingLRU，否则重启载入满表绑定后
// 每次 Bind 都驱逐刚写入的条目（自驱逐），绑定永不命中。
func TestLoadRebuildsBindingLRU(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.bindingCap = 3
	now := time.Now()
	s.EnsureAccount("a1")
	s.EnsureAccount("a2")
	// LastSeen 递增：同时间戳会让 Load 的 LRU 重建顺序不确定（CI 实测踩过）
	s.Bind("s1", "a1", now)
	s.Bind("s2", "a1", now.Add(time.Second))
	s.Bind("s3", "a2", now.Add(2*time.Second))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	loaded.bindingCap = 3
	// 满表后再 Bind 一个新 sid：应驱逐最旧（s1），而不是自驱逐新条目。
	loaded.Bind("s-new", "a2", now.Add(time.Second))
	if got, ok := loaded.LookupBinding("s-new", now.Add(time.Second)); !ok || got != "a2" {
		t.Fatalf("新绑定不得被自驱逐：got %q ok=%v", got, ok)
	}
	if _, ok := loaded.LookupBinding("s2", now.Add(time.Second)); !ok {
		t.Fatal("非最旧绑定 s2 应保留")
	}
}

// P1 修复验收：Flush 必须有生产调用（自动持久化），否则重启丢全部
// 窗口/绑定/学习上限。StartAutoFlush 周期落盘，stop 时最终 Flush。
func TestAutoFlushPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	stop := s.StartAutoFlush(10 * time.Millisecond)
	now := time.Now()
	s.EnsureAccount("a1")
	s.ObserveWindowSource("a1", "rolling", "api", 42, now.Add(time.Hour), now)
	s.Bind("s1", "a1", now)
	time.Sleep(50 * time.Millisecond) // 等 ticker 至少落盘一次
	stop()                            // stop 触发最终 Flush

	loaded, err := Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := loaded.LookupBinding("s1", now); !ok || got != "a1" {
		t.Fatalf("自动持久化应保留绑定：got %q ok=%v", got, ok)
	}
	acc := loaded.Snapshot().Accounts["a1"]
	if acc == nil || acc.Windows["rolling"].UsedPct != 42 {
		t.Fatalf("自动持久化应保留窗口：%+v", acc)
	}
}

// P1 修复验收：PurgeAccount/ClearBindings 必须同步清 bindingLRU，
// 否则幽灵条目无界累积，满表后驱逐失效。
func TestPurgeClearsBindingLRU(t *testing.T) {
	s := New("")
	s.bindingCap = 3
	now := time.Now()
	s.EnsureAccount("a1")
	s.Bind("s1", "a1", now)
	s.Bind("s2", "a1", now)
	s.PurgeAccount("a1")
	if len(s.bindingLRU) != 0 {
		t.Fatalf("purge 应清空 LRU：got %v", s.bindingLRU)
	}
	s.Bind("s3", "a1", now)
	s.Bind("s4", "a1", now)
	s.Bind("s5", "a1", now)
	if _, ok := s.LookupBinding("s5", now); !ok {
		t.Fatal("purge 后 LRU 干净时新绑定应正常存活")
	}
}
