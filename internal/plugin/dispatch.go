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
		// 上游无 count 端点；真实转发等于一次计费生成，且宿主 count_tokens
		// 适配器无 usage.handle（inflight 永久泄漏，deep-check P1）。诚实返回
		// not_implemented，宿主回退本地估算。
		return errEnvelopeHostBytes(pluginabi.Error{Code: "not_implemented", Message: "opencode-go has no count_tokens endpoint", HTTPStatus: 501}), nil
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
	p.mu.Unlock()
	if dir == "" {
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
		if f.Provider == ProviderID {
			live[f.ID] = true
		}
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
	hdrs := http.Header{}
	for k := range req.Headers {
		if equalFold(k, "x-opencode-session") {
			continue
		}
	}
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
	cands := make([]scheduler.Candidate, 0, len(req.Candidates))
	live := map[string]bool{}
	for _, c := range req.Candidates {
		cands = append(cands, scheduler.Candidate{ID: c.ID, Priority: c.Priority})
		live[c.ID] = true
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
	dec := scheduler.Pick(scheduler.Input{Candidates: cands, SessionID: sid}, storeDeps{p: p, cands: live})
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
