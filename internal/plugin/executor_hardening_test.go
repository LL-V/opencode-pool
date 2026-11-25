package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 限额 429 没有窗口词时必须归到 rolling；空键窗口永远等不到 /usage 更新，
// 会把账号封到幽灵窗口到期。
func TestUsageQuotaWithoutWindowUsesRolling(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := "opencode-go:nw"
	p.Store().EnsureAccount(id)

	raw, _ := json.Marshal(pluginapi.UsageRecord{
		AuthID: id, Provider: ProviderID, RequestID: "q-1", Failed: true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","message":"usage limit reached"}}`,
		},
	})
	if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, raw); err != nil {
		t.Fatal(err)
	}
	acc := p.Store().Snapshot().Accounts[id]
	if acc.Windows[""] != nil {
		t.Fatalf("不得写入空窗口键：%+v", acc.Windows)
	}
	if acc.Windows["rolling"] == nil {
		t.Fatalf("应写入 rolling 窗口：%+v", acc.Windows)
	}
}

// 流式 2xx 也是凭据可用的证据，必须清掉 verify_pending/requires_sign_in。
func TestStreamSuccessClearsVerifyState(t *testing.T) {
	host := &fakeHost{
		upstreams: []fakeUpstream{{status: 200}},
		script:    []fakeChunk{{payload: `data: ok`, done: true}},
	}
	p := newTestPlugin(t, host)
	id := "opencode-go:verify"
	p.Store().EnsureAccount(id)
	p.Store().SetVerifyPending(id, true)
	p.Store().SetRequiresSignIn(id, true)

	raw, _ := json.Marshal(struct {
		pluginapi.ExecutorRequest
		StreamID string `json:"stream_id,omitempty"`
	}{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: id, AuthProvider: ProviderID, Format: "openai", Stream: true,
			StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-v"}`),
			Payload:     []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
			Headers:     http.Header{},
		},
		StreamID: "sv-1",
	})
	out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if !env.OK {
		t.Fatalf("流式应成功：%s", out)
	}
	acc := p.Store().Snapshot().Accounts[id]
	if acc.VerifyPending || acc.RequiresSignIn {
		t.Fatalf("2xx 应清空疑态：%+v", acc)
	}
}

// 缺 api_key 是账号级问题：必须让宿主换号（401+CredentialScope），
// 不能 request_scoped 直接终止请求；usage 的 401 会把账号标记为需登录。
func TestMissingAPIKeyIsCredentialScopedAndPairsInflight(t *testing.T) {
	host := &fakeHost{upstreams: []fakeUpstream{{status: 200}}}
	p := newTestPlugin(t, host)
	id := "opencode-go:nokey"
	p.Store().EnsureAccount(id)

	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: id, AuthProvider: ProviderID, Format: "openai",
		StorageJSON: []byte(`{"type":"opencode-go"}`),
		Payload:     []byte(`{"model":"x"}`), Headers: http.Header{},
	})
	out, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if env.OK || env.Error == nil {
		t.Fatalf("应返回错误：%s", out)
	}
	if env.Error.Code == "request_scoped" || env.Error.HTTPStatus != 401 || !env.Error.CredentialScoped {
		t.Fatalf("缺 key 应 401+CredentialScoped：%+v", env.Error)
	}
	if got := p.Store().Snapshot().Inflight[id]; got != 1 {
		t.Fatalf("失败尝试也必须先计数：inflight=%d", got)
	}

	// 宿主对失败尝试同样投递 usage：配对归零并标记需登录。
	usageRaw, _ := json.Marshal(pluginapi.UsageRecord{
		AuthID: id, Provider: ProviderID, RequestID: "nk-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: `{"error":{"message":"requires an API key"}}`},
	})
	if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, usageRaw); err != nil {
		t.Fatal(err)
	}
	acc := p.Store().Snapshot()
	if acc.Inflight[id] != 0 {
		t.Fatalf("usage 后 inflight 应归零：%d", acc.Inflight[id])
	}
	if !acc.Accounts[id].RequiresSignIn {
		t.Fatal("401 失败应标记需重新登录")
	}
}

// 旧 host 同步数组路径：中途错误必须返回错误信封（Err 无法 JSON 往返）；
// 空流必须带一个块，避免宿主把 chunks=[] 误判为异步而挂起。
func TestSyncStreamLegacySafety(t *testing.T) {
	host := &fakeHost{
		upstreams: []fakeUpstream{{status: 200}},
		script:    []fakeChunk{{errMsg: "upstream reset"}},
	}
	p := newTestPlugin(t, host)
	out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, streamRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if env.OK || env.Error == nil || env.Error.Code != "stream_truncated" {
		t.Fatalf("同步中途错误应返回 stream_truncated 信封：%s", out)
	}

	host2 := &fakeHost{
		upstreams: []fakeUpstream{{status: 200}},
		script:    []fakeChunk{{done: true}},
	}
	p2 := newTestPlugin(t, host2)
	out, err = p2.HandleMethod(pluginabi.MethodExecutorExecuteStream, streamRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	env = pluginabi.Envelope{}
	_ = json.Unmarshal(out, &env)
	if !env.OK {
		t.Fatalf("空流也应成功：%s", out)
	}
	var resp struct {
		Chunks []json.RawMessage `json:"chunks"`
	}
	_ = json.Unmarshal(env.Result, &resp)
	if len(resp.Chunks) == 0 {
		t.Fatalf("空流必须返回至少一个块，避免被判为异步：%s", env.Result)
	}
}
