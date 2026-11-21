package plugin

import (
	"bytes"
	"crypto/sha256"

	"cpa-plugin-opencode-pool/internal/panel"
	"cpa-plugin-opencode-pool/internal/store"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// managementRegister declares the panel routes under /v0/management/.
func (p *Plugin) managementRegister() ([]byte, error) {
	// 宿主按 method+path 精确匹配且拒绝 ":"/"*"（pluginhost/management.go:151,
	// :237-240），动态 {id} 路由永不命中——单账号操作全部走固定路径，
	// id 放 query 或 body（v1.0.5）。
	routes := []map[string]any{
		{"method": "GET", "path": "/plugins/" + PluginID + "/accounts"},
		{"method": "POST", "path": "/plugins/" + PluginID + "/accounts"},
		{"method": "GET", "path": "/plugins/" + PluginID + "/account"},
		{"method": "PATCH", "path": "/plugins/" + PluginID + "/account"},
		{"method": "DELETE", "path": "/plugins/" + PluginID + "/account"},
		{"method": "POST", "path": "/plugins/" + PluginID + "/account/refresh"},
		{"method": "POST", "path": "/plugins/" + PluginID + "/account/unblock"},
		{"method": "POST", "path": "/plugins/" + PluginID + "/account/bindings/clear"},
		{"method": "GET", "path": "/plugins/" + PluginID + "/settings"},
		{"method": "PUT", "path": "/plugins/" + PluginID + "/settings"},
		{"method": "GET", "path": "/plugins/" + PluginID + "/export"},
		{"method": "POST", "path": "/plugins/" + PluginID + "/import"},
	}
	resources := []map[string]any{
		{"path": "/status", "menu": "OpenCode"},
	}
	return okEnvelope(map[string]any{"routes": routes, "resources": resources})
}

type mgmtResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func mgmtJSON(v any) mgmtResponse {
	raw, _ := json.Marshal(v)
	return mgmtResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: raw}
}

func mgmtError(status int, code, message string) mgmtResponse {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return mgmtResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: raw}
}

// statusString mirrors the §9 six-color dot derivation as a string.
func statusString(acc accountView) string {
	for _, w := range acc.Windows {
		if w.UsedPct >= 100 && time.Now().Before(mustTime(w.Until)) {
			return "exhausted"
		}
	}
	for _, w := range acc.Windows {
		if w.UsedPct >= 92 {
			return "near_limit"
		}
	}
	if acc.RequiresSignIn {
		return "sign_in_required"
	}
	if acc.VerifyPending {
		return "verify_pending"
	}
	if !acc.Enabled {
		return "disabled"
	}
	return "active"
}

type windowView struct {
	Window  string  `json:"window"`
	UsedPct float64 `json:"used_pct"`
	Until   string  `json:"until,omitempty"`
	Source  string  `json:"source,omitempty"`
}

type accountView struct {
	ID                      string         `json:"id"`
	Label                   string         `json:"label,omitempty"`
	APIKeyMasked            string         `json:"api_key_masked,omitempty"`
	Enabled                 bool           `json:"enabled"`
	Status                  string         `json:"status"`
	Windows                 []windowView   `json:"windows,omitempty"`
	Inflight                int            `json:"inflight"`
	LearnedConcurrencyLimit int            `json:"learned_concurrency_limit,omitempty"`
	Usage                   usageView      `json:"usage"`
	RequiresSignIn          bool           `json:"requires_sign_in,omitempty"`
	VerifyPending           bool           `json:"verify_pending,omitempty"`
	LastError               *lastError     `json:"last_error,omitempty"`
	BindingsCount           int            `json:"bindings_count,omitempty"`
	History                 []historyEntry `json:"history,omitempty"`
}

type usageView struct {
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
}

type lastError struct {
	Code string `json:"code"`
	At   string `json:"at"`
}

type historyEntry struct {
	At   string `json:"at"`
	Code string `json:"code"`
}

// HandleManagement serves one management request (test seam).
func (p *Plugin) HandleManagement(method, path string, query url.Values, body io.Reader) mgmtResponse {
	return p.managementServe(method, path, query, body)
}

func (p *Plugin) managementHandle(request []byte) ([]byte, error) {
	// 宿主 wire 用 SDK 结构体 Go 字段名（无 json tags）：Method/Path/Query/
	// Body；Body 为 base64 字符串，[]byte 接收端自动解码（deep-check P0）。
	var req struct {
		Method string
		Path   string
		Query  url.Values
		Body   []byte
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	resp := p.managementServe(req.Method, req.Path, req.Query, bytes.NewReader(req.Body))
	return okEnvelope(struct {
		StatusCode int
		Headers    http.Header
		Body       []byte
	}{resp.StatusCode, resp.Headers, resp.Body})
}

func (p *Plugin) managementServe(method, path string, query url.Values, body io.Reader) mgmtResponse {
	// resource 外壳走独立前缀（/v0/resource/plugins/<id>/status）。
	if strings.HasPrefix(path, "/v0/resource/plugins/"+PluginID) {
		path = strings.TrimPrefix(path, "/v0/resource/plugins/"+PluginID)
		if method == "GET" && strings.TrimRight(path, "/") == "/status" {
			return mgmtResponse{StatusCode: 200,
				Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:    panel.HTML}
		}
		return mgmtError(404, "invalid_request", "no such resource")
	}
	path = strings.TrimPrefix(path, "/v0/management")
	path = strings.TrimPrefix(path, "/plugins/"+PluginID)
	var payload []byte
	if body != nil {
		payload, _ = io.ReadAll(body)
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	_ = seg

	switch {
	case method == "GET" && path == "/status":
		// 兼容 management 前缀下直达面板（resource 注册的规范路径见上）。
		return mgmtResponse{StatusCode: 200,
			Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:    panel.HTML}

	case method == "GET" && path == "/accounts":
		snap := p.store.Snapshot()
		views := make([]accountView, 0, len(snap.Accounts))
		for id, acc := range snap.Accounts {
			p.refreshUsage(id) // 后台拉 /usage 填充窗口（节流），下次轮询可见
			views = append(views, p.viewOf(id, acc, snap, false))
		}
		return mgmtJSON(map[string]any{"accounts": views})

	case method == "GET" && path == "/account":
		id := queryID(query, payload, true)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		snap := p.store.Snapshot()
		acc, ok := snap.Accounts[id]
		if !ok {
			return mgmtError(404, "account_not_found", "no such account")
		}
		p.refreshUsage(id)
		return mgmtJSON(p.viewOf(id, acc, snap, true))

	case method == "POST" && path == "/accounts":
		var in struct {
			Name         string `json:"name"`
			APIKey       string `json:"api_key"`
			OpexportJSON string `json:"opexport_json"`
		}
		if err := json.Unmarshal(payload, &in); err != nil {
			return mgmtError(400, "invalid_request", "undecodable body")
		}
		key := in.APIKey
		if key == "" && in.OpexportJSON != "" {
			var oe struct {
				APIKey string `json:"api_key"`
			}
			_ = json.Unmarshal([]byte(in.OpexportJSON), &oe)
			key = oe.APIKey
		}
		if key == "" {
			return mgmtError(400, "invalid_request", "api_key required")
		}
		id, err := p.materializeAccount(in.Name, key)
		if err != nil {
			return mgmtError(502, "upstream_error", err.Error())
		}
		snap := p.store.Snapshot()
		return mgmtJSON(p.viewOf(id, snap.Accounts[id], snap, false))

	case method == "GET" && path == "/settings":
		return mgmtJSON(p.config())

	case method == "PUT" && path == "/settings":
		cfg, err := p.updateSettings(payload)
		if err != nil {
			return mgmtError(400, "invalid_request", "undecodable body")
		}
		return mgmtJSON(cfg)

	case method == "GET" && path == "/export":
		accounts := []map[string]string{}
		if p.bridge != nil {
			files, _ := p.bridge.AuthList()
			for _, f := range files {
				raw, err := p.bridge.AuthGet(f.ID)
				if err != nil {
					continue
				}
				st := parseStorage(raw)
				if st.Type != "opencode-go" || st.APIKey == "" {
					continue
				}
				accounts = append(accounts, map[string]string{
					"id": f.ID, "name": st.Name, "label": st.Label, "api_key": st.APIKey,
				})
			}
		}
		return mgmtJSON(map[string]any{"settings": p.config(), "accounts": accounts})

	case method == "POST" && path == "/import":
		var in struct {
			Settings *Config `json:"settings"`
			Accounts []struct {
				Name   string `json:"name"`
				APIKey string `json:"api_key"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(payload, &in); err != nil {
			return mgmtError(400, "invalid_request", "undecodable body")
		}
		if in.Settings != nil {
			raw, _ := json.Marshal(in.Settings)
			if _, err := p.updateSettings(raw); err != nil {
				return mgmtError(400, "invalid_request", "bad settings")
			}
		}
		if len(in.Accounts) > maxImportAccounts {
			return mgmtError(400, "invalid_request", "too many accounts in one import")
		}
		imported := 0
		for _, a := range in.Accounts {
			if a.APIKey == "" {
				continue
			}
			if _, err := p.materializeAccount(a.Name, a.APIKey); err != nil {
				return mgmtError(502, "upstream_error", err.Error())
			}
			imported++
		}
		return mgmtJSON(map[string]any{"imported": imported})

	case method == "PATCH" && path == "/account":
		var in struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(payload, &in); err != nil || in.Enabled == nil {
			return mgmtError(400, "invalid_request", "id and enabled required")
		}
		if in.ID == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		p.store.SetEnabled(in.ID, *in.Enabled)
		return mgmtJSON(map[string]any{"id": in.ID, "enabled": *in.Enabled})

	case method == "POST" && path == "/account/unblock":
		id := queryID(query, payload, false)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		p.store.SetRequiresSignIn(id, false)
		p.store.SetVerifyPending(id, false)
		p.store.ClearWindows(id)
		if p.bridge != nil {
			_ = p.bridge.ResetCooldown(id)
		}
		return mgmtJSON(map[string]any{"id": id, "recovered": true})

	case method == "POST" && path == "/account/refresh":
		id := queryID(query, payload, false)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		return mgmtJSON(map[string]any{"id": id, "refreshed": true})

	case method == "POST" && path == "/account/bindings/clear":
		id := queryID(query, payload, false)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		n := p.store.ClearBindings(id)
		return mgmtJSON(map[string]any{"id": id, "cleared": n})

	case method == "DELETE" && path == "/account":
		id := queryID(query, nil, true)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		if query != nil && query.Get("purge") == "true" {
			// 硬删插件侧状态；host auth 文件归 host 管（ABI 无删除回调）。
			p.store.SetEnabled(id, false)
			p.store.PurgeAccount(id)
			if p.bridge != nil {
				_ = p.bridge.ResetCooldown(id)
			}
			return mgmtJSON(map[string]any{"id": id, "deleted": true, "purged": true, "host_file": "retained"})
		}
		p.store.SetEnabled(id, false)
		return mgmtJSON(map[string]any{"id": id, "deleted": true})
	}
	return mgmtError(404, "invalid_request", "no such route")
}

// maxImportAccounts bounds one import batch (DoS guard).
const maxImportAccounts = 1000

// queryID resolves the account id from the query string first, then a JSON
// body {"id": "..."} (fixed-path routes carry dynamic ids out of the path).
func queryID(query url.Values, payload []byte, queryOnly bool) string {
	if query != nil {
		if id := strings.TrimSpace(query.Get("id")); id != "" {
			return id
		}
	}
	if queryOnly {
		return ""
	}
	var in struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(payload, &in)
	return strings.TrimSpace(in.ID)
}

// materializeAccount persists one account (name+api_key) via host AuthSave
// and registers it in the store; the id is the key fingerprint, so repeats
// are idempotent upserts.
func (p *Plugin) materializeAccount(name, key string) (string, error) {
	label := name
	if label == "" {
		label = maskKey(key)
	}
	st := storage{Type: "opencode-go", Name: name, APIKey: key, Label: label}
	raw, _ := json.Marshal(st)
	sum := sha256.Sum256([]byte(key))
	id := ProviderID + ":" + hex.EncodeToString(sum[:])[:12]
	if p.bridge != nil {
		if err := p.bridge.AuthSave(pluginapi.AuthData{
			Provider: ProviderID, ID: id,
			FileName: "opencode-" + hex.EncodeToString(sum[:])[:8] + ".json",
			Label:    st.Label, StorageJSON: raw,
		}); err != nil {
			return "", err
		}
	}
	p.store.EnsureAccount(id)
	return id, nil
}

func (p *Plugin) viewOf(id string, acc *storeAccount, snap store.Snapshot, detail bool) accountView {
	if acc == nil {
		// 并发 purge 后快照可能已无此账号；跨 cgo panic 会拖垮宿主进程。
		acc = &storeAccount{}
	}
	label, masked := p.authInfoFor(id) // 单次 AuthGet 取 label+key（deep-check：列表 N 账号不再 2N 次 FFI）
	v := accountView{
		ID: id, Label: label, APIKeyMasked: masked, Enabled: !acc.Disabled,
		Inflight:                snap.Inflight[id],
		LearnedConcurrencyLimit: acc.LearnedConcurrencyLimit,
		RequiresSignIn:          acc.RequiresSignIn,
		VerifyPending:           acc.VerifyPending,
	}
	for name, w := range acc.Windows {
		v.Windows = append(v.Windows, windowView{Window: name, UsedPct: w.UsedPct, Until: w.Until.UTC().Format(time.RFC3339), Source: w.Source})
	}
	v.Usage = usageView{Requests: acc.Usage.Requests, InputTokens: acc.Usage.InputTokens, OutputTokens: acc.Usage.OutputTokens}
	if acc.Usage.LastUsedAt != nil {
		v.Usage.LastUsedAt = acc.Usage.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if acc.LastError != nil {
		v.LastError = &lastError{Code: acc.LastError.Code, At: acc.LastError.At.UTC().Format(time.RFC3339)}
	}
	if detail {
		v.BindingsCount = snap.BindingsFor(id)
		for _, h := range acc.History {
			v.History = append(v.History, historyEntry{At: h.At.UTC().Format(time.RFC3339), Code: h.Code})
		}
	}
	v.Status = statusString(v)
	return v
}

// 类型别名：指向 store 的真实类型
type storeAccount = store.Account

func mustTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// maskedKeyFor fetches the account's api key for masked display.
// authInfoFor reads one auth document and returns (label, maskedKey).
func (p *Plugin) authInfoFor(id string) (string, string) {
	if p.bridge == nil {
		return labelFromID(id), ""
	}
	raw, err := p.bridge.AuthGet(id)
	if err != nil {
		return labelFromID(id), ""
	}
	st := parseStorage(raw)
	label := st.Label
	if label == "" {
		label = labelFromID(id)
	}
	return label, maskKey(st.APIKey)
}

// labelFromID derives a readable label from an auth id when nothing better
// is stored.
func labelFromID(id string) string {
	if tail := strings.SplitN(id, ":", 2); len(tail) == 2 && len(tail[1]) >= 4 {
		return tail[1][:4] + "…" + tail[1][len(tail[1])-4:]
	}
	return id
}

// refreshUsage pulls /usage in the background (throttled to one call per
// account per minute) so window bars appear without waiting for a 429.
func (p *Plugin) refreshUsage(id string) {
	if p.bridge == nil {
		return
	}
	p.usageFetchMu.Lock()
	if p.usageFetched == nil {
		p.usageFetched = map[string]time.Time{}
	}
	if t, ok := p.usageFetched[id]; ok && time.Since(t) < time.Minute {
		p.usageFetchMu.Unlock()
		return
	}
	p.usageFetched[id] = time.Now()
	p.usageFetchMu.Unlock()
	go func() {
		raw, err := p.bridge.AuthGet(id)
		if err != nil {
			return
		}
		st := parseStorage(raw)
		if st.APIKey == "" {
			return
		}
		p.fetchAndApplyUsage(id, st.APIKey, st.BaseURL)
	}()
}
