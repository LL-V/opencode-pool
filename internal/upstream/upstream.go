// Package upstream normalizes the OpenCode Go upstream base URL and joins
// endpoint paths per DESIGN.md M2 (guards the /zen/go/usage 404 trap).
package upstream

import "strings"

// DefaultBase is the official OpenCode Go API base.
const DefaultBase = "https://opencode.ai/zen/go/v1"

// NormalizeBase returns a canonical base ending in /v1 without a trailing
// slash. Empty input yields the official default. A base carrying a query
// string or fragment is rejected to the default — appending endpoint paths
// into a query value would 404 upstream (deep-check P2).
func NormalizeBase(base string) string {
	base = strings.TrimSpace(base)
	if base == "" || strings.ContainsAny(base, "?#") {
		return DefaultBase
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	return base + "/v1"
}

// Join appends an endpoint path to a (possibly unnormalized) base.
func Join(base, path string) string {
	return NormalizeBase(base) + "/" + strings.TrimLeft(path, "/")
}
