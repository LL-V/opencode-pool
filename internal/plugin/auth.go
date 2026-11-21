package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// authParse recognizes opencode-go auth material and expands it into AuthData.
func (p *Plugin) authParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	st := parseStorage(req.RawJSON)
	if st.Type != "opencode-go" {
		return okEnvelope(pluginapi.AuthParseResponse{})
	}
	fingerprint := keyFingerprint(st.APIKey)
	label := st.Label
	if label == "" {
		label = st.Name // 账户名（邮箱）优先于脱敏 key
	}
	if label == "" && st.APIKey != "" {
		label = maskKey(st.APIKey)
	}
	auth := pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          ProviderID + ":" + fingerprint,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: req.RawJSON,
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// authRefresh keeps OAuth credentials fresh ahead of expiry; api_key
// accounts pass through unchanged (they never expire client-side).
func (p *Plugin) authRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	st := parseStorage(req.StorageJSON)
	if st.Type != "opencode-go" || st.APIKey != "" {
		// api_key accounts: nothing to refresh.
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				Provider: ProviderID, ID: req.AuthID,
				Label: st.Label, StorageJSON: req.StorageJSON,
			},
		})
	}
	// OAuth refresh lands with the device-flow login milestone.
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider: ProviderID, ID: req.AuthID,
			Label: st.Label, StorageJSON: req.StorageJSON,
		},
	})
}

// maskKey renders an API key recognizable but safe: keep a short prefix and
// the last 4 characters. Very short keys are fully masked.
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "••••"
	}
	if len(key) <= 10 {
		return key[:1] + "…" + key[len(key)-1:]
	}
	return key[:12] + "…" + key[len(key)-4:]
}
