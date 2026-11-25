package classifier

import (
	"net/http"
	"testing"
)

// 上游给了 Retry-After 时，并发/未知 429 必须采用它，而不是固定的 2/3 秒，
// 否则宿主会在同一个限流窗口里反复重试放大。
func TestRetryAfterHintOnConcurrencyAndUnknown(t *testing.T) {
	h := http.Header{"Retry-After": []string{"7"}}

	r := Classify(429, []byte(`{"error":{"type":"rate_limit_error","message":"reached the concurrency limit"}}`), h)
	if r.Category != CategoryConcurrency {
		t.Fatalf("应判并发型：%+v", r)
	}
	if he := r.HostError(); he.RetryAfterSeconds != 7 {
		t.Fatalf("并发 429 应沿用 Retry-After=7：%+v", he)
	}

	r = Classify(429, []byte(`{"error":{"message":"slow down"}}`), h)
	if r.Category != CategoryUnknown429 {
		t.Fatalf("应判未知型：%+v", r)
	}
	if he := r.HostError(); he.RetryAfterSeconds != 7 {
		t.Fatalf("未知 429 应沿用 Retry-After=7：%+v", he)
	}

	// 无提示时保持快速重试默认值。
	r = Classify(429, []byte(`{"error":{"type":"rate_limit_error","message":"concurrency limit"}}`), nil)
	if he := r.HostError(); he.RetryAfterSeconds != 2 {
		t.Fatalf("无 Retry-After 应保持默认 2s：%+v", he)
	}
}

// 真实上游常见 "Current limit: 6" 大小写形态，必须学习到上限。
func TestParseLimitCaseInsensitive(t *testing.T) {
	r := Classify(429, []byte(`{"error":{"type":"rate_limit_error","message":"Current limit: 6"}}`), nil)
	if r.Category != CategoryConcurrency || r.Limit != 6 {
		t.Fatalf("应学习 limit=6：%+v", r)
	}
}
