package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 宿主按自己的节奏轮询 quota.fetch；窗口必须固定顺序，否则 map 随机序
// 会让配额面板每次刷新都抖动（与 /accounts 同源问题）。
func TestQuotaFetchDeterministicWindowOrder(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	id := addAccount(t, p, "q@x.com", "oc_sk_quota_0000000000000000000")
	now := time.Now()
	for _, name := range []string{"monthly", "rolling", "weekly"} {
		p.Store().ObserveWindowSource(id, name, "api", 40, now.Add(time.Hour), now)
	}

	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{AuthID: id})
	out, err := p.HandleMethod(pluginabi.MethodQuotaFetch, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var resp pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode quota: %v %s", err, out)
	}
	got := make([]string, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		got = append(got, g.DisplayName)
	}
	want := []string{"rolling", "weekly", "monthly"}
	if len(got) != len(want) {
		t.Fatalf("windows: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("windows: got %v want %v", got, want)
		}
	}
}
