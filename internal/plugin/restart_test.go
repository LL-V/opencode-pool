package plugin

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// 重启恢复：register 载入 state.json（窗口/绑定/学习上限），
// shutdown 最终 Flush；进程重启后状态完整（deep-check P1：Flush 此前零调用）。
func TestPluginRestartRestoresState(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	p1, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	call(t, p1, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	now := time.Now()
	p1.Store().EnsureAccount("opencode-go:aaa")
	p1.Store().ObserveWindowSource("opencode-go:aaa", "rolling", "api", 55, now.Add(time.Hour), now)
	p1.Store().Bind("sid-r", "opencode-go:aaa", now)
	if _, err := p1.HandleMethod(pluginabi.MethodPluginShutdown, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := filepath.Abs(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	p2, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	call(t, p2, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	if got, ok := p2.Store().LookupBinding("sid-r", now); !ok || got != "opencode-go:aaa" {
		t.Fatalf("重启应恢复绑定：got %q ok=%v", got, ok)
	}
	acc := p2.Store().Snapshot().Accounts["opencode-go:aaa"]
	if acc == nil || acc.Windows["rolling"].UsedPct != 55 {
		t.Fatalf("重启应恢复窗口：%+v", acc)
	}
}
