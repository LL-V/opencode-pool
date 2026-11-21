package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 删除必须是粘性的：宿主 auth 文件仍在（ABI 无删除回调），每次 register
// 的 Reconcile 都会把它当成 live。没有墓碑就会"删了又回来"。
func TestPurgeTombstoneBlocksReconcile(t *testing.T) {
	s := New("")
	s.EnsureAccount("opencode-go:dead")
	s.PurgeAccount("opencode-go:dead")
	if !s.IsPurged("opencode-go:dead") {
		t.Fatal("purge 应留下墓碑")
	}
	s.Reconcile(map[string]bool{"opencode-go:dead": true})
	if _, ok := s.Snapshot().Accounts["opencode-go:dead"]; ok {
		t.Fatal("Reconcile 不得复活已删除账号")
	}
	s.ClearPurge("opencode-go:dead")
	s.Reconcile(map[string]bool{"opencode-go:dead": true})
	if _, ok := s.Snapshot().Accounts["opencode-go:dead"]; !ok {
		t.Fatal("清除墓碑后应正常恢复")
	}
}

// 墓碑必须落盘：重启（Load）后仍然阻止复活。
func TestPurgeTombstonePersists(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.EnsureAccount("opencode-go:dead")
	s.PurgeAccount("opencode-go:dead")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.IsPurged("opencode-go:dead") {
		t.Fatal("重启后墓碑应保留")
	}
	loaded.Reconcile(map[string]bool{"opencode-go:dead": true})
	if _, ok := loaded.Snapshot().Accounts["opencode-go:dead"]; ok {
		t.Fatal("重启后 Reconcile 不得复活")
	}
	_ = time.Now
}
