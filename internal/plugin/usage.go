package plugin

import (
	"encoding/json"
	"time"

	"cpa-plugin-opencode-pool/internal/classifier"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageHandle is the single inflight decrement point and the passive quota
// harvest path (DESIGN.md M5/M7: every attempt yields exactly one record,
// including failures and cancellations).
func (p *Plugin) usageHandle(request []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(request, &rec); err != nil {
		return nil, err
	}
	if rec.AuthID == "" {
		return okEnvelope(map[string]any{})
	}
	p.store.EnsureAccount(rec.AuthID)
	p.store.InflightDec(rec.AuthID)
	now := time.Now()
	if rec.Failed {
		body := []byte(rec.Failure.Body)
		res := classifier.Classify(rec.Failure.StatusCode, body, nil)
		switch res.Category {
		case classifier.CategoryQuota:
			p.store.ObserveWindowSource(rec.AuthID, res.Window, "429", 100, res.ResetAt, now)
		case classifier.CategoryConcurrency:
			p.store.LearnLimit(rec.AuthID, res.Limit)
		}
		p.store.NoteError(rec.AuthID, string(res.Category), now)
		return okEnvelope(map[string]any{})
	}
	p.store.AddUsage(rec.AuthID, rec.Detail.InputTokens, rec.Detail.OutputTokens, now)
	if rec.SessionID != "" {
		p.store.Bind(rec.SessionID, rec.AuthID, now)
	}
	return okEnvelope(map[string]any{})
}
