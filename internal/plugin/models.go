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

// fetchModelsList pulls {base}/models through the host transport using the
// first available account credential; returns nil on any failure.
func (p *Plugin) fetchModelsList() []string {
	if p.bridge == nil {
		return nil
	}
	key := p.firstAccountKey()
	hdrs := map[string][]string{"User-Agent": {p.config().UpstreamUserAgent}}
	if key != "" {
		hdrs["Authorization"] = []string{"Bearer " + key}
	}
	resp, err := p.bridge.Do(context.Background(), pluginapi.HTTPRequest{
		Method: "GET", URL: upstream.Join("", "models"), Headers: hdrs,
	}, "")
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

// firstAccountKey fetches one opencode-go auth document for its api_key.
func (p *Plugin) firstAccountKey() string {
	files, err := p.bridge.AuthList()
	if err != nil {
		return ""
	}
	for _, f := range files {
		if f.Provider != ProviderID || f.ID == "" {
			continue
		}
		raw, err := p.bridge.AuthGet(f.ID)
		if err != nil {
			continue
		}
		if key := parseStorage(raw).APIKey; key != "" {
			return key
		}
	}
	return ""
}

// modelsStatic serves model.static (provider catalog).
func (p *Plugin) modelsStatic(request []byte) ([]byte, error) {
	ids := p.fetchModelsList()
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
		ids = p.fetchModelsList()
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
}
