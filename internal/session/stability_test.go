package session

import (
	"net/http"
	"testing"
)

// 图片首条消息的多轮会话：派生值必须跨轮稳定（deep-check P1——
// "any role" 回退链会随 assistant 回复逐轮 re-key 会话，摧毁 prompt cache）。
func TestImageFirstConversationStableAcrossTurns(t *testing.T) {
	turn1 := `{"messages":[
		{"role":"user","content":[{"type":"image","source":{"data":"i1"}},{"type":"text","text":""}]}
	]}`
	turn2 := `{"messages":[
		{"role":"user","content":[{"type":"image","source":{"data":"i1"}},{"type":"text","text":""}]},
		{"role":"assistant","content":[{"type":"text","text":"这是第一轮回复"}]},
		{"role":"user","content":[{"type":"text","text":"继续"}]}
	]}`
	turn3 := `{"messages":[
		{"role":"user","content":[{"type":"image","source":{"data":"i1"}},{"type":"text","text":""}]},
		{"role":"assistant","content":[{"type":"text","text":"这是第一轮回复"}]},
		{"role":"user","content":[{"type":"text","text":"继续"}]},
		{"role":"assistant","content":[{"type":"text","text":"第二轮回复更长一些，内容不同"}]},
		{"role":"user","content":[{"type":"text","text":"再继续"}]}
	]}`
	s1 := Resolve(http.Header{}, []byte(turn1))
	s2 := Resolve(http.Header{}, []byte(turn2))
	s3 := Resolve(http.Header{}, []byte(turn3))
	if s1 == "" || s1 != s2 || s2 != s3 {
		t.Fatalf("图片会话派生必须跨轮稳定：%q %q %q", s1, s2, s3)
	}
}

// 后续 user 文本作为派生源时同样跨轮稳定。
func TestLaterUserTextStableAcrossTurns(t *testing.T) {
	turn1 := `{"messages":[
		{"role":"user","content":[{"type":"image","source":{"data":"i1"}}]},
		{"role":"user","content":[{"type":"text","text":"看这张图"}]}
	]}`
	turn2 := `{"messages":[
		{"role":"user","content":[{"type":"image","source":{"data":"i1"}}]},
		{"role":"user","content":[{"type":"text","text":"看这张图"}]},
		{"role":"assistant","content":[{"type":"text","text":"好的"}]},
		{"role":"user","content":[{"type":"text","text":"谢谢"}]}
	]}`
	s1 := Resolve(http.Header{}, []byte(turn1))
	s2 := Resolve(http.Header{}, []byte(turn2))
	if s1 == "" || s1 != s2 {
		t.Fatalf("后续 user 文本派生必须跨轮稳定：%q %q", s1, s2)
	}
}
