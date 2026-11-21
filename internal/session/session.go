// Package session resolves and derives the upstream conversation session id
// (x-opencode-session) per DESIGN.md M3.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// headerSources lists inbound client session headers in priority order.
// X-Client-Request-Id is deliberately absent: it is unique per request and
// would defeat upstream prompt caching.
var headerSources = []string{
	"x-opencode-session",
	"session-id",
	"x-claude-code-session-id",
	"thread-id",
	"x-deepseek-harness-session-id",
	"x-session-id",
}

const maxSessionIDLen = 512

// Resolve returns the session id for a request: client headers first, then
// documented body fields, then a derivation stable per conversation. A nil
// header and nil body (plugin-initiated probe) yields a fresh UUID.
func Resolve(h http.Header, body []byte) string {
	return ResolveWithSources(h, body, nil)
}

// ResolveWithSources is Resolve with the operator-configured header priority
// list (ConfigField session_header_sources). Empty keeps the built-in list.
func ResolveWithSources(h http.Header, body []byte, sources []string) string {
	list := headerSources
	if len(sources) > 0 {
		list = sources
	}
	if h != nil {
		for _, src := range list {
			src = strings.TrimSpace(src)
			if src == "" {
				continue
			}
			if v := sanitize(h.Get(src)); v != "" {
				return v
			}
		}
	}
	if sid := bodyField(body); sid != "" {
		return sid
	}
	if len(body) > 0 {
		return derive(body)
	}
	return newProbeID()
}

// sanitize trims and rejects malformed session ids (empty, oversized, or
// containing header-unsafe control characters).
func sanitize(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxSessionIDLen {
		return ""
	}
	if strings.ContainsAny(v, "\r\n\x00") {
		return ""
	}
	return v
}

// bodyField reads documented stable conversation fields: OpenAI-style
// prompt_cache_key, then Anthropic-style metadata.user_id (which may itself be
// JSON containing session_id).
func bodyField(body []byte) string {
	if len(body) == 0 || !json.Valid(body) {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	if v, ok := root["prompt_cache_key"].(string); ok {
		if s := sanitize(v); s != "" {
			return s
		}
	}
	if meta, ok := root["metadata"].(map[string]any); ok {
		if uid, ok := meta["user_id"].(string); ok && sanitize(uid) != "" {
			uid = strings.TrimSpace(uid)
			if strings.HasPrefix(uid, "{") {
				var inner map[string]any
				if err := json.Unmarshal([]byte(uid), &inner); err == nil {
					if sid, ok := inner["session_id"].(string); ok && sanitize(sid) != "" {
						return sid
					}
				}
			}
			return uid
		}
	}
	return ""
}

// derive produces a stable per-conversation id from the first user message
// (system messages never participate). With no user message it falls back to
// the raw body hash so odd shapes (count_tokens, invalid JSON) stay stable.
type derivedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func derive(body []byte) string {
	var root struct {
		Messages []derivedMessage `json:"messages"`
		// OpenAI Responses API uses input[] (array of messages or a string).
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return hashPrefix("b", body)
	}
	msgs := root.Messages
	if len(msgs) == 0 && len(root.Input) > 0 {
		var text string
		if json.Unmarshal(root.Input, &text) == nil && text != "" {
			return hashPrefix("d", []byte(text))
		}
		var arr []derivedMessage
		if json.Unmarshal(root.Input, &arr) == nil {
			msgs = arr
		}
	}
	if len(msgs) == 0 {
		return hashPrefix("b", body)
	}
	for i, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		text, ok := textOf(msg.Content)
		if ok && text != "" {
			return hashPrefix("d", []byte(text))
		}
		// No usable text: a non-text block (e.g. the image itself) is a stable
		// per-conversation anchor — the same image recurs in every turn of the
		// conversation, so hashing it keeps the key stable AND distinct across
		// different image conversations.
		if anchor := nonTextAnchor(msg.Content); anchor != "" {
			return hashPrefix("d", []byte(anchor))
		}
		// Only LATER USER messages may feed the derivation — assistant replies
		// grow turn by turn, so "any role" would re-key the conversation every
		// turn (deep-check P1). No user anchor at all → empty-string constant.
		for _, later := range msgs[i+1:] {
			if later.Role != "user" {
				continue
			}
			if t, ok := textOf(later.Content); ok && t != "" {
				return hashPrefix("d", []byte(t))
			}
			if anchor := nonTextAnchor(later.Content); anchor != "" {
				return hashPrefix("d", []byte(anchor))
			}
		}
		return hashPrefix("d", []byte(""))
	}
	return hashPrefix("b", body)
}

// nonTextAnchor extracts a stable identity string from the first non-text
// content block (image url / source data / base64 payload).
func nonTextAnchor(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	for _, item := range items {
		if isTextBlock(item) {
			continue
		}
		if u, ok := item["url"].(string); ok && u != "" {
			return "url:" + u
		}
		if src, ok := item["source"].(map[string]any); ok {
			if d, ok := src["data"].(string); ok && d != "" {
				return "data:" + d
			}
			if d, ok := src["base64"].(string); ok && d != "" {
				return "b64:" + d
			}
		}
		if d, ok := item["data"].(string); ok && d != "" {
			return "data:" + d
		}
	}
	return ""
}

// textOf extracts message text: string content directly, or array content
// with text items joined by NUL to prevent cross-item concatenation
// collisions.
func textOf(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", false
	}
	var parts []string
	for _, item := range items {
		if isTextBlock(item) {
			if t, ok := item["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\x00"), true
}

// isTextBlock accepts both chat-completions ("text") and Responses API
// ("input_text"/"output_text") content blocks.
func isTextBlock(item map[string]any) bool {
	switch item["type"] {
	case "text", "input_text", "output_text":
		return true
	}
	return false
}

func hashPrefix(prefix string, data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + "-" + hex.EncodeToString(sum[:])[:16]
}

func newProbeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hashPrefix("p", []byte("probe"))
	}
	return hex.EncodeToString(b[:])
}
