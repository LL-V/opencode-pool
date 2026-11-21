package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func hdr(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

// —— 头来源优先级（R005）——

func TestResolveHeaderPriority(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"x-opencode-session 最优先", hdr("x-opencode-session", "aaa"), "aaa"},
		{"Session-Id 次之", hdr("Session-Id", "bbb"), "bbb"},
		{"X-Claude-Code-Session-Id 第三", hdr("X-Claude-Code-Session-Id", "ccc"), "ccc"},
		{"Thread-Id 第四", hdr("Thread-Id", "ddd"), "ddd"},
		{"X-DeepSeek-Harness-Session-Id 第五", hdr("X-DeepSeek-Harness-Session-Id", "eee"), "eee"},
		{"X-Session-Id 第六", hdr("X-Session-Id", "fff"), "fff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.header, nil)
			if got != tc.want {
				t.Fatalf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveHeaderCaseInsensitiveAndPriorityOrder(t *testing.T) {
	h := http.Header{}
	h.Set("X-SESSION-ID", "lower-priority")
	h.Set("x-claude-code-session-id", "higher")
	if got := Resolve(h, nil); got != "higher" {
		t.Fatalf("大小写不敏感且按优先级取值：got %q", got)
	}
}

func TestResolveSkipsMalformedHeader(t *testing.T) {
	h := http.Header{}
	h.Set("x-opencode-session", strings.Repeat("x", 600)) // 超长 → 视为缺失
	h.Set("X-Session-Id", "valid")
	if got := Resolve(h, nil); got != "valid" {
		t.Fatalf("畸形头应跳过取下一级：got %q", got)
	}
}

func TestResolveIgnoresClientRequestId(t *testing.T) {
	h := hdr("X-Client-Request-Id", "per-request")
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	got := Resolve(h, body)
	if got == "per-request" {
		t.Fatal("X-Client-Request-Id 每请求唯一，禁止作为会话来源")
	}
	if !strings.HasPrefix(got, "d-") {
		t.Fatalf("应落入派生：got %q", got)
	}
}

// —— body 字段来源 ——
func TestResolveBodyPromptCacheKey(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"pk-1","messages":[{"role":"user","content":"x"}]}`)
	if got := Resolve(nil, body); got != "pk-1" {
		t.Fatalf("prompt_cache_key 优先于派生：got %q", got)
	}
}

func TestResolveBodyMetadataUserID(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"user-77"},"messages":[{"role":"user","content":"x"}]}`)
	if got := Resolve(nil, body); got != "user-77" {
		t.Fatalf("metadata.user_id：got %q", got)
	}
}

func TestResolveMetadataUserIDJSONWithSessionID(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"inner-s\"}"},"messages":[]}`)
	if got := Resolve(nil, body); got != "inner-s" {
		t.Fatalf("user_id 为 JSON 时取内层 session_id：got %q", got)
	}
}

// —— 派生（R006）——

func openAIBody(firstUser string) []byte {
	return []byte(fmt.Sprintf(`{"model":"gpt","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":%q}]}`, firstUser))
}

func TestDeriveFromFirstUserMessage(t *testing.T) {
	sum := sha256.Sum256([]byte("hello world"))
	want := "d-" + hex.EncodeToString(sum[:])[:16]
	if got := Resolve(nil, openAIBody("hello world")); got != want {
		t.Fatalf("派生=sha256(首条user)[:16]：got %q want %q", got, want)
	}
}

func TestDeriveStableAcrossTurnsAndSystemChange(t *testing.T) {
	base := Resolve(nil, openAIBody("hello world"))
	multiTurn := []byte(`{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hello world"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`)
	if got := Resolve(nil, multiTurn); got != base {
		t.Fatal("多轮对话首条 user 不变 → 派生值必须不变")
	}
	systemChanged := []byte(`{"messages":[{"role":"system","content":"完全不同的系统提示"},"user","hello world","assistant","hi"]}`)
	_ = systemChanged
	sc := []byte(`{"messages":[{"role":"system","content":"另一个系统提示"},{"role":"user","content":"hello world"}]}`)
	if got := Resolve(nil, sc); got != base {
		t.Fatal("加/改 system 不得改变派生值")
	}
	firstChanged := []byte(`{"messages":[{"role":"user","content":"different opener"}]}`)
	if got := Resolve(nil, firstChanged); got == base {
		t.Fatal("首条 user 变了 → 派生值必须变")
	}
}

func TestDeriveClaudeContentArray(t *testing.T) {
	// claude messages 形态：content 为数组
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"part-a"},{"type":"image","source":"..."},{"type":"text","text":"part-b"}]}]}`)
	sum := sha256.Sum256([]byte("part-a\x00part-b"))
	want := "d-" + hex.EncodeToString(sum[:])[:16]
	if got := Resolve(nil, body); got != want {
		t.Fatalf("数组 text 项以 \\x00 分隔：got %q want %q", got, want)
	}
}

func TestDeriveEmptyTextFallbackChain(t *testing.T) {
	// 首条 user 纯图片（无 text）→ 取其后首条非空 text 消息（可能是 assistant 或后续 user）
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":"x"}]},{"role":"assistant","content":[{"type":"text","text":"answer"}]}]}`)
	// deep-check P1 裁决：回退链只认后续 user 消息；assistant 回复逐轮增长，
	// 参与派生会 re-key 会话。无后续 user 文本 → 空串固定哈希（跨轮稳定）。
	sum := sha256.Sum256([]byte(""))
	want := "d-" + hex.EncodeToString(sum[:])[:16]
	if got := Resolve(nil, body); got != want {
		t.Fatalf("空文本回退链只认 user，无则空串固定值：got %q want %q", got, want)
	}
}

func TestDeriveAllEmptyFallsToFixedValue(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":"x"}]}]}`)
	sum := sha256.Sum256([]byte(""))
	want := "d-" + hex.EncodeToString(sum[:])[:16]
	if got := Resolve(nil, body); got != want {
		t.Fatalf("全空 → 固定值 sha256(\"\")：got %q want %q", got, want)
	}
}

func TestResolveNoUserMessageUsesRawBodyHash(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"only system"}]}`)
	sum := sha256.Sum256(body)
	want := "b-" + hex.EncodeToString(sum[:])[:16]
	if got := Resolve(nil, body); got != want {
		t.Fatalf("无 user 消息 → b-原始body哈希：got %q want %q", got, want)
	}
}

func TestResolveEmptyBodyProbe(t *testing.T) {
	// 无头无 body（探测类）：UUID 形态（有内容且非 d-/b- 前缀）
	got := Resolve(nil, nil)
	if got == "" || strings.HasPrefix(got, "d-") || strings.HasPrefix(got, "b-") {
		t.Fatalf("探测请求应为 UUID：got %q", got)
	}
	if Resolve(nil, nil) == got {
		t.Fatal("探测 UUID 每次应不同")
	}
}

// Responses API（input[]）也必须能派生稳定会话（R006 覆盖 /v1/responses）。
func TestDeriveResponsesInput(t *testing.T) {
	a := []byte(`{"model":"x","input":[{"role":"user","content":"hello responses"}]}`)
	b := []byte(`{"model":"x","instructions":"changed","input":[{"role":"user","content":"hello responses"}]}`)
	sa, sb := Resolve(nil, a), Resolve(nil, b)
	if !strings.HasPrefix(sa, "d-") || sa != sb {
		t.Fatalf("input[] 派生应稳定：%q vs %q", sa, sb)
	}
	str := Resolve(nil, []byte(`{"input":"hello string"}`))
	if !strings.HasPrefix(str, "d-") {
		t.Fatalf("字符串 input 应派生：%q", str)
	}
}

// 配置的 header 源列表应覆盖默认优先级。
func TestResolveWithSourcesOverride(t *testing.T) {
	h := http.Header{}
	h.Set("X-Opencode-Session", "default-wins")
	h.Set("X-Custom-Session", "custom-wins")
	if got := ResolveWithSources(h, nil, []string{"x-custom-session"}); got != "custom-wins" {
		t.Fatalf("自定义源优先：%q", got)
	}
	if got := Resolve(h, nil); got != "default-wins" {
		t.Fatalf("默认源：%q", got)
	}
}

// Responses input[] 的 input_text 必须可派生；后续 user 文本回退不得
// 使用空 root.Messages 切片（曾越界 panic [1:0]）。
func TestDeriveResponsesInputBlocksNoPanic(t *testing.T) {
	body := []byte(`{"input":[
	  {"role":"user","content":[{"type":"input_text","text":"first question"}]},
	  {"role":"assistant","content":[{"type":"output_text","text":"answer"}]}
	]}`)
	got := Resolve(nil, body)
	if !strings.HasPrefix(got, "d-") {
		t.Fatalf("input_text 应派生 d-：%q", got)
	}
	// 第一条 user 无文本无锚点，触发“后续 user”回退路径
	odd := []byte(`{"input":[
	  {"role":"user","content":[{"type":"input_text"}]},
	  {"role":"assistant","content":[{"type":"output_text","text":"a"}]},
	  {"role":"user","content":[{"type":"input_text","text":"later"}]}
	]}`)
	if got := Resolve(nil, odd); !strings.HasPrefix(got, "d-") {
		t.Fatalf("回退派生：%q", got)
	}
}
