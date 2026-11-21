package upstream

import "testing"

// 官方 2026-11 文档：Go base=/inference/go；三个 API 各自带 /openai|/anthropic + /v1。
func TestEndpointPerFormat(t *testing.T) {
	cases := []struct{ format, want string }{
		{"claude", "https://opencode.ai/inference/go/anthropic/v1/messages"},
		{"openai", "https://opencode.ai/inference/go/openai/v1/chat/completions"},
		{"openai-response", "https://opencode.ai/inference/go/openai/v1/responses"},
	}
	for _, tc := range cases {
		if got := Endpoint("", tc.format); got != tc.want {
			t.Fatalf("Endpoint(\"\",%q)=%q want %q", tc.format, got, tc.want)
		}
	}
}

func TestLegacyZenBaseMigrates(t *testing.T) {
	for _, base := range []string{
		"https://opencode.ai/zen/go",
		"https://opencode.ai/zen/go/v1",
		"https://opencode.ai/zen/go/v1/",
	} {
		if got := Endpoint(base, "openai"); got != "https://opencode.ai/inference/go/openai/v1/chat/completions" {
			t.Fatalf("旧 base(%q) 应迁移：got %q", base, got)
		}
	}
}

func TestBaseNormalization(t *testing.T) {
	if got := NormalizeBase(""); got != DefaultBase {
		t.Fatalf("默认 base：got %q", got)
	}
	if got := NormalizeBase("https://opencode.ai/inference/go/"); got != DefaultBase {
		t.Fatalf("去尾斜杠：got %q", got)
	}
	if got := NormalizeBase("https://mirror.example.com/api/v1"); got != "https://mirror.example.com/api" {
		t.Fatalf("自定义 base 去掉版本段：got %q", got)
	}
	if got := NormalizeBase("https://opencode.ai/inference?v=1"); got != DefaultBase {
		t.Fatalf("query 应回退默认：got %q", got)
	}
}

func TestUsageAndModelsURL(t *testing.T) {
	if got := UsageURL(""); got != "https://opencode.ai/inference/go/v1/usage" {
		t.Fatalf("usage：got %q", got)
	}
	if got := ModelsURL(""); got != "https://opencode.ai/inference/go/v1/models" {
		t.Fatalf("models：got %q", got)
	}
	if got := UsageURL("https://opencode.ai/zen/go/v1"); got != "https://opencode.ai/inference/go/v1/usage" {
		t.Fatalf("旧 base 的 usage 应迁移：got %q", got)
	}
}
