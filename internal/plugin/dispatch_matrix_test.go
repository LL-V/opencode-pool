package plugin

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 调度状态矩阵穷举：对账号状态组合（含绑定）逐格跑 Pick，验证不变量：
// ① 选中者永远不是 bad（signin/单窗耗尽）② 绑定健康必选绑定
// ③ 全健康无信号时让权（合同设计：无约束不干预宿主轮询）
// ④ 本池候选存在 bad 时仍由插件接管：全健康也选号，全坏才 Reject。
func TestSchedulerStateMatrix(t *testing.T) {
	now := time.Now()
	// 每种状态构造函数：给账号打上状态
	states := map[string]func(p *Plugin, id string){
		"active": func(p *Plugin, id string) { p.Store().EnsureAccount(id) },
		"busy": func(p *Plugin, id string) {
			p.Store().EnsureAccount(id)
			p.Store().LearnLimit(id, 2)
			p.Store().InflightInc(id)
			p.Store().InflightInc(id)
		},
		"near": func(p *Plugin, id string) {
			p.Store().EnsureAccount(id)
			p.Store().ObserveWindowSource(id, "rolling", "api", 95, now.Add(time.Hour), now)
		},
		"exhausted": func(p *Plugin, id string) {
			p.Store().EnsureAccount(id)
			p.Store().ObserveWindowSource(id, "rolling", "429", 100, now.Add(5*time.Hour), now)
			p.Store().ObserveWindowSource(id, "weekly", "api", 30, now.Add(7*24*time.Hour), now)
		},
		"signin": func(p *Plugin, id string) {
			p.Store().EnsureAccount(id)
			p.Store().SetRequiresSignIn(id, true)
		},
		"highload": func(p *Plugin, id string) {
			p.Store().EnsureAccount(id)
			p.Store().InflightInc(id)
			p.Store().InflightInc(id)
			p.Store().InflightInc(id)
		},
	}
	badStates := map[string]bool{"exhausted": true, "signin": true}

	names := make([]string, 0, len(states))
	for k := range states {
		names = append(names, k)
	}

	for i, a := range names {
		for j, b := range names {
			// 两账号池（相同状态时只测一格）
			if i > j {
				continue
			}
			t.Run(a+"+"+b, func(t *testing.T) {
				host := &fakeHost{}
				p := newTestPlugin(t, host)
				idA, idB := "opencode-go:acctA", "opencode-go:acctB"
				states[a](p, idA)
				states[b](p, idB)

				raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
					Provider: ProviderID, Model: "opencode-go/x",
					Candidates: []pluginapi.SchedulerAuthCandidate{
						{ID: idA, Provider: ProviderID, Priority: 1},
						{ID: idB, Provider: ProviderID, Priority: 1},
					},
				})
				out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
				if err != nil {
					t.Fatal(err)
				}
				var env pluginabi.Envelope
				_ = json.Unmarshal(out, &env)
				var resp struct {
					AuthID  string
					Handled bool
					Reject  bool
				}
				_ = json.Unmarshal(env.Result, &resp)

				if resp.AuthID == "" {
					// 让权/拒绝：存在 bad 信号时不得静默让权回坏池。
					if badStates[a] || badStates[b] {
						if !resp.Handled || !resp.Reject {
							t.Fatalf("存在 bad 候选 %s+%s 时必须 Handled+Reject：%s", a, b, env.Result)
						}
					}
					// 全健康无信号让权=合同设计，合法。
					return
				}
				gotState := a
				if resp.AuthID == idB {
					gotState = b
				} else if resp.AuthID != idA {
					t.Fatalf("选中了未知账号 %q", resp.AuthID)
				}
				if badStates[gotState] {
					t.Fatalf("选中了 bad 状态账号（%s）：%s", gotState, env.Result)
				}
			})
		}
	}

	// 绑定不变量：绑定号健康时必选绑定（即使优先级低、其他号更闲）。
	t.Run("binding-wins", func(t *testing.T) {
		host := &fakeHost{}
		p := newTestPlugin(t, host)
		bound, other := "opencode-go:bound", "opencode-go:other"
		states["active"](p, bound)
		states["active"](p, other)
		p.Store().Bind("sid-m", bound, now)
		raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: "opencode-go/x",
			Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"x-opencode-session": {"sid-m"}}},
			Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: other, Provider: ProviderID, Priority: 9},
				{ID: bound, Provider: ProviderID, Priority: 0},
			},
		})
		out, _ := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
		var env pluginabi.Envelope
		_ = json.Unmarshal(out, &env)
		if !contains(fmt.Sprintf("%s", env.Result), bound) {
			t.Fatalf("健康绑定号必须胜出：%s", env.Result)
		}
	})

	// 绑定 bad（signin）时不选绑定（亲和先于让权，但 bad 绝不选）。
	t.Run("binding-bad-falls-back", func(t *testing.T) {
		host := &fakeHost{}
		p := newTestPlugin(t, host)
		bound, other := "opencode-go:bound2", "opencode-go:other2"
		states["signin"](p, bound)
		states["active"](p, other)
		p.Store().Bind("sid-n", bound, now)
		raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: "opencode-go/x",
			Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"x-opencode-session": {"sid-n"}}},
			Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: bound, Provider: ProviderID, Priority: 9},
				{ID: other, Provider: ProviderID, Priority: 0},
			},
		})
		out, _ := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
		var env pluginabi.Envelope
		_ = json.Unmarshal(out, &env)
		var resp struct {
			AuthID string
			Yield  bool
		}
		_ = json.Unmarshal(env.Result, &resp)
		if resp.AuthID == bound {
			t.Fatalf("绑定号 bad 绝不能选：%s", env.Result)
		}
	})
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
