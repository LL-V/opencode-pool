package plugin

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// chaosHost 是并发安全的故障注入宿主：每条上游流独立编号与脚本，
// 支持随机混合非 2xx、中途错误、emit 失败，用于全链路测压。
type chaosHost struct {
	mu        sync.Mutex
	authByID  map[string]map[string]any
	nextUp    int
	upScript  map[string][]fakeChunk // 上游流 id → 按序 chunk
	upClosed  map[string]bool
	emitCount map[string]int // 插件流 id → emit 次数
	closeOnce map[string]int // 插件流 id → close 次数（必须恰 1）
	statusSeq []int          // 非流式：循环注入的状态
	statusIdx int
	failEmit  bool
}

func newChaosHost() *chaosHost {
	return &chaosHost{
		authByID:  map[string]map[string]any{},
		upScript:  map[string][]fakeChunk{},
		upClosed:  map[string]bool{},
		emitCount: map[string]int{},
		closeOnce: map[string]int{},
	}
}

func (c *chaosHost) CallHost(method string, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case pluginabi.MethodHostHTTPDo:
		if len(c.statusSeq) > 0 {
			st := c.statusSeq[c.statusIdx%len(c.statusSeq)]
			c.statusIdx++
			raw, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: st, Body: []byte(`{"ok":true}`)})
			return wrapOK(raw), nil
		}
		raw, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)})
		return wrapOK(raw), nil
	case pluginabi.MethodHostHTTPDoStream:
		c.nextUp++
		id := fmt.Sprintf("up-%d", c.nextUp)
		script, ok := c.upScript[id]
		if !ok {
			script = []fakeChunk{{payload: `data: {"d":1}`, done: true}}
		}
		c.upScript[id] = script
		raw, _ := json.Marshal(map[string]any{"status_code": 200, "stream_id": id})
		return wrapOK(raw), nil
	case pluginabi.MethodHostHTTPStreamRead:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(payload, &req)
		script := c.upScript[req.StreamID]
		if len(script) == 0 {
			raw, _ := json.Marshal(map[string]any{"done": true})
			return wrapOK(raw), nil
		}
		ch := script[0]
		c.upScript[req.StreamID] = script[1:]
		raw, _ := json.Marshal(map[string]any{"payload": []byte(ch.payload), "error": ch.errMsg, "done": ch.done})
		return wrapOK(raw), nil
	case pluginabi.MethodHostHTTPStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(payload, &req)
		c.upClosed[req.StreamID] = true
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostStreamEmit:
		if c.failEmit {
			return json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "gone", Message: "host stream dead"}})
		}
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(payload, &req)
		c.emitCount[req.StreamID]++
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal(payload, &req)
		c.closeOnce[req.StreamID]++
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostAuthSave:
		var req struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		_ = json.Unmarshal(payload, &req)
		if req.Name != "" && len(req.JSON) > 0 {
			fp := keyFingerprint(parseStorage(req.JSON).APIKey)
			id := ProviderID + ":" + fp
			c.authByID[id] = map[string]any{
				"id": id, "auth_index": "idx-" + fp[:6], "name": req.Name,
				"provider": ProviderID, "json": req.JSON,
			}
		}
		raw, _ := json.Marshal(map[string]any{"name": req.Name, "path": "/auth/" + req.Name})
		return wrapOK(raw), nil
	case pluginabi.MethodHostAuthGet:
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(payload, &req)
		for _, auth := range c.authByID {
			if idx, _ := auth["auth_index"].(string); idx == req.AuthIndex && idx != "" {
				raw, _ := json.Marshal(map[string]any{"auth_index": idx, "json": auth["json"]})
				return wrapOK(raw), nil
			}
		}
		return wrapOK([]byte(`{}`)), nil
	case pluginabi.MethodHostAuthList:
		files := make([]map[string]any, 0, len(c.authByID))
		for _, auth := range c.authByID {
			files = append(files, map[string]any{
				"id": auth["id"], "auth_index": auth["auth_index"],
				"name": auth["name"], "provider": ProviderID, "type": ProviderID,
			})
		}
		raw, _ := json.Marshal(map[string]any{"files": files})
		return wrapOK(raw), nil
	}
	return wrapOK([]byte(`{}`)), nil
}

func newPluginWithHost(t *testing.T, host HostCaller) *Plugin {
	t.Helper()
	p, err := New(host, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func chaosStreamRequest(id string) []byte {
	raw, _ := json.Marshal(struct {
		pluginapi.ExecutorRequest
		StreamID string `json:"stream_id,omitempty"`
	}{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "opencode-go:aaa", AuthProvider: ProviderID,
			Format: "openai", Stream: true,
			StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
			Payload:     []byte(`{"model":"opencode-go/x","messages":[]}`),
			Headers:     map[string][]string{},
		},
		StreamID: id,
	})
	return raw
}

// 压测 1：20 并发异步流 × 10 chunk，每条插件流必须按序 emit 且恰好 close
// 一次；上游流全部释放；无 panic、无挂死。
func TestChaosConcurrentAsyncStreams(t *testing.T) {
	host := newChaosHost()
	const nStreams = 20
	const nChunks = 10
	for i := 1; i <= nStreams; i++ {
		id := fmt.Sprintf("up-%d", i)
		script := make([]fakeChunk, 0, nChunks)
		for j := 0; j < nChunks-1; j++ {
			script = append(script, fakeChunk{payload: fmt.Sprintf("s%d-c%d", i, j)})
		}
		script = append(script, fakeChunk{payload: fmt.Sprintf("s%d-done", i), done: true})
		host.upScript[id] = script
	}
	p := newPluginWithHost(t, host)

	var wg sync.WaitGroup
	for i := 1; i <= nStreams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, chaosStreamRequest(fmt.Sprintf("plg-%d", i)))
			if err != nil {
				t.Errorf("stream %d: %v", i, err)
				return
			}
			var env pluginabi.Envelope
			_ = json.Unmarshal(out, &env)
			if !env.OK {
				t.Errorf("stream %d 应 ok：%s", i, out)
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		closed := len(host.closeOnce)
		host.mu.Unlock()
		if closed == nStreams {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.closeOnce) != nStreams {
		t.Fatalf("应 %d 条流全部 close：got %d", nStreams, len(host.closeOnce))
	}
	for i := 1; i <= nStreams; i++ {
		id := fmt.Sprintf("plg-%d", i)
		if host.closeOnce[id] != 1 {
			t.Fatalf("流 %s 必须 close 恰好一次：got %d", id, host.closeOnce[id])
		}
		if host.emitCount[id] != nChunks {
			t.Fatalf("流 %s 应 emit %d 块：got %d", id, nChunks, host.emitCount[id])
		}
		if !host.upClosed[fmt.Sprintf("up-%d", i)] {
			t.Fatalf("上游流 up-%d 未释放", i)
		}
	}
}

// 压测 2：emit 全部失败（宿主侧流已死）→ 泵流退出、上游释放、不 close
// 死流；后续流不受影响。
func TestChaosEmitFailureReleasesUpstream(t *testing.T) {
	host := newChaosHost()
	host.failEmit = true
	host.upScript["up-1"] = []fakeChunk{{payload: `a`}, {payload: `b`, done: true}}
	p := newPluginWithHost(t, host)

	out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, chaosStreamRequest("plg-dead"))
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	if !env.OK {
		t.Fatalf("emit 失败不影响 RPC 返回 ok：%s", out)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		released := host.upClosed["up-1"]
		host.mu.Unlock()
		if released {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if !host.upClosed["up-1"] {
		t.Fatal("emit 失败后必须释放上游流")
	}
	// emit 失败也必须收尾宿主流（带 connection_lifecycle），否则请求会挂到
	// 客户端超时、usage/inflight 迟迟不回收。
	if len(host.closeOnce) != 1 {
		t.Fatalf("emit 失败后应恰好 close 宿主流一次：%v", host.closeOnce)
	}
}

// 压测 3：50 并发非流式请求 × 混合故障（200/429并发/429限额/401/403/500）；
// 每请求配对 usage.handle 后 inflight 必须全体归零，无负值无泄漏。
func TestChaosMixedFailuresInflightBalance(t *testing.T) {
	host := newChaosHost()
	host.statusSeq = []int{200, 429, 429, 401, 403, 500}
	p := newPluginWithHost(t, host)
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(pluginapi.ExecutorRequest{
				AuthID: "opencode-go:aaa", AuthProvider: ProviderID,
				Format:      "openai",
				StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
				Payload:     []byte(`{"model":"opencode-go/x","messages":[]}`),
				Headers:     map[string][]string{},
			})
			out, err := p.HandleMethod(pluginabi.MethodExecutorExecute, raw)
			if err != nil {
				t.Errorf("execute: %v", err)
				return
			}
			var env pluginabi.Envelope
			_ = json.Unmarshal(out, &env)
			// 宿主对每次尝试恰一次 usage.handle（失败也记账）
			rawUsage, _ := json.Marshal(pluginapi.UsageRecord{
				AuthID: "opencode-go:aaa", SessionID: "s", Failed: !env.OK,
			})
			if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, rawUsage); err != nil {
				t.Errorf("usage: %v", err)
			}
		}()
	}
	wg.Wait()

	snap := p.Store().Snapshot()
	if got := snap.Inflight["opencode-go:aaa"]; got != 0 {
		t.Fatalf("50 请求全部配对后 inflight 应 0：got %d", got)
	}
}
