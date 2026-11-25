package plugin

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// orderedWindowNames returns windows in a stable display order: the three
// known plan windows first, then any extras alphabetically. Host quota polls
// must not reshuffle between refreshes (map iteration is random).
func orderedWindowNames(windows map[string]bool) []string {
	out := make([]string, 0, len(windows))
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		if windows[name] {
			out = append(out, name)
		}
	}
	extra := make([]string, 0)
	for name := range windows {
		if name != "rolling" && name != "weekly" && name != "monthly" {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// quotaFetch surfaces per-account windows as normalized quota buckets.
func (p *Plugin) quotaFetch(request []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	// 主动路径：宿主会按自己的节奏反复调 quota.fetch；这里复用面板同一个
	// 节流器（5 分钟/账号），否则上游 /usage 会被持续探测。
	if req.AuthID != "" && !p.store.IsPurged(req.AuthID) {
		index := req.AuthIndex
		if index == "" {
			index = p.hostAuthEntries()[req.AuthID].AuthIndex
		}
		p.refreshUsage(req.AuthID, index)
	}
	snap := p.store.Snapshot()
	acc := snap.Accounts[req.AuthID]
	resp := pluginapi.QuotaFetchResponse{}
	if acc != nil {
		var groups []pluginapi.QuotaGroup
		present := make(map[string]bool, len(acc.Windows))
		for name := range acc.Windows {
			if name != "" {
				present[name] = true
			}
		}
		for _, name := range orderedWindowNames(present) {
			w := acc.Windows[name]
			remaining := (100 - w.UsedPct) / 100
			if remaining < 0 {
				remaining = 0
			}
			if remaining > 1 {
				remaining = 1
			}
			bucket := pluginapi.QuotaBucket{
				Window:            name,
				RemainingFraction: remaining,
				ResetTime:         w.Until.UTC().Format(time.RFC3339),
				Description:       "source:" + w.Source,
			}
			groups = append(groups, pluginapi.QuotaGroup{DisplayName: name, Buckets: []pluginapi.QuotaBucket{bucket}})
		}
		resp.Groups = groups
	}
	return okEnvelope(resp)
}
