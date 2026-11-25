package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// 用户 2026-11-25：请求 claude / openai-compatible 模型时，宿主同样会调用
// 本插件的选号；候选全是别的 provider 时插件必须让权（Handled=false），
// 不能 Reject——宿主只认 Reject，会直接回 503 auth_unavailable。
func TestPickYieldsForForeignProviders(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})

	cases := []pluginapi.SchedulerPickRequest{
		{
			Provider: "claude", Model: "deepseek-flash",
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "claude:apikey:abc", Provider: "claude", Priority: 1}},
		},
		{
			// 老宿主可能不带 Provider 字段：仍以候选 provider 判断。
			Provider: "",
			Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: "openai:apikey:def", Provider: "openai", Priority: 1},
				{ID: "gemini:apikey:ghi", Provider: "gemini", Priority: 2},
			},
		},
	}
	for i, req := range cases {
		raw, _ := json.Marshal(req)
		out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var env pluginabi.Envelope
		if err := json.Unmarshal(out, &env); err != nil || !env.OK {
			t.Fatalf("case %d envelope: %v %s", i, err, string(out))
		}
		var resp pluginapi.SchedulerPickResponse
		_ = json.Unmarshal(env.Result, &resp)
		if resp.Handled || resp.Reject || resp.AuthID != "" {
			t.Fatalf("case %d：非本池请求必须让权，got %+v", i, resp)
		}
	}
}

// 反向保护：本池候选全部不可用时仍必须 Handled+Reject，避免宿主回退选中
// 已限额/需登录/已删除的账号。
func TestPickStillRejectsWhenOwnCandidatesAllBad(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})

	id := "opencode-go:deadbeefcafe"
	p.Store().EnsureAccount(id)
	p.Store().SetRequiresSignIn(id, true)

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: ProviderID, Model: "opencode-go/x",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: id, Provider: ProviderID, Priority: 1}},
	})
	out, err := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var resp pluginapi.SchedulerPickResponse
	_ = json.Unmarshal(env.Result, &resp)
	if !resp.Handled || !resp.Reject {
		t.Fatalf("本池候选全坏时必须 Handled+Reject：got %+v", resp)
	}
}

// mixed 请求里本池候选全坏、但还有其他 provider 的候选：必须让权并排除
// 本池坏号，而不是 Reject 把整个请求（其他 provider 还有健康号）打死。
func TestPickMixedExcludesBadPoolAndYields(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	own := addAccount(t, p, "bad@x.com", "oc_sk_bad_0000000000000000000000")
	p.Store().PurgeAccount(own)

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "", Model: "shared-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: own, Provider: ProviderID, Priority: 1},
			{ID: "claude:apikey:healthy", Provider: "claude", Priority: 1},
		},
	})
	out, _ := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	var resp pluginapi.SchedulerPickResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.Handled || resp.Reject {
		t.Fatalf("mixed 本池全坏应让权：got %+v", resp)
	}
	found := false
	for _, id := range resp.ExcludeCandidates {
		if id == own {
			found = true
		}
	}
	if !found {
		t.Fatalf("应排除本池坏号：%+v", resp)
	}
}

// 候选缺 Provider（老宿主）时：请求未指明本池（mixed）不得认领；请求明确
// 是本池时仍可正常选号。
func TestPickCandidateWithoutProviderField(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})
	id := addAccount(t, p, "legacy@x.com", "oc_sk_legacy_000000000000000000")

	pick := func(reqProvider string) pluginapi.SchedulerPickResponse {
		raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
			Provider: reqProvider, Model: "x",
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: id}},
		})
		out, _ := p.HandleMethod(pluginabi.MethodSchedulerPick, raw)
		var env pluginabi.Envelope
		_ = json.Unmarshal(out, &env)
		var resp pluginapi.SchedulerPickResponse
		_ = json.Unmarshal(env.Result, &resp)
		return resp
	}

	if resp := pick(""); resp.Handled {
		t.Fatalf("mixed 且候选无 Provider 必须让权：%+v", resp)
	}
	if resp := pick(ProviderID); !resp.Handled || resp.AuthID != id {
		t.Fatalf("明确本池请求应正常选号：%+v", resp)
	}
}
