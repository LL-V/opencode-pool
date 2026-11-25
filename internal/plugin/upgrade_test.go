package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// 升级（重装插件二进制）必须保留已添加账号：宿主 auth 文件与插件
// state.json 都不在安装目标路径里；重新注册只读取，不重写。
func TestUpgradePreservesAccountsAndState(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	p1, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	call(t, p1, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	a := addAccount(t, p1, "a@x.com", "oc_sk_upgrade_aaaaaaaaaaaa")
	b := addAccount(t, p1, "b@x.com", "oc_sk_upgrade_bbbbbbbbbbbb")
	now := time.Now()
	p1.Store().ObserveWindowSource(a, "rolling", "api", 42, now.Add(2*time.Hour), now)
	p1.Store().Bind("sid-upgrade", a, now)
	p1.Store().LearnLimit(a, 7)
	savedBefore := len(host.saved)
	p1.Shutdown() // 升级前最终落盘

	// 新版本进程：同一数据目录 + 同一批宿主 auth 文件
	p2, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	call(t, p2, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	snap := p2.Store().Snapshot()
	for _, id := range []string{a, b} {
		if _, ok := snap.Accounts[id]; !ok {
			t.Fatalf("升级后账号丢失：%s（accounts=%v）", id, keysOf(snap.Accounts))
		}
	}
	if w := snap.Accounts[a].Windows["rolling"]; w == nil || w.UsedPct != 42 {
		t.Fatalf("升级后窗口状态丢失：%+v", w)
	}
	if got, ok := snap.Bindings["sid-upgrade"]; !ok || got != a {
		t.Fatalf("升级后会话绑定丢失：%v", snap.Bindings)
	}
	if snap.Accounts[a].LearnedConcurrencyLimit != 7 {
		t.Fatalf("升级后学习上限丢失：%d", snap.Accounts[a].LearnedConcurrencyLimit)
	}
	if len(host.saved) != savedBefore {
		t.Fatalf("注册不得重写宿主 auth 文件：before=%d after=%d", savedBefore, len(host.saved))
	}

	// 面板能看到两个账号，标签仍是邮箱
	resp := p2.HandleManagement("GET", "/accounts", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET accounts: %d %s", resp.StatusCode, resp.Body)
	}
	var list struct {
		Accounts []accountView `json:"accounts"`
	}
	_ = json.Unmarshal(resp.Body, &list)
	if len(list.Accounts) != 2 {
		t.Fatalf("面板账号数：%d %s", len(list.Accounts), resp.Body)
	}
	for _, v := range list.Accounts {
		if !strings.HasSuffix(v.Label, "@x.com") {
			t.Fatalf("升级后账户名丢失：%+v", v)
		}
	}
}

func keysOf(m map[string]*storeAccount) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
