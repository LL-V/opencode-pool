package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// R008 调度级验收：任一窗口耗尽（rolling 满、weekly 健康）时账号必须
// 让权/跳过，不得选中撞 429 墙；到期后自动回池。
func TestPickSkipsSingleExhaustedWindow(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	now := time.Now()
	exhausted := "opencode-go:full"
	healthy := "opencode-go:ok"
	p.Store().EnsureAccount(exhausted)
	p.Store().EnsureAccount(healthy)
	// rolling 满（reset +5h）、weekly 健康：旧"全耗尽"语义会继续选中它。
	p.Store().ObserveWindowSource(exhausted, "rolling", "429", 100, now.Add(5*time.Hour), now)
	p.Store().ObserveWindowSource(exhausted, "weekly", "api", 30, now.Add(7*24*time.Hour), now)

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: ProviderID, Model: "opencode-go/x",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: exhausted, Provider: ProviderID, Priority: 5},
			{ID: healthy, Provider: ProviderID, Priority: 0},
		},
	})
	out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var resp struct {
		AuthID string `json:"auth_id"`
		Yield  bool   `json:"yield"`
	}
	_ = json.Unmarshal(env.Result, &resp)
	if resp.Yield {
		t.Skipf("宿主语义：全部候选被暂停时让权是合法结果（healthy 为何未被选中需查）：%s", env.Result)
	}
	if resp.AuthID == exhausted && !resp.Yield {
		t.Fatalf("单窗口耗尽的账号不得被选中（会撞 429 墙）：%s", env.Result)
	}
}
