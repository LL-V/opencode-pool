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
	// 主动路径：宿主会按自己的节奏反复调 quota.fetch；这里复用面板同一个
	// 节流器（5 分钟/账号），否则上游 /usage 会被持续探测。
	if req.AuthID != "" && !p.store.IsPurged(req.AuthID) {
		p.refreshUsage(req.AuthID)
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
