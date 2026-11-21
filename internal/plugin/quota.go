package plugin

import (
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaFetch surfaces per-account windows as normalized quota buckets.
func (p *Plugin) quotaFetch(request []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	// 主动路径：先经宿主通道拉一次真实 /usage（面板打开/详情展开触发）。
	if p.bridge != nil && req.AuthID != "" {
		if raw, err := p.bridge.AuthGet(req.AuthID); err == nil {
			st := parseStorage(raw)
			p.fetchAndApplyUsage(req.AuthID, st.APIKey, st.BaseURL)
		}
	}
	snap := p.store.Snapshot()
	acc := snap.Accounts[req.AuthID]
	resp := pluginapi.QuotaFetchResponse{}
	if acc != nil {
		var groups []pluginapi.QuotaGroup
		for name, w := range acc.Windows {
			bucket := pluginapi.QuotaBucket{
				Window:            name,
				RemainingFraction: (100 - w.UsedPct) / 100,
				ResetTime:         w.Until.UTC().Format(time.RFC3339),
				Description:       "source:" + w.Source,
			}
			groups = append(groups, pluginapi.QuotaGroup{DisplayName: name, Buckets: []pluginapi.QuotaBucket{bucket}})
		}
		resp.Groups = groups
	}
	return okEnvelope(resp)
}
