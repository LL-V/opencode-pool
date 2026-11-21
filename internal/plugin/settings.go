package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settings.json persists operator edits made through PUT /settings so they
// survive plugin reloads; the file lives beside the store data.
const settingsFileName = "settings.json"

// loadSettingsOverlay applies a persisted settings.json over the config from
// plugin.register (runtime edits win over static config).
func (p *Plugin) loadSettingsOverlay() {
	if p.dataDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(p.dataDir, settingsFileName))
	if err != nil {
		return
	}
	if cfg, err := parseConfig(raw); err == nil {
		p.mu.Lock()
		p.cfg = cfg
		p.mu.Unlock()
	}
}

// updateSettings applies a partial Config JSON (omitted fields keep their
// current value) and persists the merged result.
func (p *Plugin) updateSettings(body []byte) (Config, error) {
	var in struct {
		UpstreamUserAgent     *string   `json:"upstream_user_agent"`
		SessionHeaderSources  *[]string `json:"session_header_sources"`
		ConcurrencyKeywords   *[]string `json:"concurrency_keywords"`
		NearLimitThresholdPct *float64  `json:"near_limit_threshold_pct"`
		QuotaProbeEnabled     *bool     `json:"quota_probe_enabled"`
		PanelPollSeconds      *int      `json:"panel_poll_seconds"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Config{}, err
	}
	p.mu.Lock()
	cfg := p.cfg
	if in.UpstreamUserAgent != nil && *in.UpstreamUserAgent != "" {
		cfg.UpstreamUserAgent = *in.UpstreamUserAgent
	}
	if in.SessionHeaderSources != nil {
		cfg.SessionHeaderSources = *in.SessionHeaderSources
	}
	if in.ConcurrencyKeywords != nil {
		cfg.ConcurrencyKeywords = *in.ConcurrencyKeywords
	}
	if in.NearLimitThresholdPct != nil && *in.NearLimitThresholdPct > 0 && *in.NearLimitThresholdPct < 100 {
		cfg.NearLimitThresholdPct = *in.NearLimitThresholdPct
	}
	if in.QuotaProbeEnabled != nil {
		cfg.QuotaProbeEnabled = *in.QuotaProbeEnabled
	}
	if in.PanelPollSeconds != nil && *in.PanelPollSeconds > 0 {
		cfg.PanelPollSeconds = *in.PanelPollSeconds
	}
	p.cfg = cfg
	p.mu.Unlock()
	p.persistSettings()
	return cfg, nil
}

// persistSettings writes settings.json atomically (tmp + rename, 0600).
func (p *Plugin) persistSettings() {
	if p.dataDir == "" {
		return
	}
	if err := os.MkdirAll(p.dataDir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(p.config())
	if err != nil {
		return
	}
	path := filepath.Join(p.dataDir, settingsFileName)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
