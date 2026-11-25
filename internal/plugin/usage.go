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
	// 宿主一次 attempt 可能投递多条 usage：附加模型（图片工具等）记录会
	// 重新生成 RequestID，但共享同一个 SelectionID（宿主 usage_helpers
	// buildAdditionalModelRecord）。按 SelectionID 去重保证一次 execute 只
	// 减一次 inflight；缺 SelectionID 时才退回 RequestID（at-least-once 重放）。
	if key := usageDedupeKey(rec); key != "" && p.markUsageSeen(key) {
		return okEnvelope(map[string]any{})
	}
	// inflight 配对属于“这次尝试”本身，必须在任何过滤之前完成；
	// 否则将来 Provider 取值变化会让计数永久泄漏、账号永远 Busy。
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
			// 限额 429 没有窗口词时也必须归到 rolling：空键窗口永远不会被
			// /usage 的 rolling/weekly 更新覆盖，会把账号封到幽灵窗口到期。
			window := res.Window
			if window == "" {
				window = "rolling"
			}
			p.store.ObserveWindowSource(rec.AuthID, window, "429", 100, res.ResetAt, now)
		case classifier.CategoryConcurrency:
			p.store.LearnLimit(rec.AuthID, res.Limit)
		case classifier.CategoryAuthInvalid:
			// 失败尝试同样要维护凭据状态（例如 executor 早退没分类的 401），
			// 否则坏账号会一直留在候选池里。
			p.store.SetRequiresSignIn(rec.AuthID, true)
		case classifier.CategoryAuthSuspect:
			p.store.SetVerifyPending(rec.AuthID, true)
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

// usageDedupeKey identifies one executor attempt. SelectionID is per pick and
// is shared by the primary and additional-model usage records; RequestID is
// unique per record and only serves the replay case when SelectionID is
// absent.
func usageDedupeKey(rec pluginapi.UsageRecord) string {
	if v := strings.TrimSpace(rec.SelectionID); v != "" {
		return "sel:" + v
	}
	if v := strings.TrimSpace(rec.RequestID); v != "" {
		return "req:" + v
	}
	return ""
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
