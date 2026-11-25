package plugin

import (
	"bytes"
	"crypto/sha256"

	"cpa-plugin-opencode-pool/internal/panel"
	"cpa-plugin-opencode-pool/internal/store"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
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

// statusString mirrors the §9 six-color dot derivation as a string and must
// stay in the same priority order as the panel's state() function.
func statusString(acc accountView, nearLimitPct float64) string {
	now := time.Now()
	if nearLimitPct <= 0 || nearLimitPct >= 100 {
		nearLimitPct = 8
	}
	nearAt := 100 - nearLimitPct
	if !acc.Enabled {
		return "disabled"
	}
	for _, w := range acc.Windows {
		if w.UsedPct >= 100 && now.Before(mustTime(w.Until)) {
			return "exhausted"
		}
	}
	if acc.RequiresSignIn {
		return "sign_in_required"
	}
	if acc.VerifyPending {
		return "verify_pending"
	}
	for _, w := range acc.Windows {
		if w.UsedPct >= nearAt && now.Before(mustTime(w.Until)) {
			return "near_limit"
		}
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
		auths := p.hostAuthEntries()
		counts := p.store.BindingCounts(time.Now())
		views := make([]accountView, 0, len(snap.Accounts))
		for id, acc := range snap.Accounts {
			if p.store.IsPurged(id) {
				p.store.PurgeAccount(id)
				continue
			}
			entry, inHost := auths[id]
			// 误入的其他 provider 账号（例如 claude:apikey:*）必须清理：
			// 宿主列表里存在但 provider 不同，或宿主根本不认识这个 id。
			if inHost && foreignAccount(id, auths) {
				p.store.PurgeAccount(id)
				continue
			}
			if auths != nil && !inHost && !strings.HasPrefix(id, ProviderID+":") {
				// 宿主列表回退（ID 为空）或条目缺失时，“不认识”不是权威结论：
				// 不能留下永久墓碑，否则之后合法的同名凭据会被挡住。
				p.store.Forget(id)
				continue
			}
			p.refreshUsage(id, entry.AuthIndex) // 后台拉 /usage，节流
			views = append(views, p.viewOf(id, acc, snap, false, counts[id]))
		}
		// 稳定排序：map 迭代顺序随机会让前端 JSON 去重永不命中，5s 全量重建。
		sort.Slice(views, func(i, j int) bool {
			li, lj := views[i].Label, views[j].Label
			if li == "" {
				li = views[i].ID
			}
			if lj == "" {
				lj = views[j].ID
			}
			if li != lj {
				return li < lj
			}
			return views[i].ID < views[j].ID
		})
		return mgmtJSON(map[string]any{"accounts": views})

	case method == "GET" && path == "/account":
		id := queryID(query, payload, true)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		if p.store.IsPurged(id) {
			return mgmtError(404, "account_not_found", "no such account")
		}
		snap := p.store.Snapshot()
		acc, ok := snap.Accounts[id]
		if !ok {
			return mgmtError(404, "account_not_found", "no such account")
		}
		p.refreshUsage(id, p.hostAuthEntries()[id].AuthIndex)
		return mgmtJSON(p.viewOf(id, acc, snap, true, p.store.BindingCounts(time.Now())[id]))

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
		return mgmtJSON(p.viewOf(id, snap.Accounts[id], snap, false, p.store.BindingCounts(time.Now())[id]))

	case method == "GET" && path == "/settings":
		return mgmtJSON(p.config())

	case method == "PUT" && path == "/settings":
		cfg, err := p.updateSettings(payload)
		if err != nil {
			if errors.Is(err, errSettingsPersist) {
				return mgmtError(500, "settings_persist_failed", err.Error())
			}
			return mgmtError(400, "invalid_request", "undecodable body")
		}
		return mgmtJSON(cfg)

	case method == "GET" && path == "/export":
		accounts := []map[string]string{}
		if p.bridge != nil {
			files, _ := p.bridge.AuthList()
			for _, f := range files {
				if f.Provider != ProviderID || f.AuthIndex == "" || p.store.IsPurged(f.ID) {
					continue
				}
				raw, err := p.bridge.AuthGet(f.AuthIndex)
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
		// 先校验数量再产生任何副作用：旧的顺序会先落盘 settings，
		// 然后才因超限返回 400。
		if len(in.Accounts) > maxImportAccounts {
			return mgmtError(400, "invalid_request", "too many accounts in one import")
		}
		if in.Settings != nil {
			raw, _ := json.Marshal(in.Settings)
			if _, err := p.updateSettings(raw); err != nil {
				if errors.Is(err, errSettingsPersist) {
					return mgmtError(500, "settings_persist_failed", err.Error())
				}
				return mgmtError(400, "invalid_request", "bad settings")
			}
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
		if !p.accountExists(in.ID) {
			return mgmtError(404, "account_not_found", "no such account")
		}
		p.store.SetEnabled(in.ID, *in.Enabled)
		return mgmtJSON(map[string]any{"id": in.ID, "enabled": *in.Enabled})

	case method == "POST" && path == "/account/unblock":
		id := queryID(query, payload, false)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		if !p.accountExists(id) {
			return mgmtError(404, "account_not_found", "no such account")
		}
		// 先清宿主冷却再清本地状态：宿主调用失败时不能谎报已恢复。
		if p.bridge != nil {
			entry, ok := p.hostAuthEntries()[id]
			if !ok {
				return mgmtError(409, "host_auth_missing", "host credential not found")
			}
			if err := p.bridge.ResetCooldown(entry.AuthIndex); err != nil {
				return mgmtError(502, "reset_failed", err.Error())
			}
		}
		p.store.SetRequiresSignIn(id, false)
		p.store.SetVerifyPending(id, false)
		p.store.ClearWindows(id)
		return mgmtJSON(map[string]any{"id": id, "recovered": true})

	case method == "POST" && path == "/account/refresh":
		id := queryID(query, payload, false)
		if id == "" {
			return mgmtError(400, "invalid_request", "id required")
		}
		entry, ok := p.hostAuthEntries()[id]
		if !ok {
			return mgmtError(404, "account_not_found", "no such account")
		}
		p.forceRefreshUsage(id, entry.AuthIndex)
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
		if !p.accountExists(id) && !(query != nil && query.Get("purge") == "true" && p.store.IsPurged(id)) {
			return mgmtError(404, "account_not_found", "no such account")
		}
		if query != nil && query.Get("purge") == "true" {
			// 硬删插件侧状态并留墓碑；host auth 文件归 host 管（ABI 无删除
			// 回调），墓碑保证重启/usage 事件都不会把它带回来。
			if p.bridge != nil {
				if entry, ok := p.hostAuthEntries()[id]; ok {
					_ = p.bridge.ResetCooldown(entry.AuthIndex)
				}
			}
			p.store.PurgeAccount(id)
			return mgmtJSON(map[string]any{"id": id, "deleted": true, "purged": true, "host_file": "retained"})
		}
		p.store.SetEnabled(id, false)
		return mgmtJSON(map[string]any{"id": id, "deleted": true})
	}
	return mgmtError(404, "invalid_request", "no such route")
}

// maxImportAccounts bounds one import batch (DoS guard).
const maxImportAccounts = 1000

// accountExists reports whether the id is a live plugin account or a host
// credential. Mutating endpoints must not EnsureAccount unknown ids: a typo
// used to create phantom accounts.
func (p *Plugin) accountExists(id string) bool {
	if id == "" || p.store.IsPurged(id) {
		return false
	}
	if _, ok := p.store.Snapshot().Accounts[id]; ok {
		return true
	}
	// 宿主凭据必须属于本插件 provider：PATCH/DELETE 不能对 claude 等其他
	// 账号创建禁用记录或墓碑。
	entry, ok := p.hostAuthEntries()[id]
	if !ok {
		return false
	}
	provider := entry.Provider
	if provider == "" {
		provider = entry.Type
	}
	return provider == ProviderID
}

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
	sum := sha256.Sum256([]byte(key))
	id := ProviderID + ":" + hex.EncodeToString(sum[:])[:12]
	fileName := "opencode-" + hex.EncodeToString(sum[:])[:8] + ".json"
	// 同 key 重新添加/导入时必须保留宿主 auth 文件里的其他字段
	// （base_url / priority / proxy_url / 备注等）。AuthSave 是整文件覆盖，
	// 旧实现只写 4 个字段，重复添加会把运营配置抹掉。
	doc := map[string]any{}
	if p.bridge != nil {
		for _, f := range p.hostAuthEntries() {
			if f.Provider != ProviderID || (f.ID != id && f.Name != fileName) || f.AuthIndex == "" {
				continue
			}
			if old, err := p.bridge.AuthGet(f.AuthIndex); err == nil && len(old) > 0 {
				_ = json.Unmarshal(old, &doc)
			}
			break
		}
	}
	doc["type"] = ProviderID
	doc["name"] = name
	doc["api_key"] = key
	doc["label"] = label
	raw, _ := json.Marshal(doc)
	if p.bridge != nil {
		// host.auth.save wire: {"name": "<bare>.json", "json": <credential>}
		// （HostAuthSaveRequest；旧的 {"auth":…} 会报 invalid auth file name）。
		if _, err := p.bridge.AuthSave(fileName, raw); err != nil {
			return "", err
		}
		p.invalidateAuthList()
		// 宿主按文件内容生成凭据 ID；正常情况下与本地指纹一致，仍以宿主为准。
		if files, err := p.bridge.AuthList(); err == nil {
			for _, f := range files {
				if f.Provider != ProviderID {
					continue
				}
				if f.ID == id || f.Name == fileName {
					if f.ID != "" {
						id = f.ID
					}
					break
				}
			}
		}
	}
	p.invalidateAuthList()
	p.store.ClearPurge(id) // 同一 key 重新添加 = 撤销删除
	p.store.SetEnabled(id, true)
	// 重新添加是操作者的明确动作：此前 401/403 留下的登录/待验证标记
	// 必须清除，否则重新添加后账号仍不可用。
	p.store.SetRequiresSignIn(id, false)
	p.store.SetVerifyPending(id, false)
	p.store.EnsureAccount(id)
	return id, nil
}

func (p *Plugin) viewOf(id string, acc *storeAccount, snap store.Snapshot, detail bool, bindings int) accountView {
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
	// 固定窗口顺序：map 随机迭代会让 /accounts 的 JSON 每轮都变。
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		if w, ok := acc.Windows[name]; ok {
			v.Windows = append(v.Windows, windowView{Window: name, UsedPct: w.UsedPct, Until: w.Until.UTC().Format(time.RFC3339), Source: w.Source})
		}
	}
	var extra []string
	for name := range acc.Windows {
		if name != "rolling" && name != "weekly" && name != "monthly" {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		w := acc.Windows[name]
		v.Windows = append(v.Windows, windowView{Window: name, UsedPct: w.UsedPct, Until: w.Until.UTC().Format(time.RFC3339), Source: w.Source})
	}
	v.Usage = usageView{Requests: acc.Usage.Requests, InputTokens: acc.Usage.InputTokens, OutputTokens: acc.Usage.OutputTokens}
	if acc.Usage.LastUsedAt != nil {
		v.Usage.LastUsedAt = acc.Usage.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if acc.LastError != nil {
		v.LastError = &lastError{Code: acc.LastError.Code, At: acc.LastError.At.UTC().Format(time.RFC3339)}
	}
	v.BindingsCount = bindings
	if detail {
		for _, h := range acc.History {
			v.History = append(v.History, historyEntry{At: h.At.UTC().Format(time.RFC3339), Code: h.Code})
		}
	}
	v.Status = statusString(v, p.config().NearLimitThresholdPct)
	return v
}

// 类型别名：指向 store 的真实类型
type storeAccount = store.Account

func mustTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// authInfoFor reads one credential and returns (label, maskedKey). The host
// list entry carries auth_index; auth.get rejects our internal id.
func (p *Plugin) authInfoFor(id string) (string, string) {
	if p.bridge == nil {
		return labelFromID(id), ""
	}
	entry, ok := p.hostAuthEntries()[id]
	if !ok {
		return labelFromID(id), ""
	}
	label := strings.TrimSpace(entry.Label)
	if entry.AuthIndex == "" {
		if label == "" {
			label = labelFromID(id)
		}
		return label, ""
	}
	raw, err := p.bridge.AuthGet(entry.AuthIndex)
	if err != nil {
		if label == "" {
			label = labelFromID(id)
		}
		return label, ""
	}
	st := parseStorage(raw)
	if label == "" {
		label = strings.TrimSpace(st.Label)
	}
	if label == "" {
		label = strings.TrimSpace(st.Name)
	}
	if label == "" {
		label = labelFromID(id)
	}
	return label, maskKey(st.APIKey)
}

// labelFromID derives a readable label from an auth id when nothing better
// is stored.
func labelFromID(id string) string {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return id
	}
	tail := parts[1]
	if i := strings.LastIndex(tail, ":"); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	if len(tail) >= 4 {
		return "未命名 · " + tail[:4] + "…" + tail[len(tail)-4:]
	}
	return id
}

// hostAuthListTTL keeps the panel's 5s poll from hammering host.auth.list.
const hostAuthListTTL = 3 * time.Second

// hostAuthEntries returns live host credentials keyed by auth id. The host
// addresses credentials by auth_index in auth.get / routing.reset_cooldown,
// so callers resolve ids through this map.
func (p *Plugin) hostAuthEntries() map[string]pluginapi.HostAuthFileEntry {
	if p.bridge == nil {
		return nil
	}
	p.authListMu.Lock()
	defer p.authListMu.Unlock()
	if p.authList != nil && time.Since(p.authListAt) < hostAuthListTTL {
		return p.authList
	}
	files, err := p.bridge.AuthList()
	if err != nil {
		return nil
	}
	m := make(map[string]pluginapi.HostAuthFileEntry, len(files))
	for _, f := range files {
		if f.ID != "" {
			m[f.ID] = f
		}
	}
	p.authList = m
	p.authListAt = time.Now()
	return m
}

func (p *Plugin) invalidateAuthList() {
	p.authListMu.Lock()
	p.authList = nil
	p.authListMu.Unlock()
}

// foreignAccount reports a store record that belongs to another provider
// (e.g. claude:apikey:…), seeded by the host-global usage stream. Such ids
// are never created by this plugin (materialize always uses ProviderID).
func foreignAccount(id string, auths map[string]pluginapi.HostAuthFileEntry) bool {
	if strings.HasPrefix(id, ProviderID+":") {
		return false
	}
	entry, ok := auths[id]
	if !ok {
		return false // unknown ≠ foreign: never purge on missing transient data
	}
	provider := entry.Provider
	if provider == "" {
		provider = entry.Type
	}
	return provider != "" && provider != ProviderID
}

// usageRefreshInterval bounds how often the panel silently polls each
// account's quota endpoint. One minute looked like "一直探测"; five minutes
// is plenty for quota bars and keeps upstream traffic quiet.
const usageRefreshInterval = 5 * time.Minute

// forceRefreshUsage bypasses the per-account throttle (explicit operator
// action) and kicks a background /usage pull.
func (p *Plugin) forceRefreshUsage(id, authIndex string) {
	p.usageFetchMu.Lock()
	delete(p.usageFetched, id)
	p.usageFetchMu.Unlock()
	p.refreshUsage(id, authIndex)
}

// refreshUsage pulls /usage in the background (throttled) so window bars
// appear without waiting for a 429.
func (p *Plugin) refreshUsage(id, authIndex string) {
	if p.bridge == nil || p.store.IsPurged(id) || authIndex == "" || !p.config().QuotaProbeEnabled {
		return
	}
	p.usageFetchMu.Lock()
	if p.usageFetched == nil {
		p.usageFetched = map[string]time.Time{}
	}
	if t, ok := p.usageFetched[id]; ok && time.Since(t) < usageRefreshInterval {
		p.usageFetchMu.Unlock()
		return
	}
	p.usageFetched[id] = time.Now()
	p.usageFetchMu.Unlock()
	go func() {
		raw, err := p.bridge.AuthGet(authIndex)
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
