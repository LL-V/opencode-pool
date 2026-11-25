package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 同一 key 重新添加：保留宿主 auth 文件里的 base_url/priority 等字段，
// 并清除此前的登录/待验证标记。
func TestReaddPreservesHostAuthFieldsAndClearsFlags(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	key := "oc_sk_merge_0000000000000000000000"
	sum := sha256.Sum256([]byte(key))
	id := ProviderID + ":" + hex.EncodeToString(sum[:])[:12]
	seed := []byte(`{"type":"opencode-go","api_key":"` + key + `","name":"old","label":"old","base_url":"https://gw.example/v1","priority":"9"}`)
	host.byID = map[string]*fakeAuth{id: {ID: id, Index: "idx-merge", Name: "old.json", Provider: ProviderID, Raw: seed}}

	p.Store().EnsureAccount(id)
	p.Store().SetRequiresSignIn(id, true)
	p.Store().SetVerifyPending(id, true)
	p.Store().SetEnabled(id, false)

	addAccount(t, p, "new@x.com", key)

	if len(host.saved) == 0 {
		t.Fatal("应调用 host.auth.save")
	}
	var saved map[string]any
	if err := json.Unmarshal(host.saved[len(host.saved)-1]["json"].(json.RawMessage), &saved); err != nil {
		t.Fatalf("saved json: %v", err)
	}
	if saved["base_url"] != "https://gw.example/v1" || saved["priority"] != "9" {
		t.Fatalf("重新添加不得丢失宿主字段：%v", saved)
	}
	if saved["name"] != "new@x.com" || saved["label"] != "new@x.com" {
		t.Fatalf("名称应更新：%v", saved)
	}
	acc := p.Store().Snapshot().Accounts[id]
	if acc == nil || acc.RequiresSignIn || acc.VerifyPending || acc.Disabled {
		t.Fatalf("重新添加应清除登录/待验证/停用标记：%+v", acc)
	}
}

// 宿主列表不认识的非本池 ID 只能移除，不能留下永久墓碑。
func TestUnknownAccountDroppedWithoutTombstone(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	addAccount(t, p, "real@x.com", "oc_sk_real_0000000000000000000000")
	p.Store().EnsureAccount("opencode-history.json")
	p.Store().Bind("sid-weird", "opencode-history.json", time.Now())

	resp := p.HandleManagement("GET", "/accounts", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("accounts: %d %s", resp.StatusCode, resp.Body)
	}
	if _, ok := p.Store().Snapshot().Accounts["opencode-history.json"]; ok {
		t.Fatal("宿主不认识的记录应被移除")
	}
	if p.Store().IsPurged("opencode-history.json") {
		t.Fatal("不得留下永久墓碑")
	}
}

// unblock 必须如实报告宿主冷却清理失败（此处：宿主没有该凭据）。
func TestUnblockReportsMissingHostCredential(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := "opencode-go:ghost"
	p.Store().EnsureAccount(id)
	resp := p.HandleManagement("POST", "/account/unblock", map[string][]string{"id": {id}}, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("宿主缺凭据应 409：%d %s", resp.StatusCode, resp.Body)
	}
}

// 非本池宿主凭据不得被 PATCH 创建本地记录。
func TestPatchForeignHostAccountRejected(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	host.byID = map[string]*fakeAuth{
		"claude:apikey:zzz": {ID: "claude:apikey:zzz", Index: "idx-cl", Name: "claude.json", Provider: "claude"},
	}
	body := `{"id":"claude:apikey:zzz","enabled":false}`
	resp := p.HandleManagement("PATCH", "/account", nil, strings.NewReader(body))
	if resp.StatusCode != 404 {
		t.Fatalf("外池账号应 404：%d %s", resp.StatusCode, resp.Body)
	}
}

// 面板状态与调度必须共用 near_limit_threshold_pct。
func TestAccountStatusUsesConfiguredNearThreshold(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	if _, err := p.updateSettings([]byte(`{"near_limit_threshold_pct":20}`)); err != nil {
		t.Fatal(err)
	}
	id := addAccount(t, p, "near@x.com", "oc_sk_near_0000000000000000000000")
	p.Store().ObserveWindowSource(id, "rolling", "api", 85, time.Now().Add(time.Hour), time.Now())

	resp := p.HandleManagement("GET", "/accounts", nil, nil)
	var listed struct {
		Accounts []accountView `json:"accounts"`
	}
	_ = json.Unmarshal(resp.Body, &listed)
	if len(listed.Accounts) != 1 || listed.Accounts[0].Status != "near_limit" {
		t.Fatalf("85%% 在 20%% 阈值下应为 near_limit：%+v", listed.Accounts)
	}
	if _, err := p.updateSettings([]byte(`{"near_limit_threshold_pct":8}`)); err != nil {
		t.Fatal(err)
	}
	resp = p.HandleManagement("GET", "/accounts", nil, nil)
	_ = json.Unmarshal(resp.Body, &listed)
	if listed.Accounts[0].Status != "active" {
		t.Fatalf("85%% 在 8%% 阈值下应仍 active：%+v", listed.Accounts[0])
	}
}

// 已删除账号被宿主重放执行时不得复活 store 记录。
func TestExecutePurgedDoesNotResurrect(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := "opencode-go:purged"
	p.Store().EnsureAccount(id)
	p.Store().PurgeAccount(id)

	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: id, AuthProvider: ProviderID, Format: "openai",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-p"}`),
		Payload:     []byte(`{"model":"x"}`), Headers: map[string][]string{},
	})
	out, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if env.OK {
		t.Fatalf("已删除账号不应执行：%s", out)
	}
	if _, ok := p.Store().Snapshot().Accounts[id]; ok {
		t.Fatal("已删除账号不得被重建")
	}
}

// 导入超限必须在写 settings 之前被拒绝。
func TestImportOverLimitDoesNotPersistSettings(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	accts := make([]map[string]string, 0, maxImportAccounts+1)
	for i := 0; i <= maxImportAccounts; i++ {
		accts = append(accts, map[string]string{"name": fmt.Sprintf("a%d", i), "api_key": fmt.Sprintf("k%d", i)})
	}
	body, _ := json.Marshal(map[string]any{
		"settings": map[string]any{"panel_poll_seconds": 9},
		"accounts": accts,
	})
	resp := p.HandleManagement("POST", "/import", nil, strings.NewReader(string(body)))
	if resp.StatusCode != 400 {
		t.Fatalf("超限应 400：%d", resp.StatusCode)
	}
	if p.config().PanelPollSeconds != 5 {
		t.Fatalf("超限导入不得先写 settings：got %d", p.config().PanelPollSeconds)
	}
}

// 并发 PUT /settings 不得写出损坏文件或丢最后状态。
func TestConcurrentSettingsPersist(t *testing.T) {
	host := &fakeHost{}
	dir := t.TempDir()
	p, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := p.HandleManagement("PUT", "/settings", nil,
				strings.NewReader(fmt.Sprintf(`{"upstream_user_agent":"ua-%d"}`, i)))
			if resp.StatusCode != 200 {
				t.Errorf("PUT settings %d: %d %s", i, resp.StatusCode, resp.Body)
			}
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(filepath.Join(dir, settingsFileName))
	if err != nil {
		t.Fatalf("settings.json 必须存在：%v", err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json 损坏：%v %s", err, raw)
	}
	if !strings.HasPrefix(got.UpstreamUserAgent, "ua-") {
		t.Fatalf("最终设置异常：%q", got.UpstreamUserAgent)
	}
}
