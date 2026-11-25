package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 一次 execute 可能产生多条 usage（附加模型记录重新生成 RequestID，但
// 共享同一个 SelectionID）。inflight 只能减一次，否则会击穿并发闸门。
func TestUsageDedupeBySelectionID(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := "opencode-go:dedupe"
	p.Store().EnsureAccount(id)

	usage := func(sel, reqID string) {
		raw, _ := json.Marshal(pluginapi.UsageRecord{
			AuthID: id, Provider: ProviderID, SelectionID: sel, RequestID: reqID,
		})
		if _, err := p.HandleMethod(pluginabi.MethodUsageHandle, raw); err != nil {
			t.Fatal(err)
		}
	}

	p.Store().InflightInc(id) // executor 开始
	usage("sel-1", "req-1")
	usage("sel-1", "req-2") // 附加模型记录：不同 RequestID，同 SelectionID
	if got := p.Store().Snapshot().Inflight[id]; got != 0 {
		t.Fatalf("同一次 attempt 的多条 usage 只应减一次：inflight=%d", got)
	}
	if got := p.Store().Snapshot().Accounts[id].Usage.Requests; got != 1 {
		t.Fatalf("附加记录不应重复计入请求数：got %d", got)
	}

	// 另一个 attempt 必须独立减计数。
	p.Store().InflightInc(id)
	usage("sel-2", "req-3")
	if got := p.Store().Snapshot().Inflight[id]; got != 0 {
		t.Fatalf("第二次 attempt 应独立归零：inflight=%d", got)
	}
}
