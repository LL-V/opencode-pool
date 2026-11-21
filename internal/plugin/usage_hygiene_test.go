package plugin

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usage.handle 是宿主全局事件源：其他 provider（claude/gemini…）的账目
// 绝不能把凭据写进插件 store，否则面板会凭空出现 apik…4f95 这类账号
// （用户实测：删除后又自动出现、token 还一直涨）。
func TestUsageIgnoresForeignProvider(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		Provider: "claude", AuthID: "claude:apikey:672b3a714f95",
		Detail: pluginapi.UsageDetail{InputTokens: 2200000, OutputTokens: 2900},
	})
	if _, ok := p.Store().Snapshot().Accounts["claude:apikey:672b3a714f95"]; ok {
		t.Fatal("其他 provider 的 usage 不得创建插件账号")
	}
}

// 已删除（purge）的账号：宿主的 usage 事件仍可能带它（host auth 文件还在），
// 不得把它复活；也不能继续累计用量。
func TestUsageIgnoresPurgedAccount(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:dead")
	p.Store().PurgeAccount("opencode-go:dead")
	call(t, p, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		Provider: ProviderID, AuthID: "opencode-go:dead",
		Detail: pluginapi.UsageDetail{InputTokens: 10},
	})
	if _, ok := p.Store().Snapshot().Accounts["opencode-go:dead"]; ok {
		t.Fatal("已删除账号不得被 usage 事件复活")
	}
}

// GET /accounts 必须清理并隐藏误入的宿主原生账号（history 中已写入 state.json
// 的幽灵条目，升级后无需重启也应消失）。
func TestManagementHidesForeignAccounts(t *testing.T) {
	host := &fakeHost{byID: map[string]map[string]any{
		"claude:apikey:672b3a714f95": {
			"ID": "claude:apikey:672b3a714f95", "Provider": "claude",
			"StorageJSON": []byte(`{"type":"claude","api_key":"sk-ant-demo"}`),
		},
	}}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("claude:apikey:672b3a714f95")
	resp := p.HandleManagement("GET", "/accounts", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET accounts: %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "claude:apikey") {
		t.Fatalf("面板不得显示其他 provider 的账号：%s", resp.Body)
	}
	if _, ok := p.Store().Snapshot().Accounts["claude:apikey:672b3a714f95"]; ok {
		t.Fatal("误入的外来账号应被清理")
	}
}

// 本插件账号（即使没有名字）不得被清理规则误伤。
func TestManagementKeepsUnnamedOwnAccount(t *testing.T) {
	host := &fakeHost{byID: map[string]map[string]any{
		"opencode-go:abcdef123456": {
			"ID": "opencode-go:abcdef123456", "Provider": ProviderID,
			"StorageJSON": []byte(`{"type":"opencode-go","api_key":"oc_sk_x"}`),
		},
	}}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:abcdef123456")
	resp := p.HandleManagement("GET", "/accounts", nil, nil)
	if !strings.Contains(string(resp.Body), "opencode-go:abcdef123456") {
		t.Fatalf("本插件账号不得被清理：%s", resp.Body)
	}
}

// 宿主会按自己的节奏反复调 quota.fetch；上游 /usage 探测必须被节流，
// 否则就是用户看到的"一直探测"。
func TestQuotaFetchIsThrottled(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:aaa")
	probes := func() int {
		n := 0
		for _, c := range host.recorded() {
			if c == pluginabi.MethodHostHTTPDo {
				n++
			}
		}
		return n
	}
	before := probes()
	for i := 0; i < 5; i++ {
		p.HandleManagement("GET", "/account", map[string][]string{"id": {"opencode-go:aaa"}}, nil)
		for j := 0; j < 3; j++ {
			call(t, p, pluginabi.MethodQuotaFetch, pluginapi.QuotaFetchRequest{AuthID: "opencode-go:aaa"})
		}
	}
	after := probes()
	if after-before > 1 { // 15 次面板/quota 调用最多发一次上游 /usage
		t.Fatalf("5 分钟内 /usage 探测应被节流：上游探测 %d 次", after-before)
	}
}

// 删除后重新用同一 key 添加：墓碑必须被清除，账号恢复可见。
func TestReaddClearsPurgeTombstone(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := addAccount(t, p, "a@x.com", "oc_sk_readd_aaaaaaaaaaaaaaaa")
	p.Store().PurgeAccount(id)
	if !p.Store().IsPurged(id) {
		t.Fatal("删除应留下墓碑")
	}
	id2 := addAccount(t, p, "a@x.com", "oc_sk_readd_aaaaaaaaaaaaaaaa")
	if id2 != id {
		t.Fatalf("同一 key 应产生同一 id：%s vs %s", id, id2)
	}
	if p.Store().IsPurged(id) {
		t.Fatal("重新添加应清除墓碑")
	}
	if _, ok := p.Store().Snapshot().Accounts[id]; !ok {
		t.Fatal("重新添加后账号应回到 store")
	}
}
