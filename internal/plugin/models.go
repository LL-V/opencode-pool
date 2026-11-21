package plugin

import (
	"context"
	"encoding/json"
	"time"

	"cpa-plugin-opencode-pool/internal/upstream"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// builtinModels is the compile-time third fallback of the never-empty chain
// (DESIGN.md M2b: an empty ModelsForAuth unregisters the account host-side).
var builtinModels = []string{
	"claude-sonnet-4-5", "claude-opus-4-6", "gpt-5.5", "gpt-5.5-codex",
	"glm-5.3", "kimi-k3", "qwen3-max", "deepseek-v4",
}

func modelInfos(ids []string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: ProviderID, Type: ProviderID, DisplayName: id, Name: id})
	}
	return out
}

// probeTimeout bounds plugin-initiated /models and /usage calls so a hung
// upstream cannot block host registration/model discovery forever.
const probeTimeout = 8 * time.Second

// doProbe runs one upstream call through the host transport with a deadline.
// The host transport callback itself is not cancellable, so the goroutine may
// finish later; the caller stops waiting at the deadline.
func (p *Plugin) doProbe(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	type result struct {
		resp pluginapi.HTTPResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := p.bridge.Do(context.Background(), req, "")
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(probeTimeout):
		return pluginapi.HTTPResponse{}, context.DeadlineExceeded
	}
}

// fetchModelsList pulls {base}/models through the host transport with the
// given credential; returns nil on any failure.
func (p *Plugin) fetchModelsList(key, base string) []string {
	if p.bridge == nil {
		return nil
	}
	hdrs := map[string][]string{"User-Agent": {p.config().UpstreamUserAgent}}
	if key != "" {
		hdrs["Authorization"] = []string{"Bearer " + key}
	}
	resp, err := p.doProbe(pluginapi.HTTPRequest{
		Method: "GET", URL: upstream.Join(base, "models"), Headers: hdrs,
	})
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil || len(payload.Data) == 0 {
		return nil
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// firstAccount returns one opencode-go credential's key and base_url.
func (p *Plugin) firstAccount() (key, base string) {
	files, err := p.bridge.AuthList()
	if err != nil {
		return "", ""
	}
	for _, f := range files {
		if f.Provider != ProviderID || f.AuthIndex == "" {
			continue
		}
		raw, err := p.bridge.AuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		st := parseStorage(raw)
		if st.APIKey != "" {
			return st.APIKey, st.BaseURL
		}
	}
	return "", ""
}

// modelsStaticTTL caches the provider-wide catalog (model.static is polled by
// the host and must not hit the network on every call).
const modelsStaticTTL = 5 * time.Minute

// staticModels returns the cached provider catalog or refetches it.
func (p *Plugin) staticModels() []string {
	p.modelsMu.Lock()
	if p.modelsIDs != nil && time.Since(p.modelsAt) < modelsStaticTTL {
		ids := p.modelsIDs
		p.modelsMu.Unlock()
		return ids
	}
	p.modelsMu.Unlock()
	key, base := p.firstAccount()
	ids := p.fetchModelsList(key, base)
	if len(ids) == 0 {
		return nil
	}
	p.modelsMu.Lock()
	p.modelsIDs, p.modelsAt = ids, time.Now()
	p.modelsMu.Unlock()
	return ids
}

// modelsStatic serves model.static (provider catalog).
func (p *Plugin) modelsStatic(request []byte) ([]byte, error) {
	ids := p.staticModels()
	if len(ids) == 0 {
		ids = builtinModels
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: modelInfos(ids)})
}

// modelsForAuth serves model.for_auth with the never-empty fallback chain:
// per-account cache → static catalog → builtin constants.
// modelsCacheTTL bounds how long a fetched catalog stays fresh.
const modelsCacheTTL = 24 * time.Hour

func (p *Plugin) modelsForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	cached := p.store.ModelsCache(req.AuthID)
	ids := cached
	if len(ids) == 0 || !p.store.ModelsCacheFresh(req.AuthID, modelsCacheTTL) {
		st := parseStorage(req.StorageJSON)
		ids = p.fetchModelsList(st.APIKey, st.BaseURL)
		if len(ids) == 0 {
			ids = p.staticModels() // provider-wide fallback
		}
	}
	if len(ids) == 0 {
		ids = builtinModels
	} else {
		p.store.SetModelsCache(req.AuthID, ids, time.Now())
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: modelInfos(ids)})
}

// InvalidateModelsCache drops a per-account catalog (model_not_found hook).
func (p *Plugin) InvalidateModelsCache(authID string) {
	p.store.SetModelsCache(authID, nil, time.Now())
	p.modelsMu.Lock()
	p.modelsIDs, p.modelsAt = nil, time.Time{}
	p.modelsMu.Unlock()
}
