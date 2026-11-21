package plugin

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// 真实 Go 用量响应形态（2026-11-21 实测）
const realUsageBody = `{"usage":{"rolling":{"status":"ok","percent":41,"resetsAt":"2026-11-21T07:12:21.287Z"},"weekly":{"status":"ok","percent":12,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"limited","percent":100,"resetsAt":"2026-12-20T22:32:56.000Z"}}}`

func TestParseUsageResponse(t *testing.T) {
	wins := parseUsageResponse([]byte(realUsageBody))
	if len(wins) != 3 {
		t.Fatalf("应解析三窗口：got %d", len(wins))
	}
	r := wins["rolling"]
	if r.UsedPct != 41 || r.Source != "api" {
		t.Fatalf("rolling：got %+v", r)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-11-21T07:12:21.287Z")
	if !r.Until.Equal(want) {
		t.Fatalf("resetsAt 解析：got %v want %v", r.Until, want)
	}
	if wins["monthly"].UsedPct != 100 {
		t.Fatalf("monthly percent=100：got %+v", wins["monthly"])
	}
	// 空值/畸形容错
	if got := parseUsageResponse([]byte(`{}`)); len(got) != 0 {
		t.Fatalf("空对象应返回空：got %d", len(got))
	}
	if got := parseUsageResponse(nil); len(got) != 0 {
		t.Fatalf("nil 应返回空")
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"oc_sk_sample111111_Aaaaaaaaaaaaaaaaaa_Bbbbbbbbbbbbbbbb", "oc_sk_sample…bbbb"},
		{"short", "s…t"},
		{"ab", "••••"},
		{"", ""},
	}
	for _, c := range cases {
		if got := maskKey(c.in); got != c.want {
			t.Fatalf("maskKey(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestAccountHasNameAndMaskedKey(t *testing.T) {
	host := &fakeHost{}
	p := newTestPlugin(t, host)
	call(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": ""})

	// 创建账户：账户名（邮箱）+ api_key
	body := `{"name":"demo@example.com","api_key":"oc_sk_sample111111_Aaaaaaaaaaaaaaaaaa_Bbbbbbbbbbbbbbbb"}`
	resp := p.HandleManagement("POST", "/accounts", nil, strings.NewReader(body))
	var created struct {
		ID           string `json:"id"`
		Label        string `json:"label"`
		APIKeyMasked string `json:"api_key_masked"`
	}
	if err := json.Unmarshal(resp.Body, &created); err != nil {
		t.Fatalf("创建响应：%v %s", err, resp.Body)
	}
	if created.Label != "demo@example.com" {
		t.Fatalf("账户名应显示邮箱：got %q", created.Label)
	}
	if created.APIKeyMasked != "oc_sk_sample…bbbb" {
		t.Fatalf("脱敏 key：got %q", created.APIKeyMasked)
	}
	// 物化时 name 写入 storage 并带 label
	if len(host.saved) == 0 {
		t.Fatal("应经 host.auth.save 物化")
	}
	saved := host.saved[0]
	raw, _ := saved["json"].(json.RawMessage)
	if !strings.Contains(string(raw), `"label":"demo@example.com"`) || !strings.Contains(string(raw), `"name":"demo@example.com"`) {
		t.Fatalf("auth 文件应含账户名与 label：%s", raw)
	}
	// 详情也带脱敏 key
	detail := p.HandleManagement("GET", "/account", url.Values{"id": []string{created.ID}}, nil)
	var dv struct {
		APIKeyMasked string `json:"api_key_masked"`
	}
	_ = json.Unmarshal(detail.Body, &dv)
	if dv.APIKeyMasked == "" {
		t.Fatalf("详情应含脱敏 key：%s", detail.Body)
	}
}
