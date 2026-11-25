package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"cpa-plugin-opencode-pool/internal/scheduler"

	"cpa-plugin-opencode-pool/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type abiEnvelope struct {
	OK     bool             `json:"ok"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *pluginabi.Error `json:"error,omitempty"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(abiEnvelope{OK: true, Result: raw})
}

func errEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(abiEnvelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

// executorError carries the host error protocol fields through dispatch.
type executorError struct{ he hostErrPayload }

func (e executorError) Error() string { return e.he.Message }

// HandleMethod dispatches one host RPC (the ABI entry in cmd/ calls this).
func (p *Plugin) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return p.handleLifecycle(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.shutdown()
		return okEnvelope(map[string]any{})
	case pluginabi.MethodRequestInterceptBefore:
		return p.interceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		// 响应阶段不修改（宿主对同一插件两相都调；空 ok 避免 unknown_method 噪音）
		return okEnvelope(map[string]any{})
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case pluginabi.MethodExecutorExecute:
		return p.execute(request, false)
	case pluginabi.MethodExecutorExecuteStream:
		return p.execute(request, true)
	case pluginabi.MethodExecutorHTTPRequest:
		// 本插件不走宿主 HTTP 转发执行器路径（上游调用经 host.http.do）。
		return errEnvelopeHostBytes(pluginabi.Error{Code: "not_implemented", Message: "executor.http_request not used", HTTPStatus: 501}), nil
	case pluginabi.MethodExecutorCountTokens:
		// 上游无 count 端点，真实转发等于一次计费生成；这里本地估算。
		// 宿主对 501 不会回退估算（conductor_execution.go 直接上抛），
		// 所以必须自己返回一个 input_tokens 响应。
		return p.countTokens(request)
	case pluginabi.MethodSchedulerPick:
		return p.pick(request)
	case pluginabi.MethodUsageHandle:
		return p.usageHandle(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case pluginabi.MethodAuthParse:
		return p.authParse(request)
	case pluginabi.MethodAuthRefresh:
		return p.authRefresh(request)
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return errEnvelope("not_implemented", "interactive login arrives in a later milestone; import keys via the panel", 400), nil
	case pluginabi.MethodModelStatic:
		return p.modelsStatic(request)
	case pluginabi.MethodModelForAuth:
		return p.modelsForAuth(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(map[string]any{"supported_providers": []string{ProviderID}, "display_name": "OpenCode Go", "supports_reset": false})
	case pluginabi.MethodQuotaFetch:
		return p.quotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return okEnvelope(map[string]any{"success": false, "message": "reset not supported"})
	case pluginabi.MethodManagementRegister:
		return p.managementRegister()
	case pluginabi.MethodManagementHandle:
		return p.managementHandle(request)
	}
	return errEnvelope("unknown_method", "unknown method: "+method, 0), nil
}

func (p *Plugin) handleLifecycle(request []byte) ([]byte, error) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	if err := p.applyConfig(req.ConfigYAML); err != nil {
		return nil, err
	}
	p.restoreState()
	p.reconcileWithHost()
	return okEnvelope(p.buildRegistration())
}

// restoreState adopts a persisted state.json (windows/bindings/learned
// limits) and starts the auto-flush loop; without it restarts lose all
// runtime state (deep-check P1: Flush had no production call site).
func (p *Plugin) restoreState() {
	p.mu.Lock()
	dir := p.dataDir
	// reconfigure 会再次走 register 生命周期。此时必须复用运行中的 store：
	// 重新 Load+Adopt 会让旧 store 的 flush goroutine 继续用旧快照覆盖
	// state.json，而新 store 没有 flush 循环，运行期数据会丢。
	restored := p.stopFlush != nil
	p.mu.Unlock()
	if dir == "" || restored {
		return
	}
	if s, err := LoadStore(filepath.Join(dir, "state.json")); err == nil {
		p.AdoptStore(s)
	} else {
		p.store = store.New(dir) // fresh store bound to the same dir
	}
	p.mu.Lock()
	if p.stopFlush == nil {
		p.stopFlush = p.store.StartAutoFlush(autoFlushInterval)
	}
	p.mu.Unlock()
}

// Shutdown stops the auto-flush loop (final Flush lands synchronously).
// Exported for the ABI shutdown export.
func (p *Plugin) Shutdown() { p.shutdown() }

// shutdown stops the auto-flush loop (final Flush lands synchronously).
func (p *Plugin) shutdown() {
	p.mu.Lock()
	stop := p.stopFlush
	p.stopFlush = nil
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// reconcileWithHost aligns store accounts with host auth files.
func (p *Plugin) reconcileWithHost() {
	if p.bridge == nil {
		return
	}
	files, err := p.bridge.AuthList()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, f := range files {
		// 宿主启动时 register 早于 SetAuthManager，auth.list 走磁盘回退：
		// 条目 ID/AuthIndex 为空。空 id 绝不能当作“唯一存活账号”，否则
		// Reconcile 会清空刚载入的全部窗口/绑定/学习上限（真实启动路径）。
		if f.Provider != ProviderID || strings.TrimSpace(f.ID) == "" {
			continue
		}
		live[f.ID] = true
	}
	if len(live) == 0 {
		return // auth manager not ready (or genuinely empty): keep persisted state
	}
	p.store.Reconcile(live)
}

// interceptBefore injects the resolved/derived session header before
// credential selection (verified chain: handlers_execution.go:110→116).
func (p *Plugin) interceptBefore(request []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if req.Headers == nil {
		req.Headers = http.Header{}
	}
	sid := p.resolveSession(req.Headers, req.Body)
	if sid == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// Host merges returned headers over the current request (mergeHeaders),
	// so returning only the injected session header is correct and preserves
	// every other client header.
	hdrs := http.Header{}
	hdrs.Set("X-Opencode-Session", sid)
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: hdrs})
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// pick implements scheduler.pick over the store.
func (p *Plugin) pick(request []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	// 宿主对所有 provider 都调用本插件的选号。请求不属于本池时直接让权，
	// 由宿主内置选号处理 claude/openai 等模型。
	if req.Provider != "" && req.Provider != ProviderID {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	cands := make([]scheduler.Candidate, 0, len(req.Candidates))
	ownIDs := make([]string, 0, len(req.Candidates))
	foreign := 0
	live := map[string]bool{}
	for _, c := range req.Candidates {
		// 只在本插件 provider 的凭据里选号；跨 provider 候选（若有）不得被
		// 插件返回给宿主，否则可能把别家账号当成本池账号使用。
		if c.Provider != "" && c.Provider != ProviderID {
			foreign++
			continue
		}
		if c.Provider == "" && req.Provider != ProviderID {
			// 候选缺 Provider（老宿主）：只有请求明确是本池时才敢认领，
			// mixed 请求下不能把他家账号当本池。
			foreign++
			continue
		}
		if c.ID == "" {
			continue
		}
		cands = append(cands, scheduler.Candidate{ID: c.ID, Priority: c.Priority})
		ownIDs = append(ownIDs, c.ID)
		live[c.ID] = true
	}
	// 没有本池候选（例如 claude 模型）：必须让权。返回 Reject 会让宿主
	// 直接 503 auth_unavailable，把其他 provider 的请求一起打死。
	if len(cands) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	sid := ""
	// 绑定键优先用宿主 canonical session（Options.Metadata["canonical_session_id"]，
	// 宿主形式 pck:/claude:/codex: 等，与 usage.handle 的 record.SessionID 同源）；
	// 无 canonical 的无头客户端退回 BeforeAuth 注入的派生头（与 executor 成功
	// 路径的 Bind 同源）。两个键空间不得混用。
	if v, ok := req.Options.Metadata["canonical_session_id"].(string); ok {
		sid = v
	}
	if sid == "" && req.Options.Headers != nil {
		for k, v := range req.Options.Headers {
			if strings.EqualFold(k, "x-opencode-session") && len(v) > 0 {
				sid = v[0]
				break
			}
		}
	}
	p.pickSeqMu.Lock()
	snap := p.store.SnapshotScheduling()
	dec := scheduler.Pick(scheduler.Input{Candidates: cands, SessionID: sid}, storeDeps{p: p, cands: live, snap: &snap})
	if dec.Handled && !dec.Reject && dec.AuthID != "" {
		p.reservePick(dec.AuthID) // smooth bursts before executor inflight++
		p.noteSelection(dec.AuthID)
	}
	p.pickSeqMu.Unlock()
	// mixed 请求里本池全坏、但他家候选健康：不能 Reject（宿主会直接
	// 终止整个请求），改为让权并排除本池坏号，由宿主路由到其他 provider。
	if dec.Handled && dec.Reject && foreign > 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false, ExcludeCandidates: ownIDs})
	}
	resp := pluginapi.SchedulerPickResponse{
		AuthID:       dec.AuthID,
		Handled:      dec.Handled,
		Reject:       dec.Reject,
		RejectReason: dec.RejectReason,
	}
	if dec.Reject {
		resp.RejectCode = "auth_unavailable"
	}
	return okEnvelope(resp)
}

var _ = fmt.Sprintf
