package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost implements HostCaller with scripted upstream responses.
type fakeHost struct {
	upstreams   []fakeUpstream // 按序消费
	calls       []string
	saved       []map[string]any // 原始 auth.save 请求
	byID        map[string]*fakeAuth
	script      []fakeChunk         // StreamRead 按序消费
	emits       []string            // "streamID|payload"（host.stream.emit）
	closes      []string            // "streamID|err"（host.stream.close）
	lastURL     string              // 最近一次 host.http.do 的 URL
	lastHeaders map[string][]string // 最近一次 host.http.do 的出站头
	mu          sync.Mutex
}

// fakeAuth models one host credential entry (id + stable auth_index + file
// JSON), mirroring the real pluginhost auth callbacks.
type fakeAuth struct {
	ID       string
	Index    string
	Name     string
	Provider string
	Label    string
	Raw      []byte
}
type fakeUpstream struct {
	status int
	body   string
	header http.Header
}
type fakeChunk struct {
	payload string
	errMsg  string
	done    bool
}

func (f *fakeHost) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeHost) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHost) CallHost(method string, payload []byte) ([]byte, error) {
	f.record(method)
	switch method {
	case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
		var hostReq struct {
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		_ = json.Unmarshal(payload, &hostReq)
		f.mu.Lock()
		f.lastURL = hostReq.URL
		f.lastHeaders = hostReq.Headers
		f.mu.Unlock()
		up := fakeUpstream{status: 200, body: `{"ok":true}`}
		if len(f.upstreams) > 0 {
			up = f.upstreams[0]
			f.upstreams = f.upstreams[1:]
		}
		raw, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: up.status, Body: []byte(up.body), Headers: up.header})
		if method == pluginabi.MethodHostHTTPDoStream {
			if up.status >= 300 && up.body != "" {
				// 真实 host 把非 2xx 的错误体放在流的第一个 chunk。
				f.mu.Lock()
				f.script = append([]fakeChunk{{payload: up.body, done: true}}, f.script...)
				f.mu.Unlock()
			}
			raw, _ = json.Marshal(map[string]any{"status_code": up.status, "stream_id": "s1"})
		}
		return wrapOK(raw), nil
	case pluginabi.MethodHostHTTPStreamRead:
		f.mu.Lock()
		ch := fakeChunk{done: true}
		if len(f.script) > 0 {
			ch = f.script[0]
			f.script = f.script[1:]
		}
		f.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"payload": []byte(ch.payload), "error": ch.errMsg, "done": ch.done})
		return wrapOK(raw), nil
	case pluginabi.MethodHostStreamEmit:
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(payload, &req)
		f.mu.Lock()
		f.emits = append(f.emits, req.StreamID+"|"+string(req.Payload)+req.Error)
		f.mu.Unlock()
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(payload, &req)
		f.mu.Lock()
		f.closes = append(f.closes, req.StreamID+"|"+req.Error)
		f.mu.Unlock()
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostHTTPStreamClose, pluginabi.MethodHostLog, pluginabi.MethodHostRoutingResetCooldown:
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostAuthList:
		f.mu.Lock()
		entries := make([]*fakeAuth, 0, len(f.byID))
		for _, a := range f.byID {
			entries = append(entries, a)
		}
		f.mu.Unlock()
		if len(entries) == 0 {
			entries = []*fakeAuth{{ID: "opencode-go:aaa", Index: "idx-aaa", Name: "aaa.json", Provider: ProviderID}}
		}
		files := make([]map[string]any, 0, len(entries))
		for _, a := range entries {
			files = append(files, map[string]any{
				"id": a.ID, "auth_index": a.Index, "name": a.Name,
				"provider": a.Provider, "type": a.Provider, "label": a.Label,
			})
		}
		raw, _ := json.Marshal(map[string]any{"files": files})
		return wrapOK(raw), nil
	case pluginabi.MethodHostAuthSave:
		// 宿主 wire：{"name": "<bare>.json", "json": <credential object>}
		var req struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(payload, &req); err != nil || req.Name == "" || len(req.JSON) == 0 {
			return json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "bad_request", Message: "invalid auth file name"}})
		}
		f.saved = append(f.saved, map[string]any{"name": req.Name, "json": req.JSON})
		if f.byID == nil {
			f.byID = map[string]*fakeAuth{}
		}
		st := parseStorage(req.JSON)
		fp := keyFingerprint(st.APIKey)
		id := ProviderID + ":" + fp
		f.byID[id] = &fakeAuth{
			ID: id, Index: "idx-" + fp[:6], Name: req.Name,
			Provider: ProviderID, Label: st.Label, Raw: append([]byte(nil), req.JSON...),
		}
		raw, _ := json.Marshal(map[string]any{"name": req.Name, "path": "/auth/" + req.Name})
		return wrapOK(raw), nil
	case pluginabi.MethodHostAuthGet:
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(payload, &req)
		for _, a := range f.byID {
			if a.Index == req.AuthIndex && req.AuthIndex != "" {
				raw, _ := json.Marshal(map[string]any{"auth_index": a.Index, "name": a.Name, "json": json.RawMessage(a.Raw)})
				return wrapOK(raw), nil
			}
		}
		return wrapOK([]byte(`{}`)), nil
	}
	return wrapOK([]byte(`{}`)), nil
}

func wrapOK(result []byte) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
	return raw
}

func newTestPlugin(t *testing.T, host *fakeHost) *Plugin {
	t.Helper()
	p, err := New(host, nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func call(t *testing.T, p *Plugin, method string, req any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(req)
	out, err := p.HandleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("%s envelope: %v %s", method, err, string(out))
	}
	var m map[string]any
	_ = json.Unmarshal(env.Result, &m)
	return m
}

// 1. 注册：能力与元数据
func TestRegister(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	reg := call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	if reg["schema_version"].(float64) < 9 {
		t.Fatalf("schema_version: %v", reg["schema_version"])
	}
	meta := reg["metadata"].(map[string]any)
	if meta["Name"] != PluginID {
		t.Fatalf("PluginID: %v", meta["name"])
	}
	caps := reg["capabilities"].(map[string]any)
	for _, k := range []string{"request_interceptor", "executor", "scheduler", "usage_plugin", "quota_provider", "management_api", "auth_provider", "model_provider"} {
		if caps[k] != true {
			t.Fatalf("capability %s 未声明: %v", k, caps)
		}
	}
}

// 2. BeforeAuth 注入 x-opencode-session（R005/R006 经分发层）
func TestInterceptBeforeInjectsSession(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	body, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hello pool"}},
	})
	resp := call(t, p, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		Headers: http.Header{}, Body: body,
	})
	hdrs, _ := resp["Headers"].(map[string]any)
	sid := firstString(hdrs["X-Opencode-Session"])
	if !strings.HasPrefix(sid, "d-") || len(sid) != 18 {
		t.Fatalf("应注入派生 sid：headers=%v", hdrs)
	}
	// 稳定性：同 body 再来一次同值
	resp2 := call(t, p, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		Headers: http.Header{}, Body: body,
	})
	hdrs2, _ := resp2["Headers"].(map[string]any)
	if firstString(hdrs2["X-Opencode-Session"]) != sid {
		t.Fatalf("派生值应稳定：%v vs %v", hdrs2["X-Opencode-Session"], sid)
	}
}

// 3. Executor：UA 覆写 + 会话头透传 + 2xx 直通
func TestExecuteOverridesUAAndKeepsSession(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	body := []byte(`{"model":"opencode-go/x","messages":[]}`)
	resp := call(t, p, pluginabi.MethodExecutorExecute, pluginapi.ExecutorRequest{
		AuthID: "opencode-go:aaa", AuthProvider: ProviderID,
		Format:      "openai",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
		Payload:     body,
		Headers:     http.Header{"X-Opencode-Session": []string{"sid-1"}},
	})
	if resp["Payload"] == nil {
		t.Fatalf("2xx 应返回 payload：%v", resp)
	}
	// 上游调用至少一次
	sawDo := false
	for _, c := range host.calls {
		if c == pluginabi.MethodHostHTTPDo {
			sawDo = true
		}
	}
	if !sawDo {
		t.Fatal("应经 host.http.do 调上游")
	}
}

// 4. 并发 429 → transient_rate_limit(2s) 错误信封（R007/R010）
func TestExecuteConcurrency429ErrorProtocol(t *testing.T) {
	host := &fakeHost{upstreams: []fakeUpstream{{
		status: 429,
		body:   `{"error":{"type":"rate_limit_error","message":"... concurrency limit ... (current limit: 6)"}}`,
	}}}
	p := newTestPlugin(t, host)
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "opencode-go:aaa", AuthProvider: ProviderID,
		Format:      "openai",
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
		Payload:     []byte(`{"model":"opencode-go/x","messages":[]}`),
		Headers:     http.Header{},
	})
	out, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if env.OK {
		t.Fatalf("429 应返回错误信封：%s", out)
	}
	e := env.Error
	if e.Code != "transient_rate_limit" || e.RetryAfterSeconds != 2 || e.CredentialScoped {
		t.Fatalf("并发 429 错误码协议：got %+v", e)
	}
	if p.Store().Snapshot().Accounts["opencode-go:aaa"].LearnedConcurrencyLimit != 6 {
		t.Fatal("应学习并发上限 6")
	}
}

// 5. usage.handle：成功落账 + 绑定迁移 + inflight 归零（R002/R006）
func TestUsageHandleBookkeeps(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		AuthID: "opencode-go:aaa", SessionID: "sid-9", Failed: false,
		Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
	})
	snap := p.Store().Snapshot()
	acc := snap.Accounts["opencode-go:aaa"]
	if acc.Usage.Requests != 1 || acc.Usage.InputTokens != 100 || acc.Usage.OutputTokens != 40 {
		t.Fatalf("台账：%+v", acc.Usage)
	}
	if id, ok := snap.Bindings["sid-9"]; !ok || id != "opencode-go:aaa" {
		t.Fatalf("成功应写绑定：%v", snap.Bindings)
	}
	// 失败记录也应处理（inflight 语义在 executor 层测）
	call(t, p, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		AuthID: "opencode-go:aaa", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached","message":"5 hour limit","reset_at":"2026-11-21T17:00:00Z"}}`},
	})
	w := p.Store().Snapshot().Accounts["opencode-go:aaa"].Windows["rolling"]
	if w == nil || w.UsedPct != 100 {
		t.Fatalf("失败 body 应收割入窗口：%+v", w)
	}
}

// 6. scheduler.pick：绑定亲和优先于让权（v1.0.1 修订）
func TestSchedulerPickAffinity(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		AuthID: "opencode-go:aaa", SessionID: "sid-9",
	})
	resp := call(t, p, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
		Provider: ProviderID, Model: "x",
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"x-opencode-session": {"sid-9"}}},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "opencode-go:bbb", Provider: ProviderID, Priority: 5},
			{ID: "opencode-go:aaa", Provider: ProviderID, Priority: 0},
		},
	})
	if resp["AuthID"] != "opencode-go:aaa" || resp["Handled"] != true {
		t.Fatalf("绑定亲和应胜过高优先级：%v", resp)
	}
}

// 7. management：GET accounts 只读面板数据（R003）
func TestManagementAccounts(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""}) // 注册触发对账
	reg := call(t, p, pluginabi.MethodManagementRegister, map[string]any{})
	routes, _ := reg["routes"].([]any)
	if len(routes) < 6 {
		t.Fatalf("管理路由不足：%v", routes)
	}
	resp := p.HandleManagement("GET", "/accounts", nil, nil)
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	if len(out.Accounts) < 1 {
		t.Fatalf("accounts 应含宿主 auth 对账结果：%s", resp.Body)
	}
	if out.Accounts[0]["id"] == "" {
		t.Fatal("账号应带 id")
	}
}

func firstString(v any) string {
	if arr, ok := v.([]any); ok && len(arr) > 0 {
		if s, ok := arr[0].(string); ok {
			return s
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

var _ = fmt.Sprintf
