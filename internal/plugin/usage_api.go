package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"cpa-plugin-opencode-pool/internal/store"
	"cpa-plugin-opencode-pool/internal/upstream"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageWindow is one window observation parsed from /zen/go/v1/usage.
type usageWindow = store.Window

// parseUsageResponse decodes the real OpenCode Go usage payload:
//
//	{"usage":{"rolling":{"status":"ok","percent":41,"resetsAt":"..."},
//	          "weekly":{...},"monthly":{...}}}
//
// Window names are rolling / weekly / monthly (verified 2026-11-21). Unknown
// shapes and missing sections are skipped, never fatal.
func parseUsageResponse(body []byte) map[string]usageWindow {
	var payload struct {
		Usage map[string]struct {
			Status   string `json:"status"`
			Percent  int    `json:"percent"`
			ResetsAt string `json:"resetsAt"`
		} `json:"usage"`
	}
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil || payload.Usage == nil {
		return map[string]usageWindow{}
	}
	out := make(map[string]usageWindow, len(payload.Usage))
	now := time.Now()
	for name, w := range payload.Usage {
		if name == "" || w.ResetsAt == "" {
			continue
		}
		until, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
		if err != nil {
			continue
		}
		pct := float64(w.Percent)
		if w.Status == "limited" && pct < 100 {
			pct = 100
		}
		out[name] = usageWindow{UsedPct: pct, Until: until, Source: "api", SeenAt: now}
	}
	return out
}

// fetchAndApplyUsage pulls /usage for one account and merges into the store
// (monotonic-with-authority rules apply inside ObserveWindowSource).
func (p *Plugin) fetchAndApplyUsage(authID, apiKey, baseURL string) bool {
	if p.bridge == nil || apiKey == "" {
		return false
	}
	resp, err := p.bridge.Do(context.Background(), pluginapi.HTTPRequest{
		Method: "GET",
		URL:    upstream.Join(baseURL, "usage"),
		Headers: http.Header{
			"User-Agent":    []string{p.config().UpstreamUserAgent},
			"Authorization": []string{"Bearer " + apiKey},
		},
	}, "")
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	wins := parseUsageResponse(resp.Body)
	if len(wins) == 0 {
		return false
	}
	now := time.Now()
	for name, w := range wins {
		p.store.ObserveWindowSource(authID, name, "api", w.UsedPct, w.Until, now)
	}
	return true
}
