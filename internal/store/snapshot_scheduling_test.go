package store

import (
	"testing"
	"time"
)

func TestSnapshotSchedulingSkipsBindings(t *testing.T) {
	s := New("")
	s.EnsureAccount("opencode-go:a")
	s.Bind("sid-1", "opencode-go:a", time.Now())
	s.InflightInc("opencode-go:a")

	full := s.Snapshot()
	if full.Bindings["sid-1"] != "opencode-go:a" {
		t.Fatalf("完整快照应含绑定：%+v", full.Bindings)
	}
	sched := s.SnapshotScheduling()
	if sched.Bindings != nil {
		t.Fatalf("调度快照不应复制绑定：%+v", sched.Bindings)
	}
	if sched.Accounts["opencode-go:a"] == nil || sched.Inflight["opencode-go:a"] != 1 {
		t.Fatalf("调度快照必须包含账号与 inflight：%+v", sched)
	}
	// 快照与后续写入隔离。
	s.InflightInc("opencode-go:a")
	if sched.Inflight["opencode-go:a"] != 1 {
		t.Fatal("调度快照必须与后续写入隔离")
	}
}
