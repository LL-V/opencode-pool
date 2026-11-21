package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// F1 全链路模拟（R005/R006/R007/R008/R010 集成验收）：
// 注册→面板添加 2 账号→无头客户端派生会话→绑定→并发 429 换号→
// 限额 429 暂停→窗口过期恢复→面板状态正确→inflight 平衡。
func TestEndToEndLifecycle(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})

	// 面板添加两个账户（邮箱+key）。
	aID := addAccount(t, p, "a@x.com", "oc_sk_e2e_aaaaaaaaaaaaaaaaaa")
	bID := addAccount(t, p, "b@x.com", "oc_sk_e2e_bbbbbbbbbbbbbbbbbb")

	// 无头客户端：无会话头，首条 user 消息派生稳定 sid（R005/R006）。
	body := []byte(`{"model":"opencode-go/x","messages":[{"role":"user","content":"帮我写一个排序"}]}`)
	hdrs := map[string][]string{}

	pickReq := func(sid string, tried map[string]bool, cands ...string) (authID string, yield bool) {
		t.Helper()
		list := make([]pluginapi.SchedulerAuthCandidate, 0, len(cands))
		for _, id := range cands {
			if tried[id] {
				continue
			}
			list = append(list, pluginapi.SchedulerAuthCandidate{ID: id, Provider: ProviderID, Priority: 1})
		}
		raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: "opencode-go/x",
			Options:    pluginapi.SchedulerOptions{Headers: map[string][]string{"x-opencode-session": {sid}}},
			Candidates: list,
		})
		out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
		if err != nil {
			t.Fatal(err)
		}
		var env pluginabi.Envelope
		_ = json.Unmarshal(out, &env)
		var resp struct {
			AuthID  string
			Handled bool
			Reject  bool
		}
		_ = json.Unmarshal(env.Result, &resp)
		_ = resp.Reject
		return resp.AuthID, !resp.Handled
	}

	execStream := func(authID string) (ok bool, code string) {
		t.Helper()
		raw, _ := json.Marshal(struct {
			pluginapi.ExecutorRequest
			StreamID string `json:"stream_id,omitempty"`
		}{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthID: authID, AuthProvider: ProviderID, Format: "openai", Stream: true,
				StorageJSON: []byte(`{"type":"opencode-go","api_key":"sk-test"}`),
				Payload:     body, Headers: hdrs,
			},
			StreamID: "e2e-1",
		})
		// 异步泵 goroutine 在 RPC 返回后仍在跑；改脚本前等本轮流收尾。
		closesBefore := countCalls(host, pluginabi.MethodHostStreamClose)
		out, err := p.HandleMethod(pluginabi.MethodExecutorExecuteStream, raw)
		if err != nil {
			t.Fatal(err)
		}
		var env pluginabi.Envelope
		_ = json.Unmarshal(out, &env)
		if env.OK {
			waitFor(t, "流收尾", func() bool {
				return countCalls(host, pluginabi.MethodHostStreamClose) > closesBefore
			})
			return true, ""
		}
		return false, env.Error.Code
	}

	usage := func(authID string, failed bool) {
		t.Helper()
		// 真实宿主行为：无头客户端无 canonical session，record.SessionID 为空；
		// 绑定由 executor 成功路径（派生键）写入。
		raw, _ := json.Marshal(pluginapi.UsageRecord{AuthID: authID, Failed: failed})
		if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, raw); err != nil {
			t.Fatal(err)
		}
	}

	// ——第 1 轮：A 被选中（或让权后宿主默认），流式成功，绑定建立——
	first, _ := pickReq("", nil, aID, bID)
	if first == "" {
		first = aID // 让权时宿主默认取第一个（模拟宿主行为）
	}
	host.upstreams = []fakeUpstream{{status: 200}}
	host.script = []fakeChunk{{payload: `data: 1`, done: true}}
	ok, code := execStream(first)
	if !ok {
		t.Fatalf("首轮流式应成功：code=%s", code)
	}
	usage(first, false)
	// usage.handle 成功→绑定 sid→first。派生 sid 需与 resolveSession 一致：
	// 直接用 body 派生值验证绑定。
	sid := deriveSid(t, p, hdrs, body)
	if got, _ := pickReq(sid, nil, aID, bID); got != first {
		t.Fatalf("同会话应命中绑定号 %s：got %q", first, got)
	}

	// ——第 2 轮：绑定号撞并发 429（R007/R010）→错误信封换号，不透传——
	host.upstreams = []fakeUpstream{{
		status: 429,
		body:   `{"error":{"type":"rate_limit_error","message":"Your workspace has reached the concurrency limit for this model (current limit: 6)"}}`,
	}}
	host.script = nil
	ok, code = execStream(first)
	if ok || code != "transient_rate_limit" {
		t.Fatalf("并发 429 应返回 transient_rate_limit 换号：ok=%v code=%s", ok, code)
	}
	usage(first, true)
	// 宿主换号：排除已试号再 pick；剩单候选无信号时让权=合同语义，
	// 宿主默认选剩余候选（此处模拟宿主行为）。
	tried := map[string]bool{first: true}
	second, _ := pickReq(sid, tried, aID, bID)
	if second == "" {
		second = other(first, aID, bID)
	}
	if second == first {
		t.Fatalf("换号应选中另一账号：got %q", second)
	}
	host.upstreams = []fakeUpstream{{status: 200}}
	host.script = []fakeChunk{{payload: `data: 2`, done: true}}
	if ok, code = execStream(second); !ok {
		t.Fatalf("换号后应成功：code=%s", code)
	}
	usage(second, false)

	// ——第 3 轮：A 撞限额 429（带 reset，R008）→窗口标记→pick 永不选 A——
	resetAt := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	host.upstreams = []fakeUpstream{{
		status: 429,
		body:   `{"error":{"type":"usage_limit_reached","message":"weekly usage limit reached","reset_at":"` + resetAt + `"}}`,
	}}
	if ok, code = execStream(aID); ok || code != "credential_quota" {
		t.Fatalf("限额 429 应返回 credential_quota：ok=%v code=%s", ok, code)
	}
	usage(aID, true)
	// A 已限额：无论绑定与否，pick 必须跳过 A。
	if got, _ := pickReq(sid, nil, aID, bID); got == aID {
		t.Fatal("限额中的账号不得被选中")
	}

	// ——面板状态（R003）：A=exhausted/限额窗口可见，B 正常——
	resp := p.HandleManagement("GET", "/accounts", nil, nil)
	var listed struct {
		Accounts []accountView `json:"accounts"`
	}
	_ = json.Unmarshal(resp.Body, &listed)
	byID := map[string]accountView{}
	for _, v := range listed.Accounts {
		byID[v.ID] = v
	}
	if byID[aID].Status != "exhausted" {
		t.Fatalf("A 应显示 exhausted：got %q（windows=%v）", byID[aID].Status, byID[aID].Windows)
	}
	if len(byID[aID].Windows) == 0 || byID[aID].Windows[0].Window != "weekly" {
		t.Fatalf("A 应有 weekly 窗口记录：%v", byID[aID].Windows)
	}
	if byID[bID].Label != "b@x.com" {
		t.Fatalf("B 应显示账户名：%q", byID[bID].Label)
	}

	// ——窗口过期恢复（R008 惰性判过期）——
	p.Store().ObserveWindowSource(aID, "weekly", "429", 100, time.Now().Add(-time.Minute), time.Now())
	if got, _ := pickReq(sid, nil, aID, bID); got == "" {
		t.Fatal("窗口过期后 A 应回池")
	}

	// ——inflight 全配对归零——
	snap := p.Store().Snapshot()
	for id, n := range snap.Inflight {
		if n != 0 {
			t.Fatalf("inflight 未归零：%s=%d", id, n)
		}
	}

	// ——有头客户端 canonical 键绑定（宿主形式 pck:xxx）——
	// usage.handle 带 canonical SessionID → 绑定；pick 经 Options.Metadata
	// canonical_session_id 命中（宿主 conductor_selection.go:1240 传入）。
	rawUsage, _ := json.Marshal(pluginapi.UsageRecord{AuthID: bID, SessionID: "pck:conv-77", Failed: false})
	if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, rawUsage); err != nil {
		t.Fatal(err)
	}
	pickRaw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: ProviderID, Model: "opencode-go/x",
		Options: pluginapi.SchedulerOptions{Metadata: map[string]any{"canonical_session_id": "pck:conv-77"}},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: aID, Provider: ProviderID, Priority: 9},
			{ID: bID, Provider: ProviderID, Priority: 0},
		},
	})
	out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, pickRaw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var pr struct{ AuthID string }
	_ = json.Unmarshal(env.Result, &pr)
	if pr.AuthID != bID {
		t.Fatalf("canonical 键绑定应命中 b（优先级低但绑定）：got %q", pr.AuthID)
	}
}

// countCalls 统计 fakeHost 已收到的某方法调用次数（读快照，有锁）。
func countCalls(host *fakeHost, method string) int {
	n := 0
	for _, c := range host.recorded() {
		if c == method {
			n++
		}
	}
	return n
}

// other 返回两 id 中非 exclude 的那个。
func other(exclude, a, b string) string {
	if exclude == a {
		return b
	}
	return a
}

// deriveSid 用与生产相同的派生链取 sid（首条 user 消息派生）。
func deriveSid(t *testing.T, p *Plugin, hdrs map[string][]string, body []byte) string {
	t.Helper()
	sid := p.resolveSession(hdrs, body)
	if sid == "" {
		t.Fatal("派生 sid 为空")
	}
	if !strings.HasPrefix(sid, "d-") {
		t.Fatalf("应为派生值（d- 前缀）：%q", sid)
	}
	return sid
}
