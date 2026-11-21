package classifier

import (
	"net/http"
	"testing"
	"time"
)

// —— 分类（R007）——

func TestClassify(t *testing.T) {
	now := time.Unix(1763700000, 0)
	cases := []struct {
		name    string
		status  int
		body    string
		headers http.Header
		want    Category
	}{
		{"并发型：type+concurrency limit", 429,
			`{"error":{"type":"rate_limit_error","message":"Your workspace has reached the concurrency limit for this model (current limit: 6). Please send fewer requests in parallel."}}`,
			nil, CategoryConcurrency},
		{"并发型：宽词表 parallel", 429,
			`{"error":{"type":"rate_limit_error","message":"too many parallel requests"}}`, nil, CategoryConcurrency},
		{"无 type 裸词→unknown429（deep-check：裸词不判并发防误伤）", 429,
			`{"error":{"message":"simultaneous requests exceeded"}}`, nil, CategoryUnknown429},
		{"限额文案含 concurrently 不误判并发", 429,
			`{"error":{"type":"usage_limit_reached","message":"your weekly limit; requests running concurrently"}}`, nil, CategoryQuota},
		{"限额型：usage_limit_reached", 429,
			`{"error":{"type":"usage_limit_reached","message":"5 hour limit"}}`, nil, CategoryQuota},
		{"限额型：weekly 窗口词", 429,
			`{"error":{"message":"weekly usage limit exceeded"}}`, nil, CategoryQuota},
		{"限额型：monthly 窗口词", 429,
			`{"error":{"message":"You hit your monthly limit"}}`, nil, CategoryQuota},
		{"限额型：5 hour 窗口词", 429,
			`{"error":{"message":"usage limit resets in the 5 hour window"}}`, nil, CategoryQuota},
		{"未知型：无特征", 429, `{"error":{"message":"slow down"}}`, nil, CategoryUnknown429},
		{"护栏：仅 Retry-After 90s 不判限额", 429, `{"error":{"message":"rate limited"}}`,
			hdrOf("Retry-After", "90"), CategoryUnknown429},
		{"护栏通过：仅 Retry-After 300s 判限额", 429, `{"error":{"message":"rate limited"}}`,
			hdrOf("Retry-After", "300"), CategoryQuota},
		{"401", 401, `{}`, nil, CategoryAuthInvalid},
		{"403 CF WAF", 403, `cloudflare error code: 1010`, nil, CategoryWAFBlock},
		{"403 其他", 403, `{"error":{"message":"forbidden"}}`, nil, CategoryAuthSuspect},
		{"503", 503, `upstream unavailable`, nil, CategoryTransient},
		{"408", 408, ``, nil, CategoryTransient},
		{"400 缺会话", 400, `{"error":{"message":"MissingSessionID: x-opencode-session is required"}}`, nil, CategorySessionMissing},
		{"200 不分类", 200, ``, nil, CategoryNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.status, []byte(tc.body), tc.headers)
			if got.Category != tc.want {
				t.Fatalf("Category = %v, want %v", got.Category, tc.want)
			}
		})
	}
	_ = now
}

func TestParseConcurrencyLimit(t *testing.T) {
	body := `{"error":{"type":"rate_limit_error","message":"... (current limit: 6). We may adjust"}}`
	r := Classify(429, []byte(body), nil)
	if r.Limit != 6 {
		t.Fatalf("应解析 current limit: 6，got %d", r.Limit)
	}
}

func TestQuotaResetParsing(t *testing.T) {
	// body 内 reset 字段优先
	body := `{"error":{"type":"usage_limit_reached","message":"5 hour limit","reset_at":"2026-11-21T18:00:00Z"}}`
	r := Classify(429, []byte(body), nil)
	if !r.ResetAt.Equal(time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset_at 解析：got %v", r.ResetAt)
	}
	// 其次 Retry-After（相对秒数）
	r2 := Classify(429, []byte(`{"error":{"message":"weekly usage limit"}}`), hdrOf("Retry-After", "3600"))
	want2 := time.Now().Add(3600 * time.Second)
	diff := r2.ResetAt.Sub(want2)
	if diff < -time.Minute || diff > time.Minute {
		t.Fatalf("Retry-After 相对换算：got %v want≈%v", r2.ResetAt, want2)
	}
}

func TestQuotaWindowFromMessage(t *testing.T) {
	r := Classify(429, []byte(`{"error":{"message":"weekly usage limit exceeded"}}`), nil)
	if r.Window != "weekly" {
		t.Fatalf("窗口词识别：got %q", r.Window)
	}
	// 5 hour → rolling（与真实 /usage 窗口名一致）
	r2 := Classify(429, []byte(`{"error":{"message":"5 hour limit reached"}}`), nil)
	if r2.Window != "rolling" {
		t.Fatalf("5 hour 应映射 rolling：got %q", r2.Window)
	}
}

// —— 宿主错误码构造（§10 防回归断言）——

func TestHostErrorConcurrency(t *testing.T) {
	r := Classify(429, []byte(`{"error":{"message":"concurrency limit (current limit: 3)"}}`), nil)
	he := r.HostError()
	if he.Code != "transient_rate_limit" || he.HTTPStatus != 429 || he.RetryAfterSeconds != 2 {
		t.Fatalf("并发型→transient_rate_limit/429/2s：got %+v", he)
	}
	if he.CredentialScoped {
		t.Fatal("防回归：transient_rate_limit 禁止携带 CredentialScoped")
	}
}

func TestHostErrorQuota(t *testing.T) {
	r := Classify(429, []byte(`{"error":{"type":"usage_limit_reached","message":"5 hour limit","reset_at":"2026-11-21T18:00:00Z"}}`), nil)
	he := r.HostError()
	if he.Code != "credential_quota" || !he.CredentialScoped || he.HTTPStatus != 429 {
		t.Fatalf("限额型→credential_quota/Scoped：got %+v", he)
	}
	if he.RetryAfterSeconds <= 0 {
		t.Fatal("限额型必须带 reset 秒数")
	}
}

func TestHostErrorUnknown429(t *testing.T) {
	r := Classify(429, []byte(`{"error":{"message":"slow down"}}`), nil)
	he := r.HostError()
	if he.Code != "transient_rate_limit" || he.RetryAfterSeconds != 3 || he.CredentialScoped {
		t.Fatalf("未知型→transient_rate_limit/3s/非Scoped：got %+v", he)
	}
}

func TestHostErrorWAF(t *testing.T) {
	r := Classify(403, []byte(`cloudflare error code: 1010`), nil)
	he := r.HostError()
	if he.Code != "request_scoped" || he.HTTPStatus != 403 {
		t.Fatalf("WAF→request_scoped/403：got %+v", he)
	}
}

func TestHostErrorTransientKeepsStatus(t *testing.T) {
	r := Classify(503, nil, nil)
	he := r.HostError()
	if he.HTTPStatus != 503 {
		t.Fatalf("5xx 保留原状态码：got %d", he.HTTPStatus)
	}
}

func hdrOf(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

func TestQuotaWindowDefaultReset(t *testing.T) {
	// 无 body reset、无 Retry-After → 按窗口类型保守默认
	r := Classify(429, []byte(`{"error":{"message":"weekly usage limit"}}`), nil)
	if r.ResetAt.IsZero() {
		t.Fatal("weekly 无 reset 来源应有保守默认（+7d）")
	}
	d := time.Until(r.ResetAt)
	if d < 7*24*time.Hour-time.Minute || d > 8*24*time.Hour {
		t.Fatalf("weekly 默认应≈7d：got %v", d)
	}
	r2 := Classify(429, []byte(`{"error":{"message":"monthly limit"}}`), nil)
	d2 := time.Until(r2.ResetAt)
	if d2 < 30*24*time.Hour-time.Minute {
		t.Fatalf("monthly 默认应≥30d：got %v", d2)
	}
	r3 := Classify(429, []byte(`{"error":{"type":"usage_limit_reached","message":"limit"}}`), nil)
	d3 := time.Until(r3.ResetAt)
	if d3 < 5*time.Hour-time.Minute || d3 > 6*time.Hour {
		t.Fatalf("未识别窗口默认应≈5h：got %v", d3)
	}
}
