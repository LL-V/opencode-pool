package plugin

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// M8 管理端点：GET/PUT settings（与 ConfigFields 同一 schema）、
// GET export / POST import、DELETE ?purge=true 硬删插件侧状态。

func TestSettingsGetPut(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)

	resp := p.HandleManagement("GET", "/settings", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET settings: %d", resp.StatusCode)
	}
	var cfg Config
	if err := json.Unmarshal(resp.Body, &cfg); err != nil {
		t.Fatalf("settings 应为 Config schema：%s", resp.Body)
	}
	if cfg.UpstreamUserAgent != DefaultUserAgent {
		t.Fatalf("默认 UA：got %q", cfg.UpstreamUserAgent)
	}

	resp = p.HandleManagement("PUT", "/settings", nil, strings.NewReader(`{"upstream_user_agent":"agent-x/1","panel_poll_seconds":9}`))
	if resp.StatusCode != 200 {
		t.Fatalf("PUT settings: %d %s", resp.StatusCode, resp.Body)
	}
	if got := p.config().UpstreamUserAgent; got != "agent-x/1" {
		t.Fatalf("PUT 应即时生效：got %q", got)
	}
	resp = p.HandleManagement("GET", "/settings", nil, nil)
	_ = json.Unmarshal(resp.Body, &cfg)
	if cfg.UpstreamUserAgent != "agent-x/1" || cfg.PanelPollSeconds != 9 {
		t.Fatalf("GET 应反映 PUT：%s", resp.Body)
	}
	// 未提供的字段保持原值
	if cfg.NearLimitThresholdPct != 8 {
		t.Fatalf("部分更新不应清空其他字段：%s", resp.Body)
	}
}

func TestSettingsPersistAcrossRebuild(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	p, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	resp := p.HandleManagement("PUT", "/settings", nil, strings.NewReader(`{"upstream_user_agent":"agent-y/2"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("PUT settings: %d", resp.StatusCode)
	}
	p2, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p2.config().UpstreamUserAgent; got != "agent-y/2" {
		t.Fatalf("settings 应持久化到数据目录：got %q", got)
	}
}

func addAccount(t *testing.T, p *Plugin, name, key string) string {
	t.Helper()
	body := `{"name":"` + name + `","api_key":"` + key + `"}`
	resp := p.HandleManagement("POST", "/accounts", nil, strings.NewReader(body))
	if resp.StatusCode != 200 {
		t.Fatalf("add account %s: %d %s", name, resp.StatusCode, resp.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp.Body, &created)
	return created.ID
}

func TestExportImport(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	addAccount(t, p, "a@x.com", "oc_sk_key_aaaaaaaaaaaaaaaaaa")
	addAccount(t, p, "b@x.com", "oc_sk_key_bbbbbbbbbbbbbbbbbb")

	resp := p.HandleManagement("GET", "/export", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET export: %d", resp.StatusCode)
	}
	var dump struct {
		Settings Config `json:"settings"`
		Accounts []struct {
			Name   string `json:"name"`
			APIKey string `json:"api_key"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &dump); err != nil {
		t.Fatalf("export 结构：%s", resp.Body)
	}
	if len(dump.Accounts) != 2 {
		t.Fatalf("export 应含全部账号：got %d", len(dump.Accounts))
	}
	for _, a := range dump.Accounts {
		if a.APIKey == "" {
			t.Fatalf("export 需要明文 key 才能恢复：%s", resp.Body)
		}
	}

	// import 新账号 + 重复幂等
	resp = p.HandleManagement("POST", "/import", nil, strings.NewReader(
		`{"accounts":[{"name":"c@x.com","api_key":"oc_sk_key_cccccccccccccccccc"},{"name":"a@x.com","api_key":"oc_sk_key_aaaaaaaaaaaaaaaaaa"}]}`))
	if resp.StatusCode != 200 {
		t.Fatalf("POST import: %d %s", resp.StatusCode, resp.Body)
	}
	var listed struct {
		Accounts []accountView `json:"accounts"`
	}
	resp = p.HandleManagement("GET", "/accounts", nil, nil)
	_ = json.Unmarshal(resp.Body, &listed)
	if len(listed.Accounts) != 3 {
		t.Fatalf("import 后应 3 账号（同 key 幂等）：got %d", len(listed.Accounts))
	}
}

func TestDeletePurge(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := addAccount(t, p, "a@x.com", "oc_sk_key_aaaaaaaaaaaaaaaaaa")
	p.Store().AddUsage(id, 10, 20, time.Now())

	// 无 purge：软删（禁用，数据保留）
	resp := p.HandleManagement("DELETE", "/account", url.Values{"id": []string{id}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	acc := p.Store().Snapshot().Accounts[id]
	if acc == nil || acc.Disabled != true || acc.Usage.Requests != 1 {
		t.Fatalf("软删应仅禁用、保留数据：%+v", acc)
	}

	// purge=true：清空插件侧全部状态
	resp = p.HandleManagement("DELETE", "/account", url.Values{"id": []string{id}, "purge": []string{"true"}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE purge: %d %s", resp.StatusCode, resp.Body)
	}
	if _, still := p.Store().Snapshot().Accounts[id]; still {
		t.Fatal("purge 应移除插件侧账号状态")
	}
}
