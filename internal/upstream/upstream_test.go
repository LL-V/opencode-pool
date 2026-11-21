package upstream

import "testing"

func TestJoinEndpoint(t *testing.T) {
	cases := []struct {
		name, base, path, want string
	}{
		{"标准 base", "https://opencode.ai/zen/go/v1", "chat/completions",
			"https://opencode.ai/zen/go/v1/chat/completions"},
		{"尾部斜杠", "https://opencode.ai/zen/go/v1/", "usage",
			"https://opencode.ai/zen/go/v1/usage"},
		{"★无 /v1 的 base + usage（404 陷阱）", "https://opencode.ai/zen/go", "usage",
			"https://opencode.ai/zen/go/v1/usage"},
		{"无 /v1 的 base + chat", "https://opencode.ai/zen/go", "responses",
			"https://opencode.ai/zen/go/v1/responses"},
		{"自定义 base 保留", "https://mirror.example.com/api/v1", "models",
			"https://mirror.example.com/api/v1/models"},
		{"自定义 base 无 /v1 不强加 zen 路径", "https://mirror.example.com/api", "models",
			"https://mirror.example.com/api/v1/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Join(tc.base, tc.path); got != tc.want {
				t.Fatalf("Join(%q,%q) = %q, want %q", tc.base, tc.path, got, tc.want)
			}
		})
	}
}

func TestJoinDefaultBase(t *testing.T) {
	want := "https://opencode.ai/zen/go/v1/models"
	if got := Join("", "models"); got != want {
		t.Fatalf("空 base 应使用默认：got %q", got)
	}
}

func TestBaseNormalization(t *testing.T) {
	if got := NormalizeBase(""); got != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("默认 base：got %q", got)
	}
	if got := NormalizeBase("https://opencode.ai/zen/go/v1/"); got != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("去尾斜杠：got %q", got)
	}
	if got := NormalizeBase("https://opencode.ai/zen/go"); got != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("补 /v1：got %q", got)
	}
}
