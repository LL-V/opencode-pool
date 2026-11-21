// Package upstream normalizes the OpenCode inference base URL and builds the
// per-API endpoint paths.
//
// Official docs (https://opencode.ai/docs/console/harnesses#base-urls):
//   - Pay-as-you-go base: https://opencode.ai/inference
//   - Go plan base:        https://opencode.ai/inference/go
//   - OpenAI Responses:    /openai/v1/responses
//   - Chat Completions:    /openai/v1/chat/completions
//   - Anthropic Messages:  /anthropic/v1/messages
//
// The older /zen/go/v1 endpoints still answer but are the legacy surface; a
// Messages request sent there with a Bearer header is rejected as
// "OpenCode Go requires an API key" (verified 2026-11-21).
package upstream

import (
	"net/url"
	"strings"
)

// DefaultBase is the official Go-plan inference base.
const DefaultBase = "https://opencode.ai/inference/go"

// NormalizeBase returns a plan base without a trailing slash or trailing
// /v1 (endpoint paths already carry their own version). Empty or malformed
// input yields the official Go base. Legacy opencode.ai/zen/go bases are
// migrated to /inference/go.
func NormalizeBase(base string) string {
	base = strings.TrimSpace(base)
	if base == "" || strings.ContainsAny(base, "?#") {
		return DefaultBase
	}
	base = strings.TrimRight(base, "/")
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		if strings.EqualFold(u.Host, "opencode.ai") && strings.HasPrefix(u.Path, "/zen/go") {
			u.Path = strings.TrimRight(strings.Replace(u.Path, "/zen/go", "/inference/go", 1), "/")
			base = strings.TrimRight(u.String(), "/")
		}
	}
	base = strings.TrimSuffix(base, "/v1")
	return strings.TrimRight(base, "/")
}

// Endpoint builds the upstream URL for one negotiated request format.
// format is the request-body format (claude / openai / openai-response).
func Endpoint(base, format string) string {
	b := NormalizeBase(base)
	switch format {
	case "claude":
		return b + "/anthropic/v1/messages"
	case "openai-response":
		return b + "/openai/v1/responses"
	default:
		return b + "/openai/v1/chat/completions"
	}
}

// UsageURL is the Go-plan quota endpoint (GET, Bearer key).
// Verified 2026-11-21: returns 401 AuthError without a key (route exists),
// while the legacy /zen/go/v1/usage is 404.
func UsageURL(base string) string {
	return NormalizeBase(base) + "/v1/usage"
}

// ModelsURL is the public model catalog under the same plan base.
func ModelsURL(base string) string {
	return NormalizeBase(base) + "/v1/models"
}
