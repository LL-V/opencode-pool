package plugin

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// 宿主 wire 回归（deep-check P0）：ManagementRequest/Response 无 json tags，
// 字段名是 Method/Path/Query/Body；Body 是 base64 字符串（[]byte 编码）。
// 插件两侧必须用 Go 名收发，否则管理面全部 400/空响应。
func TestManagementWireRoundTrip(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)

	// 宿主格式：创建账号（body 为 base64 的 JSON 字符串）。
	body := base64.StdEncoding.EncodeToString([]byte(`{"name":"w@x.com","api_key":"oc_sk_wire_wwwwwwwwwwwwwww"}`))
	raw, _ := json.Marshal(map[string]any{
		"Method": "POST",
		"Path":   "/v0/management/plugins/opencode-pool/accounts",
		"Query":  map[string][]string{},
		"Body":   body,
	})
	out, err := p.HandleMethod(pluginabi.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("宿主 wire 创建应 ok：%s", out)
	}
	var resp struct {
		StatusCode int
		Body       []byte // base64 回来
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("响应应为宿主字段名（StatusCode/Body）：%s", env.Result)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode=200：got %d", resp.StatusCode)
	}
	var created struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(resp.Body, &created); err != nil {
		t.Fatalf("Body 应回 base64 的账号 JSON：%s", env.Result)
	}
	if created.Label != "w@x.com" {
		t.Fatalf("label 应为账户名：%q", created.Label)
	}

	// 宿主格式：详情（query id）。
	q := url.Values{"id": []string{created.ID}}
	raw, _ = json.Marshal(map[string]any{
		"Method": "GET",
		"Path":   "/v0/management/plugins/opencode-pool/account",
		"Query":  q,
	})
	out, _ = p.HandleMethod(pluginabi.MethodManagementHandle, raw)
	_ = json.Unmarshal(out, &env)
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "w@x.com") {
		t.Fatalf("固定路径详情应可达（宿主精确匹配拒绝 {id}）：%d %s", resp.StatusCode, resp.Body)
	}

	// resource 外壳：/status 返回面板 HTML。
	raw, _ = json.Marshal(map[string]any{
		"Method": "GET",
		"Path":   "/v0/resource/plugins/opencode-pool/status",
	})
	out, _ = p.HandleMethod(pluginabi.MethodManagementHandle, raw)
	_ = json.Unmarshal(out, &env)
	var htmlResp struct {
		StatusCode int
		Body       []byte
	}
	_ = json.Unmarshal(env.Result, &htmlResp)
	if htmlResp.StatusCode != 200 || !strings.Contains(string(htmlResp.Body), "<!doctype html>") {
		t.Fatalf("/status resource 应返回面板 HTML：%d %.80s", htmlResp.StatusCode, htmlResp.Body)
	}
}
