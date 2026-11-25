package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cpa-plugin-opencode-pool/internal/classifier"
	"cpa-plugin-opencode-pool/internal/session"
	"cpa-plugin-opencode-pool/internal/upstream"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// hostErrPayload mirrors the pluginabi error protocol fields the executor
// returns (dispatch serializes it into the envelope Error).
type hostErrPayload struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped  bool   `json:"credential_scoped,omitempty"`
}

// storage is the plugin-owned auth payload persisted inside auth files.
type storage struct {
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"` // 账户名（邮箱）
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Label   string `json:"label,omitempty"`
}

func parseStorage(raw []byte) storage {
	var s storage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// resolveSession finds the conversation id, preferring already-injected
// headers (BeforeAuth ran first), then body fields, then derivation.
func (p *Plugin) resolveSession(hdrs http.Header, body []byte) string {
	return session.ResolveWithSources(hdrs, body, p.config().SessionHeaderSources)
}

// (endpoint paths live in internal/upstream: /openai/v1/... and
// /anthropic/v1/... under the Go plan base).

// applyOutboundHeaders rewrites User-Agent (delete all case variants, then
// set) and guarantees x-opencode-session presence.
func (p *Plugin) applyOutboundHeaders(hdrs http.Header, body []byte) http.Header {
	out := http.Header{}
	for k, v := range hdrs {
		// 凭据类头一律重建：客户端的 Authorization/x-api-key 不得透传上游
		// （会绕过账号池鉴权或携带外币凭据）。
		if equalFold(k, "User-Agent") || equalFold(k, "x-opencode-session") ||
			equalFold(k, "Authorization") || equalFold(k, "x-api-key") ||
			equalFold(k, "api-key") || equalFold(k, "x-goog-api-key") ||
			equalFold(k, "Cookie") ||
			// 客户端显式带 Accept-Encoding 时 Go 不会自动解压；
			// 交给 host transport 自己协商，避免压缩流破坏 SSE/用量解析。
			equalFold(k, "Accept-Encoding") {
			continue
		}
		out[k] = v
	}
	out.Set("User-Agent", p.config().UpstreamUserAgent)
	if sid := p.resolveSession(hdrs, body); sid != "" {
		out.Set("X-Opencode-Session", sid)
	}
	return out
}

// classifyUpstream applies store effects and returns the host error payload
// for non-2xx statuses (nil for 2xx).
func (p *Plugin) classifyUpstream(authID string, status int, body []byte, hdrs http.Header) *hostErrPayload {
	if status >= 200 && status < 300 {
		// 成功即证明凭据可用：清掉此前疑态（verify_pending 曾永久残留）。
		p.store.SetVerifyPending(authID, false)
		p.store.SetRequiresSignIn(authID, false)
		return nil
	}
	res := classifier.ClassifyWithKeywords(status, body, hdrs, p.config().ConcurrencyKeywords)
	switch res.Category {
	case classifier.CategoryConcurrency:
		p.store.LearnLimit(authID, res.Limit)
	case classifier.CategoryQuota:
		window := res.Window
		if window == "" {
			window = "rolling" // 无窗口词时不写空键
		}
		p.store.ObserveWindowSource(authID, window, "429", 100, res.ResetAt, time.Now())
	case classifier.CategoryAuthInvalid:
		// api_key accounts have no refresh path: a terminal 401 needs sign-in.
		p.store.SetRequiresSignIn(authID, true)
	case classifier.CategoryWAFBlock:
	case classifier.CategoryAuthSuspect:
		p.store.SetVerifyPending(authID, true)
	}
	if res.Category == classifier.CategoryNone {
		// 未分类非 2xx（404/422/3xx…）绝不伪造成 200；用 request_scoped
		// 防止宿主把 404 当成模型问题冷却 12 小时。
		if status == 404 {
			p.InvalidateModelsCache(authID)
		}
		msg := upstreamSnippet(body)
		if msg == "" {
			msg = "upstream rejected request"
		}
		return &hostErrPayload{Code: "request_scoped", Message: msg, HTTPStatus: status}
	}
	he := res.HostError()
	if res.Category == classifier.CategoryTransient {
		he.HTTPStatus = status // 保留 408/500/502/503/504，别统一成 503
	}
	if he.Message == "" {
		he.Message = upstreamSnippet(body)
	}
	if he.Message == "" {
		he.Message = "upstream error " + statusText(status)
	}
	return &hostErrPayload{
		Code: he.Code, Message: he.Message, HTTPStatus: he.HTTPStatus,
		RetryAfterSeconds: he.RetryAfterSeconds, CredentialScoped: he.CredentialScoped,
	}
}

// upstreamSnippet keeps a bounded, human-readable slice of the upstream
// error body so the client sees the real reason instead of "plugin call
// failed". Control characters are collapsed.
func upstreamSnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func statusText(status int) string {
	switch status {
	case 400:
		return "bad request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not found"
	case 429:
		return "rate limited"
	}
	return "status"
}

// countTokens returns a local estimate (upstream has no billing-free count
// endpoint, and the host does not fall back on 501).
func (p *Plugin) countTokens(request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]int{"input_tokens": estimateTokens(req.Payload)})
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// estimateTokens is a coarse ~4 bytes/token approximation of the serialized
// request. It never performs an upstream call.
func estimateTokens(payload []byte) int {
	n := len(payload) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// execute serves executor.execute / execute_stream / count_tokens.
func (p *Plugin) execute(request []byte, stream bool) ([]byte, error) {
	// host 调 execute_stream 时用 rpcExecutorRequest 包装：
	// ExecutorRequest + stream_id + host_callback_id（rpc_schema.go:100-104）。
	var req struct {
		pluginapi.ExecutorRequest
		StreamID       string `json:"stream_id,omitempty"`
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	authID := req.AuthID
	// The pick-time reservation only covers the gap until execution starts.
	// Release it on every path (including early failures) so a malformed
	// account cannot pin the slot until the 90s TTL.
	p.releasePick(authID)
	// 已删除（墓碑）账号：宿主可能因在途/重放仍执行一次，绝不能重建记录。
	if p.store.IsPurged(authID) {
		return errEnvelopeHostBytes(pluginabi.Error{Message: "account deleted", HTTPStatus: 401, CredentialScoped: true}), nil
	}
	// Count this attempt before any early return: the host delivers one usage
	// record per attempt, and a mismatched decrement would eat another live
	// request's slot on the same account.
	p.store.EnsureAccount(authID)
	p.store.InflightInc(authID)
	if p.bridge == nil {
		return errEnvelopeHostBytes(pluginabi.Error{Code: "not_ready", Message: "host bridge unavailable", HTTPStatus: 500}), nil
	}
	st := parseStorage(req.StorageJSON)
	if st.APIKey == "" {
		// 凭据缺 key 是账号问题，不能 request_scoped（宿主会停止换号）；
		// 用 401 + CredentialScoped 让宿主换号，usage 侧会标记需重新登录。
		return errEnvelopeHostBytes(pluginabi.Error{Message: "account has no api_key", HTTPStatus: 401, CredentialScoped: true}), nil
	}

	hdrs := p.applyOutboundHeaders(req.Headers, req.Payload)
	// 上游端点由“请求体格式”(SourceFormat=inputFormat) 决定；Format 是
	// 宿主期望的响应格式。跨格式路由时两者不同，用 Format 会打错端点。
	reqFormat := req.SourceFormat
	if strings.TrimSpace(reqFormat) == "" {
		reqFormat = req.Format
	}
	// OpenCode Go 的 Anthropic 兼容端点（/messages）只认 x-api-key；
	// Chat/Responses 用 Bearer。发错头会被网关判成"requires an API key"。
	if reqFormat == "claude" {
		hdrs.Set("x-api-key", st.APIKey)
		if hdrs.Get("anthropic-version") == "" {
			hdrs.Set("anthropic-version", "2023-06-01")
		}
	} else {
		hdrs.Set("Authorization", "Bearer "+st.APIKey)
	}
	url := upstream.Endpoint(st.BaseURL, reqFormat)

	ctx := context.Background()
	if stream {
		return p.executeStream(ctx, authID, url, hdrs, req.Payload, canonicalSession(req.Metadata), req.StreamID, req.HostCallbackID)
	}

	resp, err := p.bridge.Do(ctx, pluginapi.HTTPRequest{Method: "POST", URL: url, Headers: hdrs, Body: req.Payload}, req.HostCallbackID)
	if err != nil {
		return errEnvelopeHostBytes(pluginabi.Error{Code: "transient_transport", Message: err.Error(), HTTPStatus: 502}), nil
	}
	if he := p.classifyUpstream(authID, resp.StatusCode, resp.Body, resp.Headers); he != nil {
		return errEnvelopeHostBytes(pluginabi.Error{Code: he.Code, Message: he.Message, HTTPStatus: he.HTTPStatus, RetryAfterSeconds: he.RetryAfterSeconds, CredentialScoped: he.CredentialScoped}), nil
	}
	p.bindSession(authID, hdrs, req.Payload, canonicalSession(req.Metadata))
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
}

// canonicalSession extracts the host's canonical session key from executor
// metadata (cliproxyexecutor.CanonicalSessionIDMetadataKey).
func canonicalSession(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	v, _ := meta["canonical_session_id"].(string)
	return v
}

// bindSession records session→auth affinity on upstream acceptance
// (write-on-success). 无头客户端没有宿主 canonical session（usage.handle 的
// SessionID 为空），executor 是它们唯一的绑定写入点；派生键 d- 稳定于首条
// user 消息，b-（body 哈希）逐请求变化无亲和价值，跳过。
func (p *Plugin) bindSession(authID string, hdrs http.Header, body []byte, canonical string) {
	// 有宿主 canonical 键时优先用它（与 usage.handle 的 record.SessionID
	// 同源）；否则退回导出的客户端/派生值。两套键混写会白占 LRU。
	sid := strings.TrimSpace(canonical)
	if sid == "" {
		sid = p.resolveSession(hdrs, body)
	}
	if sid == "" || strings.HasPrefix(sid, "b-") {
		return
	}
	// 宿主对每个凭据周期发健康探测（verify-<auth>-<ts>，一次一变）——
	// 绑定它们只会污染 LRU，跳过。
	if strings.HasPrefix(sid, "verify-") {
		return
	}
	if p.store.IsPurged(authID) {
		return
	}
	p.store.Bind(sid, authID, time.Now())
}

// executeStream serves executor.execute_stream. pluginStreamID 非空时走
// 异步模式（host 约定：RPC 响应 chunks 为空 = 插件稍后经 host.stream.emit
// 推送、host.stream.close 收尾，rpc_client_stream.go:29-47）；否则退回
// 同步数组模式（旧 host 兼容）。
func (p *Plugin) executeStream(ctx context.Context, authID, url string, hdrs http.Header, body []byte, canonical, pluginStreamID, callbackID string) ([]byte, error) {
	status, respHdrs, streamID, err := p.bridge.DoStream(ctx, pluginapi.HTTPRequest{Method: "POST", URL: url, Headers: hdrs, Body: body}, callbackID)
	if err != nil {
		return errEnvelopeHostBytes(pluginabi.Error{Code: "transient_transport", Message: err.Error(), HTTPStatus: 502}), nil
	}
	if status >= 300 {
		// Non-2xx stream start: pull one chunk as the error body. 错误随 RPC
		// 同步返回，host 走换号；host 侧插件流由 host cleanup 收尾。
		payload, _, _, _ := p.bridge.StreamRead(streamID)
		_ = p.bridge.StreamClose(streamID)
		if he := p.classifyUpstream(authID, status, payload, respHdrs); he != nil {
			return errEnvelopeHostBytes(pluginabi.Error{Code: he.Code, Message: he.Message, HTTPStatus: he.HTTPStatus, RetryAfterSeconds: he.RetryAfterSeconds, CredentialScoped: he.CredentialScoped}), nil
		}
		return errEnvelopeHostBytes(pluginabi.Error{Code: "upstream_error", Message: "upstream rejected stream", HTTPStatus: status}), nil
	}
	// 流式 2xx 同样证明凭据可用（旧实现只在非流路径清疑态，
	// verify_pending 会一直挂在面板上）。
	p.classifyUpstream(authID, status, nil, respHdrs)
	if pluginStreamID != "" {
		// 异步模式：先返回 headers（chunks 为空），再后台泵流。
		p.bindSession(authID, hdrs, body, canonical)
		go p.pumpUpstreamStream(pluginStreamID, streamID)
		return okEnvelope(map[string]any{"headers": respHdrs})
	}
	// 同步数组模式：读完所有 chunk 再一次性返回。
	p.bindSession(authID, hdrs, body, canonical)
	var chunks []pluginapi.ExecutorStreamChunk
	for {
		payload, errMsg, done, rerr := p.bridge.StreamRead(streamID)
		if rerr != nil {
			_ = p.bridge.StreamClose(streamID)
			return errEnvelopeHostBytes(pluginabi.Error{Code: "transient_transport", Message: rerr.Error(), HTTPStatus: 502}), nil
		}
		if errMsg != "" {
			_ = p.bridge.StreamClose(streamID)
			// 同步块里的 Err 是 error 接口，宿主无法从 JSON 字符串还原；
			// 用错误信封返回，宿主按流截断失败处理（不换号重试）。
			msg := errMsg
			if len(payload) > 0 {
				msg = errMsg + ": " + upstreamSnippet(payload)
			}
			return errEnvelopeHostBytes(pluginabi.Error{Code: "stream_truncated", Message: msg, HTTPStatus: 502}), nil
		}
		if len(payload) > 0 {
			cp := make([]byte, len(payload))
			copy(cp, payload)
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: cp})
		}
		if done {
			_ = p.bridge.StreamClose(streamID)
			break
		}
	}
	if len(chunks) == 0 {
		// 空 chunks 会被宿主判为异步模式并等待永不出现的 emit/close；
		// 塞一个空块表示同步空流已正常结束。
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{})
	}
	return okEnvelope(map[string]any{"headers": respHdrs, "chunks": chunks})
}

// pumpUpstreamStream forwards upstream chunks to the host-side client stream
// and closes it when done. Mid-stream failures reach the client through
// error/error_detail on host.stream.close.
func (p *Plugin) pumpUpstreamStream(pluginStreamID, upstreamStreamID string) {
	// DLL 场景 panic=宿主全进程崩溃；兜底转成流错误并尽力释放上游。
	defer func() {
		if r := recover(); r != nil {
			_ = p.bridge.StreamClose(upstreamStreamID)
			msg := fmt.Sprintf("pump panic: %v", r)
			p.bridge.FinishStream(pluginStreamID, msg, &pluginabi.Error{
				Code: "stream_truncated", Message: msg, HTTPStatus: 502,
			})
		}
	}()
	for {
		payload, errMsg, done, rerr := p.bridge.StreamRead(upstreamStreamID)
		if rerr != nil {
			_ = p.bridge.StreamClose(upstreamStreamID)
			// 已经进入流式阶段：不是 HTTP 建立前的拨号失败，按流截断处理，
			// 宿主不会换号重放（避免重复生成）。
			p.bridge.FinishStream(pluginStreamID, rerr.Error(), &pluginabi.Error{
				Code: "stream_truncated", Message: rerr.Error(), HTTPStatus: 502,
			})
			return
		}
		if errMsg != "" {
			_ = p.bridge.StreamClose(upstreamStreamID)
			msg := errMsg
			if len(payload) > 0 { // 中途错误的响应体常含原因，别丢
				msg = errMsg + ": " + upstreamSnippet(payload)
			}
			// stream_truncated: 生成已提交上游，宿主不得换号重试、也不冷却凭据。
			p.bridge.FinishStream(pluginStreamID, msg, &pluginabi.Error{
				Code: "stream_truncated", Message: msg, HTTPStatus: 502,
			})
			return
		}
		if len(payload) > 0 {
			if err := p.bridge.EmitStream(pluginStreamID, payload); err != nil {
				_ = p.bridge.StreamClose(upstreamStreamID)
				// 非取消类失败（回调实例重建等）也必须收尾，否则宿主流挂起。
				p.bridge.FinishStream(pluginStreamID, "stream emit failed: "+err.Error(), &pluginabi.Error{
					Code: "connection_lifecycle", Message: err.Error(), HTTPStatus: 502,
				})
				return
			}
		}
		if done {
			_ = p.bridge.StreamClose(upstreamStreamID)
			p.bridge.FinishStream(pluginStreamID, "", nil)
			return
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// errEnvelopeHostBytes serializes a host-protocol error envelope.
func errEnvelopeHostBytes(e pluginabi.Error) []byte {
	raw, _ := json.Marshal(abiEnvelope{OK: false, Error: &e})
	return raw
}

var _ = strings.TrimSpace
