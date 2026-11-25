package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// stressHost 是并发安全的极简宿主：auth 文件保存/列表/读取 + 固定 200 上游。
// fakeHost 的 upstreams/script 未完全加锁，只适合顺序脚本；压测用这个。
type stressHost struct {
	mu      sync.Mutex
	byID    map[string]*fakeAuth
	byIndex map[string]*fakeAuth
	seq     int
	doCalls int64
}

func newStressHost() *stressHost {
	return &stressHost{byID: map[string]*fakeAuth{}, byIndex: map[string]*fakeAuth{}}
}

func (h *stressHost) CallHost(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		h.mu.Lock()
		files := make([]map[string]any, 0, len(h.byID))
		for _, a := range h.byID {
			files = append(files, map[string]any{
				"id": a.ID, "auth_index": a.Index, "name": a.Name,
				"provider": a.Provider, "type": a.Provider, "label": a.Label,
			})
		}
		h.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"files": files})
		return wrapOK(raw), nil
	case pluginabi.MethodHostAuthSave:
		var req struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(payload, &req); err != nil || req.Name == "" || len(req.JSON) == 0 {
			return json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "bad_request", Message: "invalid auth file name"}})
		}
		st := parseStorage(req.JSON)
		fp := keyFingerprint(st.APIKey)
		id := ProviderID + ":" + fp
		h.mu.Lock()
		h.seq++
		h.byID[id] = &fakeAuth{ID: id, Index: fmt.Sprintf("idx-%d", h.seq), Name: req.Name, Provider: ProviderID, Label: st.Label, Raw: append([]byte(nil), req.JSON...)}
		h.byIndex[h.byID[id].Index] = h.byID[id]
		h.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"name": req.Name, "path": "/auth/" + req.Name})
		return wrapOK(raw), nil
	case pluginabi.MethodHostAuthGet:
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(payload, &req)
		h.mu.Lock()
		a := h.byIndex[req.AuthIndex]
		h.mu.Unlock()
		if a == nil {
			return wrapOK([]byte(`{}`)), nil
		}
		raw, _ := json.Marshal(map[string]any{"auth_index": a.Index, "name": a.Name, "json": json.RawMessage(a.Raw)})
		return wrapOK(raw), nil
	case pluginabi.MethodHostHTTPDo:
		atomic.AddInt64(&h.doCalls, 1)
		raw, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"ok":true}`), Headers: http.Header{}})
		return wrapOK(raw), nil
	}
	return wrapOK([]byte(`{}`)), nil
}

func (h *stressHost) doCount() int64 { return atomic.LoadInt64(&h.doCalls) }

// 200 个并发客户端 × 4 账号：选号必须均匀，inflight 必须归零，用量必须
// 全部落账。这是用户反复报告的"请求全压一个号"的回归压测。
func TestStressEvenDistributionAcrossAccounts(t *testing.T) {
	host := newStressHost()
	p := newStressPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})

	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		ids = append(ids, addAccount(t, p, fmt.Sprintf("acct%d@x.com", i), fmt.Sprintf("oc_sk_stress_%028d", i)))
	}

	const clients = 200
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	picks := sync.Map{} // authID -> *int64
	for _, id := range ids {
		var n int64
		picks.Store(id, &n)
	}

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cands := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
			for _, id := range ids {
				cands = append(cands, pluginapi.SchedulerAuthCandidate{ID: id, Provider: ProviderID, Priority: 1})
			}
			pickRaw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
				Provider: ProviderID, Model: "opencode-go/x", Candidates: cands,
			})
			out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, pickRaw)
			if err != nil {
				errs <- err
				return
			}
			var env pluginabi.Envelope
			_ = json.Unmarshal(out, &env)
			if !env.OK {
				errs <- fmt.Errorf("pick failed: %s", out)
				return
			}
			var pr pluginapi.SchedulerPickResponse
			_ = json.Unmarshal(env.Result, &pr)
			if !pr.Handled || pr.AuthID == "" {
				errs <- fmt.Errorf("pick not handled: %+v", pr)
				return
			}
			if ctr, ok := picks.Load(pr.AuthID); ok {
				atomic.AddInt64(ctr.(*int64), 1)
			}

			// 每个请求首条消息唯一 → 无会话亲和干扰，只测负载均衡。
			body := []byte(fmt.Sprintf(`{"model":"opencode-go/x","messages":[{"role":"user","content":"stress-%d"}]}`, i))
			execRaw, _ := json.Marshal(pluginapi.ExecutorRequest{
				AuthID: pr.AuthID, AuthProvider: ProviderID, Model: "opencode-go/x", Format: "openai",
				StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-stress"}`),
				Payload:     body, Headers: http.Header{},
			})
			out, err = p.HandleMethod(pluginabi.MethodExecutorExecute, execRaw)
			if err != nil {
				errs <- err
				return
			}
			_ = json.Unmarshal(out, &env)
			if !env.OK {
				errs <- fmt.Errorf("execute failed: %s", out)
				return
			}

			usageRaw, _ := json.Marshal(pluginapi.UsageRecord{
				AuthID: pr.AuthID, Provider: ProviderID, RequestID: fmt.Sprintf("stress-%d", i),
			})
			if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, usageRaw); err != nil {
				errs <- err
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent client: %v", err)
	}

	// 1) 各账号被选次数应接近 200/4=50（允许启动抖动）。
	total := int64(0)
	for _, id := range ids {
		ctr, _ := picks.Load(id)
		n := atomic.LoadInt64(ctr.(*int64))
		total += n
		t.Logf("selection distribution: %s=%d", id, n)
		if n < 30 || n > 70 {
			t.Fatalf("账号 %s 被选 %d 次，分布不均（期望约 50）", id, n)
		}
	}
	if total != clients {
		t.Fatalf("选号总数 %d != %d", total, clients)
	}

	// 2) 全部执行与落账，inflight 必须归零。
	if got := host.doCount(); got != clients {
		t.Fatalf("上游调用 %d != %d", got, clients)
	}
	for id, n := range p.Store().Snapshot().Inflight {
		if n != 0 {
			t.Fatalf("inflight 泄漏：%s=%d", id, n)
		}
	}
	sum := int64(0)
	for _, id := range ids {
		if acc := p.Store().Snapshot().Accounts[id]; acc != nil {
			sum += acc.Usage.Requests
		}
	}
	if sum != clients {
		t.Fatalf("用量落账 %d != %d", sum, clients)
	}
}

// 非本池候选必须让权；混合候选（本池+他池）仍要选中本池，不能误让权。
func TestPickMixedCandidatesChoosesPool(t *testing.T) {
	host := newStressHost()
	p := newStressPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	own := addAccount(t, p, "owner@x.com", "oc_sk_mixed_0000000000000000000")

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "", Model: "opencode-go/x",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "claude:apikey:foreign", Provider: "claude", Priority: 9},
			{ID: own, Provider: ProviderID, Priority: 1},
		},
	})
	out, _ := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var resp pluginapi.SchedulerPickResponse
	_ = json.Unmarshal(env.Result, &resp)
	if !resp.Handled || resp.AuthID != own {
		t.Fatalf("混合候选应选中本池账号：got %+v (own=%s)", resp, own)
	}
}

// 本池候选被删除（墓碑）后必须 Reject，不能因"过滤后为空"让权给宿主
// 内置选号而复活坏号。
func TestPickPurgedPoolCandidateRejects(t *testing.T) {
	host := newStressHost()
	p := newStressPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	id := addAccount(t, p, "gone@x.com", "oc_sk_gone_00000000000000000000")
	p.Store().PurgeAccount(id)

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: ProviderID, Model: "opencode-go/x",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: id, Provider: ProviderID, Priority: 1}},
	})
	out, _ := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var resp pluginapi.SchedulerPickResponse
	_ = json.Unmarshal(env.Result, &resp)
	if !resp.Handled || !resp.Reject {
		t.Fatalf("已删除账号必须 Handled+Reject：got %+v", resp)
	}
}

func newStressPlugin(t *testing.T, host *stressHost) *Plugin {
	t.Helper()
	p, err := New(host, nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// 500 个同时到达的 pick（不执行）：预约必须串行可见、按累计选中次数打散，
// 不能整批压到 ID 最小的账号。这是用户“并发应该平均”的直接回归。
func TestStressBurstPicksStayEven(t *testing.T) {
	host := newStressHost()
	p := newStressPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id := addAccount(t, p, fmt.Sprintf("burst%d@x.com", i), fmt.Sprintf("oc_sk_burst_%028d", i))
		p.Store().LearnLimit(id, 20)
		ids = append(ids, id)
	}

	const picksN = 500
	counts := sync.Map{}
	for _, id := range ids {
		var n int64
		counts.Store(id, &n)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < picksN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cands := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
			for _, id := range ids {
				cands = append(cands, pluginapi.SchedulerAuthCandidate{ID: id, Provider: ProviderID, Priority: 1})
			}
			raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: ProviderID, Model: "x", Candidates: cands})
			out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
			if err != nil {
				t.Errorf("pick: %v", err)
				return
			}
			var env pluginabi.Envelope
			_ = json.Unmarshal(out, &env)
			var resp pluginapi.SchedulerPickResponse
			_ = json.Unmarshal(env.Result, &resp)
			if !resp.Handled || resp.AuthID == "" {
				t.Errorf("pick not handled: %+v", resp)
				return
			}
			if ctr, ok := counts.Load(resp.AuthID); ok {
				atomic.AddInt64(ctr.(*int64), 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	total := int64(0)
	for _, id := range ids {
		ctr, _ := counts.Load(id)
		n := atomic.LoadInt64(ctr.(*int64))
		total += n
		t.Logf("burst distribution: %s=%d", id, n)
		if n < 110 || n > 140 {
			t.Fatalf("突发选号分布不均：%s=%d（期望约 125）", id, n)
		}
	}
	if total != picksN {
		t.Fatalf("选号总数 %d != %d", total, picksN)
	}
}
