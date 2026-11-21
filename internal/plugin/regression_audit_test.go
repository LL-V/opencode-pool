package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 未分类非 2xx 绝不能当 200 透传（曾因 switch 里提前 return nil 变成死代码）。
func TestUnclassifiedNon2xxReturnsError(t *testing.T) {
	host := &fakeHost{upstreams: []fakeUpstream{{status: 404, body: `{"error":{"message":"model not found"}}`}}}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:aaa")
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "opencode-go:aaa", AuthProvider: ProviderID, Format: "openai",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
		Payload:     []byte(`{"model":"opencode-go/x","messages":[]}`),
	})
	out, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if env.OK || env.Error == nil {
		t.Fatalf("404 必须返回错误信封，不得伪装成功：%s", out)
	}
	if env.Error.Code != "request_scoped" || env.Error.HTTPStatus != 404 {
		t.Fatalf("404 应用 request_scoped 避免宿主 12h 冷却：%+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "model not found") {
		t.Fatalf("应保留上游原因：%q", env.Error.Message)
	}
}

// count_tokens 必须本地返回 input_tokens（宿主不会对 501 回退估算）。
func TestCountTokensEstimatesLocally(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "opencode-go:aaa", AuthProvider: ProviderID, Format: "claude",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
		Payload:     []byte(`{"messages":[{"role":"user","content":"count me please"}]}`),
	})
	out, err := p.HandleMethod(pluginabi.MethodExecutorCountTokens, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if !env.OK {
		t.Fatalf("count_tokens 应成功：%s", out)
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("响应解析：%v", err)
	}
	var body struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(resp.Payload, &body); err != nil || body.InputTokens < 1 {
		t.Fatalf("应返回 input_tokens：%s", resp.Payload)
	}
}

// verify 探针不得写入会话绑定；重复 RequestID 不得重复记账。
func TestUsageIgnoresProbeBindingAndDedupesRequestID(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:aaa")
	// 探针：不计用量、不写绑定
	call(t, p, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		RequestID: "verify-1", Provider: ProviderID, AuthID: "opencode-go:aaa",
		SessionID: "verify-opencode-go:aaa-123", Failed: false,
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 2},
	})
	// 真实请求：只记一次（宿主重放同一 RequestID）
	rec := pluginapi.UsageRecord{
		RequestID: "req-1", Provider: ProviderID, AuthID: "opencode-go:aaa",
		SessionID: "sid-real", Failed: false,
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 2},
	}
	call(t, p, pluginabi.MethodUsageHandle, rec)
	call(t, p, pluginabi.MethodUsageHandle, rec)
	snap := p.Store().Snapshot()
	if _, ok := snap.Bindings["verify-opencode-go:aaa-123"]; ok {
		t.Fatal("verify 探针不得写绑定")
	}
	if got := snap.Accounts["opencode-go:aaa"].Usage.Requests; got != 1 {
		t.Fatalf("探针不计、重放幂等：requests=%d", got)
	}
}

// 成功请求必须清除疑态；401 必须标记需重新登录。
func TestAuthStateTransitions(t *testing.T) {
	host := &fakeHost{upstreams: []fakeUpstream{{status: 200, body: `{"ok":true}`}}}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:aaa")
	p.Store().SetVerifyPending("opencode-go:aaa", true)
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "opencode-go:aaa", AuthProvider: ProviderID, Format: "openai",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
		Payload:     []byte(`{"model":"x","messages":[]}`),
	})
	if _, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw); err != nil {
		t.Fatal(err)
	}
	if p.Store().Snapshot().Accounts["opencode-go:aaa"].VerifyPending {
		t.Fatal("成功应清除 verify_pending")
	}

	host2 := &fakeHost{upstreams: []fakeUpstream{{status: 401, body: `{"error":"invalid api key"}`}}}
	p2 := newTestPlugin(t, host2)
	p2.Store().EnsureAccount("opencode-go:bbb")
	raw2, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "opencode-go:bbb", AuthProvider: ProviderID, Format: "openai",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"bad"}`),
		Payload:     []byte(`{"model":"x","messages":[]}`),
	})
	if _, err := p2.HandleMethod(pluginabi.MethodExecutorExecute, raw2); err != nil {
		t.Fatal(err)
	}
	if !p2.Store().Snapshot().Accounts["opencode-go:bbb"].RequiresSignIn {
		t.Fatal("401 应标记 requires_sign_in")
	}
}

// 面板设置必须在 register 的 config_yaml 覆盖后仍然生效。
func TestSettingsSurviveRegister(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	p, err := New(host, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	resp := p.HandleManagement("PUT", "/settings", nil, strings.NewReader(`{"upstream_user_agent":"panel-ua/9"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("PUT settings: %d %s", resp.StatusCode, resp.Body)
	}
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("upstream_user_agent: host-ua/1\n")})
	if got := p.config().UpstreamUserAgent; got != "panel-ua/9" {
		t.Fatalf("面板设置应覆盖 register 配置：got %q", got)
	}
}

// 宿主启动时 register 早于 SetAuthManager：auth.list 磁盘回退条目没有 ID。
// 此时绝不能执行对账，否则持久化状态会被清空（真实启动路径）。
type emptyIDAuthHost struct{ fakeHost }

func (h *emptyIDAuthHost) CallHost(method string, payload []byte) ([]byte, error) {
	if method == pluginabi.MethodHostAuthList {
		raw, _ := json.Marshal(map[string]any{"files": []map[string]any{
			{"provider": ProviderID, "name": "opencode-existing.json"},
		}})
		return wrapOK(raw), nil
	}
	return h.fakeHost.CallHost(method, payload)
}

func TestRegisterDoesNotWipeStateOnDiskFallbackAuthList(t *testing.T) {
	host := &emptyIDAuthHost{}
	p, err := New(host, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Store().EnsureAccount("opencode-go:keep")
	p.Store().ObserveWindowSource("opencode-go:keep", "weekly", "api", 42, time.Now().Add(time.Hour), time.Now())
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	snap := p.Store().Snapshot()
	acc := snap.Accounts["opencode-go:keep"]
	if acc == nil || acc.Windows["weekly"] == nil {
		t.Fatalf("磁盘回退 auth.list 不得清空持久状态：%+v", snap.Accounts)
	}
	if _, ok := snap.Accounts[""]; ok {
		t.Fatal("空 ID 不得成为账号")
	}
}

// 上游端点必须由请求体格式（SourceFormat）决定，而不是响应格式（Format）。
func TestExecuteUsesSourceFormatEndpoint(t *testing.T) {
	host := &fakeHost{upstreams: []fakeUpstream{{status: 200, body: `{"ok":true}`}}}
	p := newTestPlugin(t, host)
	p.Store().EnsureAccount("opencode-go:aaa")
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "opencode-go:aaa", AuthProvider: ProviderID,
		SourceFormat: "openai", Format: "claude",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
		Payload:     []byte(`{"model":"x","messages":[]}`),
	})
	if _, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	url := host.lastURL
	host.mu.Unlock()
	if !strings.HasSuffix(url, "/chat/completions") {
		t.Fatalf("应按 SourceFormat=openai 选端点：%q", url)
	}
}
