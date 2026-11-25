package session

import (
	"net/http"
	"testing"
)

// 首条纯图片消息必须按图片锚点派生：不同图片 → 不同会话键；同一图片
// 跨轮 → 同一键。旧实现只认 url/source，真实 image_url/input_image 全部
// 落到 d-""（不同图片会话共用亲和键）。
func TestDeriveImageAnchors(t *testing.T) {
	chat := func(url string) string {
		body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + url + `"}}]}]}`)
		return Resolve(http.Header{}, body)
	}
	responses := func(url string) string {
		body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + url + `"}]}]}`)
		return Resolve(http.Header{}, body)
	}

	img1, img2 := "data:image/png;base64,AAA", "data:image/png;base64,BBB"
	if chat(img1) == chat(img2) {
		t.Fatal("不同图片必须派生出不同会话键")
	}
	if chat(img1) != chat(img1) {
		t.Fatal("同一图片必须派生稳定会话键")
	}
	if responses(img1) == "" || responses(img1) == responses(img2) {
		t.Fatal("Responses input_image 必须参与派生且区分图片")
	}
}

// Responses 的 input 允许是字符串数组；旧实现解析失败后退化为整包哈希
// （逐轮变化）且被绑定逻辑跳过。
func TestDeriveStringArrayInput(t *testing.T) {
	body1 := []byte(`{"input":["hello world"]}`)
	body2 := []byte(`{"input":["hello world","second turn"]}`)
	if got := Resolve(http.Header{}, body1); got[:2] != "d-" {
		t.Fatalf("字符串数组 input 应派生 d- 键：%q", got)
	}
	if Resolve(http.Header{}, body1) != Resolve(http.Header{}, body2) {
		t.Fatal("首条输入一致时派生键必须稳定")
	}
}
