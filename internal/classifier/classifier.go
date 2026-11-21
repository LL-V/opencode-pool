// Package classifier sorts upstream failures into the categories defined by
// DESIGN.md M5 and maps them onto the host error-code protocol.
package classifier

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Category is the classification outcome for one upstream response.
type Category string

const (
	CategoryNone           Category = ""
	CategoryConcurrency    Category = "concurrency"
	CategoryQuota          Category = "quota"
	CategoryUnknown429     Category = "unknown429"
	CategoryAuthInvalid    Category = "auth_invalid" // 401
	CategoryWAFBlock       Category = "waf_block"    // 403 Cloudflare
	CategoryAuthSuspect    Category = "auth_suspect" // 403 other
	CategoryTransient      Category = "transient"    // 5xx / 408
	CategorySessionMissing Category = "session_missing"
)

// Host error protocol codes (pluginabi/types.go:201-234). Retryable is
// display-only; routing and cooldown are driven by Code + HTTPStatus +
// RetryAfterSeconds + CredentialScoped.
const (
	CodeTransientRateLimit = "transient_rate_limit"
	CodeCredentialQuota    = "credential_quota"
	CodeRequestScoped      = "request_scoped"
)

// retryAfterFloorForQuota: a bare Retry-Only header below this many seconds is
// treated as UNKNOWN429 rather than quota exhaustion (short values usually
// mean concurrency/transient throttling).
const retryAfterFloorForQuota = 120 * time.Second

// Result carries the classification plus parsed signals.
type Result struct {
	Category Category
	Limit    int       // learned concurrency limit (CONCURRENCY)
	Window   string    // five_hour | weekly | monthly (QUOTA)
	ResetAt  time.Time // absolute recovery time (QUOTA)
}

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		ResetAt string `json:"reset_at"`
		Resets  string `json:"resets_at"`
	} `json:"error"`
	ResetAt string `json:"reset_at"`
	Resets  string `json:"resets_at"`
}

var (
	limitRe       = regexp.MustCompile(`current limit:\s*(\d+)`)
	concurrencyRe = regexp.MustCompile(`concurr|parallel|simultaneous`)
	quotaWordsRe  = regexp.MustCompile(`usage_limit_reached|usage limit`)
	// 窗口名与 /zen/go/v1/usage 的真实键一致（rolling/weekly/monthly，
	// 2026-11-21 实测），429 被动标记与 API 主动拉取才能按同名窗口合并。
	windowRes = []struct{ word, name string }{
		{"5-hour", "rolling"},
		{"5 hour", "rolling"},
		{"5hour", "rolling"},
		{"five hour", "rolling"},
		{"five-hour", "rolling"},
		{"rolling", "rolling"},
		{"7-day", "weekly"},
		{"7 day", "weekly"},
		{"weekly", "weekly"},
		{"monthly", "monthly"},
		// "month" 裸词会误吞 "months ago" 之类文案（deep-check P2）；
		// monthly 已由 "monthly" 覆盖，删裸词。
	}
)

// Classify inspects an upstream response. body may be nil.
func Classify(status int, body []byte, headers http.Header) Result {
	if status == 200 || status < 300 && status > 0 {
		return Result{Category: CategoryNone}
	}
	var eb errorBody
	haveBody := len(body) > 0 && json.Valid(body)
	if haveBody {
		_ = json.Unmarshal(body, &eb)
	}
	msg := eb.Error.Message
	blob := string(body)

	switch {
	case status == 429:
		lmsg := strings.ToLower(msg)
		// 并发判定要求强信号（deep-check P1）：type=rate_limit_error 或显式
		// "current limit: N"。裸词表命中（如限额文案里出现 concurrently）
		// 不再判并发——误判会把真限额当 2 秒瞬时错误无限重试。
		if eb.Error.Type == "rate_limit_error" && concurrencyRe.MatchString(lmsg) {
			// type=rate_limit_error 是强信号：message 内的并发词表可信。
			return Result{Category: CategoryConcurrency, Limit: parseLimit(msg)}
		}
		if parseLimit(msg) > 0 && (eb.Error.Type == "rate_limit_error" || concurrencyRe.MatchString(lmsg)) {
			// "current limit: N" 短语本身是并发语义（限额上限不会这么表述）；
			// token 量级数字由 parseLimit 的 ≤64 上限挡住。
			return Result{Category: CategoryConcurrency, Limit: parseLimit(msg)}
		}
		if matched, win := matchWindow(lmsg); matched {
			return quotaResult(win, eb, headers, time.Now())
		}
		if eb.Error.Type == "usage_limit_reached" || quotaWordsRe.MatchString(lmsg) {
			return quotaResult("", eb, headers, time.Now())
		}
		if ra := retryAfter(headers); ra >= retryAfterFloorForQuota {
			return quotaResult("", eb, headers, time.Now())
		}
		return Result{Category: CategoryUnknown429}
	case status == 401:
		return Result{Category: CategoryAuthInvalid}
	case status == 403:
		lblob := strings.ToLower(blob)
		if strings.Contains(lblob, "cloudflare") || strings.Contains(lblob, "error code: 1010") {
			return Result{Category: CategoryWAFBlock}
		}
		return Result{Category: CategoryAuthSuspect}
	case status == 400:
		if strings.Contains(strings.ToLower(blob), "session") {
			return Result{Category: CategorySessionMissing}
		}
		return Result{Category: CategoryNone}
	case status == 408 || status >= 500:
		return Result{Category: CategoryTransient}
	}
	return Result{Category: CategoryNone}
}

// HostError builds the error to return to the host pipeline per the M5 table.
// Construction guards: transient_rate_limit must never carry CredentialScoped
// and must always set an explicit RetryAfterSeconds.
func (r Result) HostError() HostError {
	switch r.Category {
	case CategoryConcurrency:
		return HostError{Code: CodeTransientRateLimit, HTTPStatus: 429, RetryAfterSeconds: 2}
	case CategoryQuota:
		secs := int(time.Until(r.ResetAt).Seconds())
		if secs < 0 {
			secs = 0
		}
		if secs < 10 { // host minQuotaCooldownFloor
			secs = 10
		}
		return HostError{Code: CodeCredentialQuota, HTTPStatus: 429, RetryAfterSeconds: secs, CredentialScoped: true}
	case CategoryUnknown429:
		return HostError{Code: CodeTransientRateLimit, HTTPStatus: 429, RetryAfterSeconds: 3}
	case CategoryWAFBlock:
		return HostError{Code: CodeRequestScoped, HTTPStatus: 403}
	case CategorySessionMissing:
		return HostError{Code: CodeRequestScoped, HTTPStatus: 400}
	case CategoryAuthInvalid, CategoryAuthSuspect, CategoryTransient:
		status := 403
		if r.Category == CategoryAuthInvalid {
			status = 401
		}
		if r.Category == CategoryTransient {
			status = 503
		}
		return HostError{HTTPStatus: status}
	}
	return HostError{}
}

// HostError is the wire form of the host plugin error envelope fields.
type HostError struct {
	Code              string `json:"code,omitempty"`
	Message           string `json:"message,omitempty"`
	Retryable         bool   `json:"retryable"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped  bool   `json:"credential_scoped,omitempty"`
}

func parseLimit(s string) int {
	m := limitRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	// 运营观测的并发上限是个位数（3/5/6）；"current limit: 100000 tokens"
	// 之类的 token 限额混入时拒绝学习，防止污染 busy 判定。
	if n > 64 {
		return 0
	}
	return n
}

func matchWindow(msg string) (bool, string) {
	l := strings.ToLower(msg)
	for _, w := range windowRes {
		if strings.Contains(l, w.word) {
			return true, w.name
		}
	}
	return false, ""
}

func quotaResult(window string, eb errorBody, headers http.Header, now time.Time) Result {
	r := Result{Category: CategoryQuota, Window: window}
	for _, raw := range []string{eb.ResetAt, eb.Resets, eb.Error.ResetAt, eb.Error.Resets} {
		if raw == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			r.ResetAt = ts
			return r
		}
	}
	if ra := retryAfter(headers); ra > 0 {
		r.ResetAt = now.Add(ra)
		return r
	}
	// Conservative window-type defaults when no reset source is available.
	switch window {
	case "weekly":
		r.ResetAt = now.Add(7 * 24 * time.Hour)
	case "monthly":
		r.ResetAt = now.Add(30 * 24 * time.Hour)
	default: // rolling or unrecognized
		r.ResetAt = now.Add(5 * time.Hour)
	}
	return r
}

func retryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	v := headers.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 0
}
