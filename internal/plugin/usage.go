package plugin

import (
	"encoding/json"
	"strings"
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
	// 宿主 at-least-once 投递：同一 RequestID 只记账/减计数一次。
	if rec.RequestID != "" && p.markUsageSeen(rec.RequestID) {
		return okEnvelope(map[string]any{})
	}
	// inflight 配对属于“这次尝试”本身，必须在任何过滤之前完成；
	// 否则将来 Provider 取值变化会让计数永久泄漏、账号永远 Busy。
	p.releasePick(rec.AuthID)
	p.store.InflightDec(rec.AuthID)
	// 已删除账号：宿主 auth 文件仍在、usage 仍会来，但不得复活。
	if p.store.IsPurged(rec.AuthID) {
		return okEnvelope(map[string]any{})
	}
	// 宿主把 usage 事件广播给所有 usage 插件（全局，不分 provider）。
	// 只处理本插件执行器/凭据的账目：否则 claude/gemini 等原生账号会被
	// 误建进面板（用户实测：删除后又出现 apik…4f95，token 一直涨）。
	// Provider 是执行器 Identifier；老宿主可能为空，退回 id 前缀判断。
	if rec.Provider != "" {
		if rec.Provider != ProviderID {
			return okEnvelope(map[string]any{})
		}
	} else if !strings.HasPrefix(rec.AuthID, ProviderID+":") {
		return okEnvelope(map[string]any{})
	}
	p.store.EnsureAccount(rec.AuthID)
	// 健康探测是宿主/控制台自动发的，不计入运营台账（否则"没用也在涨"）。
	probe := strings.HasPrefix(rec.SessionID, "verify-")
	now := time.Now()
	if rec.Failed {
		body := []byte(rec.Failure.Body)
		res := classifier.ClassifyWithKeywords(rec.Failure.StatusCode, body, nil, p.config().ConcurrencyKeywords)
		switch res.Category {
		case classifier.CategoryQuota:
			p.store.ObserveWindowSource(rec.AuthID, res.Window, "429", 100, res.ResetAt, now)
		case classifier.CategoryConcurrency:
			p.store.LearnLimit(rec.AuthID, res.Limit)
		}
		p.store.NoteError(rec.AuthID, string(res.Category), now)
		return okEnvelope(map[string]any{})
	}
	if !probe {
		p.store.AddUsage(rec.AuthID, rec.Detail.InputTokens, rec.Detail.OutputTokens, now)
	}
	// 探针/无意义 body 哈希不写亲和，否则会挤掉真实会话绑定。
	if rec.SessionID != "" && !probe && !strings.HasPrefix(rec.SessionID, "b-") {
		p.store.Bind(rec.SessionID, rec.AuthID, now)
	}
	return okEnvelope(map[string]any{})
}

// usageSeenTTL bounds the idempotency set; host replays happen within
// seconds, so a few minutes is plenty.
const usageSeenTTL = 10 * time.Minute

// markUsageSeen returns true when requestID was already processed.
func (p *Plugin) markUsageSeen(requestID string) bool {
	p.usageSeenMu.Lock()
	defer p.usageSeenMu.Unlock()
	now := time.Now()
	if p.usageSeen == nil {
		p.usageSeen = make(map[string]time.Time)
	}
	if t, ok := p.usageSeen[requestID]; ok && now.Sub(t) < usageSeenTTL {
		return true
	}
	// opportunistic prune (bounded growth; replays are short-lived)
	if len(p.usageSeen) > 4096 {
		for id, t := range p.usageSeen {
			if now.Sub(t) > usageSeenTTL {
				delete(p.usageSeen, id)
			}
		}
	}
	p.usageSeen[requestID] = now
	return false
}
