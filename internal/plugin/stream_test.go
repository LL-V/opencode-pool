package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// streamRequest 是 host 调 executor.execute_stream 的 rpcExecutorRequest 包装
// （internal/pluginhost/rpc_schema.go:100-104：ExecutorRequest + stream_id + host_callback_id）。
func streamRequest(streamID string) []byte {
	raw, _ := json.Marshal(struct {
		pluginapi.ExecutorRequest
		StreamID       string `json:"stream_id,omitempty"`
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "opencode-go:aaa", AuthProvider: ProviderID,
			Format: "openai", Stream: true,
			StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
			Payload:     []byte(`{"model":"opencode-go/x","messages":[]}`),
			Headers:     httpHeader(),
		},
		StreamID:       streamID,
		HostCallbackID: "cb-1",
	})
	return raw
}

func httpHeader() map[string][]string { return map[string][]string{} }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("超时等待：%s", what)
}

// M2 实时流式（异步模式）：execute_stream 带 stream_id 时，RPC 立即返回
// 空 chunks（host 视为异步标记），chunk 经 host.stream.emit 推送、
// host.stream.close 收尾（rpc_client_stream.go:29-47）。
func TestExecuteStreamAsyncEmitsChunks(t *testing.T) {
	host := &fakeHost{
		upstreams: []fakeUpstream{{status: 200}},
		script: []fakeChunk{
			{payload: `data: {"d":1}`},
			{payload: `data: [DONE]`, done: true},
		},
	}
	p := newTestPlugin(t, host)

	out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, streamRequest("plg-1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("2xx 流应以 ok 信封返回（空 chunks=异步标记）：%s", out)
	}
	var resp struct {
		Chunks []json.RawMessage `json:"chunks"`
	}
	_ = json.Unmarshal(env.Result, &resp)
	if len(resp.Chunks) != 0 {
		t.Fatalf("异步模式 RPC 响应 chunks 必须为空（host 靠它区分异步）：%s", env.Result)
	}

	waitFor(t, "host.stream.close 收尾", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return len(host.closes) == 1
	})
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emits) != 2 ||
		host.emits[0] != "plg-1|data: {\"d\":1}" ||
		host.emits[1] != "plg-1|data: [DONE]" {
		t.Fatalf("应按序 emit 两个 chunk 到 stream_id=plg-1：%v", host.emits)
	}
	if host.closes[0] != "plg-1|" {
		t.Fatalf("成功收尾应无错误：%v", host.closes)
	}
}

// 异步模式下首响应非 2xx（如并发 429）：DoStream 同步拿到 status，
// 错误信封直接随 RPC 返回（host 走换号），不 emit/close host 流。
func TestExecuteStreamAsyncUpstream429(t *testing.T) {
	host := &fakeHost{upstreams: []fakeUpstream{{
		status: 429,
		body:   `{"error":{"type":"rate_limit_error","message":"... concurrency limit for this model (current limit: 6)"}}`,
	}}}
	p := newTestPlugin(t, host)

	out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, streamRequest("plg-1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if env.OK || env.Error == nil {
		t.Fatalf("429 应随 RPC 返回错误信封：%s", out)
	}
	e := env.Error
	if e.Code != "transient_rate_limit" || e.RetryAfterSeconds != 2 || e.CredentialScoped {
		t.Fatalf("并发 429 错误码协议：got %+v", e)
	}
	host.mu.Lock()
	emits, closes := len(host.emits), len(host.closes)
	host.mu.Unlock()
	if emits != 0 || closes != 0 {
		t.Fatalf("首响应失败不应 emit/close host 流：emits=%d closes=%d", emits, closes)
	}
	sawUpstreamClose := false
	for _, c := range host.recorded() {
		if c == pluginabi.MethodHostHTTPStreamClose {
			sawUpstreamClose = true
		}
	}
	if !sawUpstreamClose {
		t.Fatal("应释放上游流（host.http.stream_close）")
	}
}

// 无 stream_id（旧 host / count_tokens 复用路径）：保持同步数组模式。
func TestExecuteStreamSyncArrayWithoutStreamID(t *testing.T) {
	host := &fakeHost{
		upstreams: []fakeUpstream{{status: 200}},
		script: []fakeChunk{
			{payload: `data: {"d":1}`},
			{payload: `data: [DONE]`, done: true},
		},
	}
	p := newTestPlugin(t, host)

	out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, streamRequest(""))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("应为 ok 信封：%s", out)
	}
	var resp struct {
		Chunks []struct {
			Payload []byte `json:"Payload"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal(env.Result, &resp)
	if len(resp.Chunks) != 2 || string(resp.Chunks[0].Payload) != `data: {"d":1}` {
		t.Fatalf("同步模式应返回 chunk 数组：%s", env.Result)
	}
	for _, c := range host.recorded() {
		if c == pluginabi.MethodHostStreamEmit || c == pluginabi.MethodHostStreamClose {
			t.Fatalf("同步模式不应触发 host.stream.*：%v", host.recorded())
		}
	}
}
