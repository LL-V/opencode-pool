package store

import (
	"testing"
	"time"
)

// 24h 窗口轮转模拟：任一窗口耗尽即暂停（上游任一窗口满即 429），
// 按该窗口 reset 惰性恢复；多窗口并行互不干扰。
func TestWindowLifecycleAnyExhaustedBlocksAndRecovers(t *testing.T) {
	s := New("")
	id := "opencode-go:t1"
	s.EnsureAccount(id)
	now := time.Now()

	// rolling 满（+5h），weekly 健康：账号必须立刻视为耗尽（bad）。
	s.ObserveWindowSource(id, "rolling", "429", 100, now.Add(5*time.Hour), now)
	s.ObserveWindowSource(id, "weekly", "api", 30, now.Add(7*24*time.Hour), now)
	if !s.AnyWindowExhausted(id, now) {
		t.Fatal("任一窗口耗尽即应暂停（上游任一窗口满即 429），全耗尽语义会导致持续撞墙")
	}
	if s.AllWindowsExhausted(id, now) {
		t.Fatal("weekly 健康时全耗尽应为 false")
	}

	// rolling 到期未刷新：惰性过期 → 恢复可用。
	if s.AnyWindowExhausted(id, now.Add(5*time.Hour+time.Minute)) {
		t.Fatal("rolling 过期后应恢复可用（惰性判过期）")
	}

	// weekly 也耗尽：全耗尽；到最早 reset（rolling）恢复。
	s.ObserveWindowSource(id, "weekly", "429", 100, now.Add(7*24*time.Hour), now.Add(time.Second))
	if !s.AllWindowsExhausted(id, now.Add(2*time.Second)) {
		t.Fatal("两窗口都耗尽应为全耗尽")
	}
	if !s.AnyWindowExhausted(id, now.Add(2*time.Second)) {
		t.Fatal("两窗口都耗尽时任一语义也应为 true")
	}
}

// 时间推进模拟：限额→reset→api 刷新 0%→完全恢复（window 轮转一周）。
func TestWindowLifecycleFullCycle(t *testing.T) {
	s := New("")
	id := "opencode-go:t2"
	s.EnsureAccount(id)
	base := time.Now()

	// T0: 429 被动标记 rolling 100%（reset +5h）
	s.ObserveWindowSource(id, "rolling", "429", 100, base.Add(5*time.Hour), base)
	// T0+1h: 仍耗尽
	if !s.AnyWindowExhausted(id, base.Add(time.Hour)) {
		t.Fatal("T0+1h 应耗尽")
	}
	// T0+5h: api 权威刷新到新周期 0%（reset 再 +5h）
	s.ObserveWindowSource(id, "rolling", "api", 0, base.Add(10*time.Hour), base.Add(5*time.Hour))
	if s.AnyWindowExhausted(id, base.Add(5*time.Hour+time.Minute)) {
		t.Fatal("api 刷新 0% 后应恢复")
	}
	// T0+8h: 用量再次到 100%（新周期限额）
	s.ObserveWindowSource(id, "rolling", "api", 100, base.Add(10*time.Hour), base.Add(8*time.Hour))
	if !s.AnyWindowExhausted(id, base.Add(8*time.Hour)) {
		t.Fatal("新周期再次满时应再次暂停")
	}
	// T0+10h+1m: 第二周期过期恢复
	if s.AnyWindowExhausted(id, base.Add(10*time.Hour+time.Minute)) {
		t.Fatal("第二周期过期后应恢复")
	}
}
