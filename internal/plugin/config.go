package plugin

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the plugin configuration delivered at register/reconfigure.
type Config struct {
	UpstreamUserAgent     string   `json:"upstream_user_agent" yaml:"upstream_user_agent"`
	SessionHeaderSources  []string `json:"session_header_sources" yaml:"session_header_sources"`
	ConcurrencyKeywords   []string `json:"concurrency_keywords" yaml:"concurrency_keywords"`
	NearLimitThresholdPct float64  `json:"near_limit_threshold_pct" yaml:"near_limit_threshold_pct"`
	QuotaProbeEnabled     bool     `json:"quota_probe_enabled" yaml:"quota_probe_enabled"`
	PanelPollSeconds      int      `json:"panel_poll_seconds" yaml:"panel_poll_seconds"`
}

// DefaultUserAgent impersonates the Claude Code CLI per user decision R9.
const DefaultUserAgent = "claude-cli/2.1.296 (external, cli)"

func defaultConfig() Config {
	return Config{
		UpstreamUserAgent:     DefaultUserAgent,
		SessionHeaderSources:  nil, // session package defaults
		ConcurrencyKeywords:   []string{"concurr", "parallel", "simultaneous"},
		NearLimitThresholdPct: 8, // used_pct >= 92 flags near
		QuotaProbeEnabled:     true,
		PanelPollSeconds:      5,
	}
}

// parseConfig accepts the config_yaml bytes delivered by plugin.register
// (YAML or JSON) and applies defaults for absent fields.
func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, err
		}
	} else if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.UpstreamUserAgent) == "" {
		cfg.UpstreamUserAgent = DefaultUserAgent
	}
	if cfg.PanelPollSeconds <= 0 {
		cfg.PanelPollSeconds = 5
	}
	if cfg.NearLimitThresholdPct <= 0 || cfg.NearLimitThresholdPct >= 100 {
		cfg.NearLimitThresholdPct = 8
	}
	return cfg, nil
}
