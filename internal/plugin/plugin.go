// Package plugin wires the opencode-pool capabilities onto the CLIProxyAPI
// plugin ABI per DESIGN.md v1.0 (M0-M8).
package plugin

import (
	"sync"
	"time"

	"cpa-plugin-opencode-pool/internal/scheduler"
	"cpa-plugin-opencode-pool/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// PluginID namespaces the store, management routes, and registration.
// ProviderID is the host model-routing namespace (opencode-go/* models).
const (
	PluginID      = "opencode-pool"
	ProviderID    = "opencode-go"
	PluginVersion = "0.1.1"
)

// Plugin is the assembled capability set plus runtime state.
type Plugin struct {
	cfg       Config
	bridge    *Bridge
	store     *Store
	dataDir   string
	stopFlush func()
	mu        sync.Mutex
}

// autoFlushInterval is the periodic state.json persist cadence.
const autoFlushInterval = 5 * time.Second

// Store narrows the store type used by Plugin.
type Store = store.Store

// New builds the plugin. caller may be nil (unit tests inject fakes later).
func New(caller HostCaller, _ []byte, dataDir string) (*Plugin, error) {
	p := &Plugin{cfg: defaultConfig(), dataDir: dataDir}
	if caller != nil {
		p.bridge = NewBridge(caller)
	}
	p.store = store.New(dataDir)
	p.loadSettingsOverlay()
	return p, nil
}

// Store exposes runtime state (read-only consumers).
func (p *Plugin) Store() *store.Store { return p.store }

// Bridge exposes the host bridge (nil before init in unit contexts).
func (p *Plugin) Bridge() *Bridge { return p.bridge }

// applyConfig reconfigures the plugin (register/reconfigure).
func (p *Plugin) applyConfig(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return nil
}

func (p *Plugin) config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// registration builds the ABI registration payload.
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]any     `json:"capabilities"`
}

func (p *Plugin) buildRegistration() registration {
	return registration{
		SchemaVersion: 10,
		Metadata: pluginapi.Metadata{
			Name:            PluginID,
			Version:         PluginVersion,
			Author:          "LL-V",
			GitHubRepository: "https://github.com/LL-V/opencode-pool",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "upstream_user_agent", Type: pluginapi.ConfigFieldTypeString, Default: DefaultUserAgent, Description: "Upstream User-Agent override."},
				{Name: "session_header_sources", Type: pluginapi.ConfigFieldTypeArray, Description: "Inbound session header sources in priority order."},
				{Name: "concurrency_keywords", Type: pluginapi.ConfigFieldTypeArray, Default: []string{"concurr", "parallel", "simultaneous"}, Description: "Concurrency 429 message keywords."},
				{Name: "near_limit_threshold_pct", Type: pluginapi.ConfigFieldTypeNumber, Default: 8, Description: "Percent below 100 that flags near-limit."},
				{Name: "quota_probe_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: true, Description: "Probe a tiny request after window reset."},
				{Name: "panel_poll_seconds", Type: pluginapi.ConfigFieldTypeInteger, Default: 5, Description: "Panel list poll interval."},
			},
		},
		Capabilities: map[string]any{
			"model_provider":              true,
			"auth_provider":               true,
			"scheduler":                   true,
			"scheduler_across_priorities": true,
			"executor":                    true,
			"executor_model_scope":        pluginapi.ExecutorModelScopeOAuth,
			"executor_input_formats":      []string{"openai", "claude", "openai-response"},
			"executor_output_formats":     []string{"openai", "claude", "openai-response"},
			"request_interceptor":         true,
			"usage_plugin":                true,
			"management_api":              true,
			"quota_provider":              true,
		},
	}
}

// —— scheduler.Deps 实现 bridging store state ——

type storeDeps struct {
	p     *Plugin
	cands map[string]bool // candidate ids for EarliestRecovery scoping
}

func (d storeDeps) Bad(id string) bool {
	acc := d.p.store.Snapshot().Accounts[id]
	if acc == nil {
		return false
	}
	if acc.RequiresSignIn || acc.Disabled {
		return true // 禁用/需登录的账号绝不推荐（含亲和路径）
	}
	// 任一窗口未到期耗尽即暂停：上游任一窗口满即 429，"全耗尽才停"
	// 会让 rolling 满、weekly 健康的账号被反复选中撞墙。
	return d.p.store.AnyWindowExhausted(id, time.Now())
}

func (d storeDeps) Busy(id string) bool {
	snap := d.p.store.Snapshot()
	acc := snap.Accounts[id]
	if acc == nil || acc.LearnedConcurrencyLimit <= 0 {
		return false // unknown limit never busy
	}
	return snap.Inflight[id] >= acc.LearnedConcurrencyLimit
}

func (d storeDeps) Near(id string) bool {
	now := time.Now()
	acc := d.p.store.Snapshot().Accounts[id]
	if acc == nil {
		return false
	}
	nearAt := 100 - d.p.config().NearLimitThresholdPct
	for _, w := range acc.Windows {
		// 已过期的窗口是陈旧数据：不参与 near，否则无流量的账号会被
		// 永久钉在 near 形成"被回避→无刷新"停滞（deep-check P2）。
		if w.UsedPct >= nearAt && w.UsedPct < 100 && now.Before(w.Until) {
			return true
		}
	}
	return false
}

// Pressure = remaining% / max(hours to reset, 0.5), only when pct is known.
func (d storeDeps) Pressure(id string) (float64, bool) {
	now := time.Now()
	acc := d.p.store.Snapshot().Accounts[id]
	if acc == nil || len(acc.Windows) == 0 {
		return 0, false
	}
	best := -1.0
	ok := false
	for _, w := range acc.Windows {
		if w.UsedPct <= 0 || w.UsedPct >= 100 || !now.Before(w.Until) {
			continue // 过期窗口同上，不参与压力排序
		}
		h := time.Until(w.Until).Hours()
		if h < 0.5 {
			h = 0.5
		}
		if pressure := (100 - w.UsedPct) / h; pressure > best {
			best, ok = pressure, true
		}
	}
	return best, ok
}

func (d storeDeps) Inflight(id string) int {
	return d.p.store.Snapshot().Inflight[id]
}

func (d storeDeps) BoundAuth(sid string) (string, bool) {
	return d.p.store.LookupBinding(sid, time.Now())
}

func (d storeDeps) EarliestRecovery() time.Time {
	snap := d.p.store.Snapshot()
	var earliest time.Time
	for id := range d.cands {
		acc := snap.Accounts[id]
		if acc == nil {
			continue
		}
		for _, w := range acc.Windows {
			if w.UsedPct >= 100 && (earliest.IsZero() || w.Until.Before(earliest)) {
				earliest = w.Until
			}
		}
	}
	return earliest
}

var _ scheduler.Deps = storeDeps{}

// LoadStore reads a persisted store from path (nil error required).
func LoadStore(path string) (*store.Store, error) {
	return store.Load(path)
}

// AdoptStore replaces the runtime store (register-time state restore).
func (p *Plugin) AdoptStore(s *store.Store) {
	if s != nil {
		p.store = s
	}
}
