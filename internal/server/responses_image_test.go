package server

import (
	"encoding/json"
	"testing"
)

// translateMsgs 跑一遍 Responses→chat 翻译并取出 messages 数组。
func translateMsgs(t *testing.T, src string) []any {
	t.Helper()
	chat, _, err := translateResponsesToChat([]byte(src))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages 缺失: %s", chat)
	}
	return msgs
}

// /v1/responses 识图：user 消息里的 input_image 必须翻译为 chat 的 image_url
// part。此前该部件被静默丢弃（只取部件的 text 字段），出站 body 里一张图都没有，
// 模型收到纯文本 → 无法识图。
func TestResponsesImageInput(t *testing.T) {
	msgs := translateMsgs(t, `{
		"model": "glm-5v-turbo",
		"input": [
			{"type": "message", "role": "user", "content": [
				{"type": "input_text", "text": "这张图里是什么？"},
				{"type": "input_image", "image_url": "data:image/png;base64,QUJD"}
			]}
		]
	}`)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d want 1", len(msgs))
	}
	raw := msgs[0].(map[string]any)["content"]
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("带图 content 应为多模态数组，实为 %T: %v", raw, raw)
	}
	if len(arr) != 2 {
		t.Fatalf("parts=%d want 2: %v", len(arr), arr)
	}
	if p := arr[0].(map[string]any); p["type"] != "text" || p["text"] != "这张图里是什么？" {
		t.Errorf("text part=%v", p)
	}
	p := arr[1].(map[string]any)
	if p["type"] != "image_url" {
		t.Fatalf("image part type=%v", p["type"])
	}
	if u := p["image_url"].(map[string]any)["url"]; u != "data:image/png;base64,QUJD" {
		t.Errorf("image url=%v", u)
	}
}

// input_image 的 image_url 兼容 {url,detail} 对象形态（部分客户端这么发），
// detail 透传给上游。
func TestResponsesImageObjectForm(t *testing.T) {
	msgs := translateMsgs(t, `{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_image","image_url":{"url":"https://x/y.png","detail":"high"}}]}]}`)
	arr, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content 应为数组: %v", msgs[0])
	}
	if len(arr) != 1 {
		t.Fatalf("纯图消息 parts=%d want 1: %v", len(arr), arr)
	}
	iu := arr[0].(map[string]any)["image_url"].(map[string]any)
	if iu["url"] != "https://x/y.png" || iu["detail"] != "high" {
		t.Errorf("image_url=%v", iu)
	}
}

// 纯文本路径的出站形状不得退化：content 仍是字符串（拼接规则与改写前一致）；
// 取不到 url 的 image 部件（如 file_id 引用形态）静默丢弃，不产生空数组。
func TestResponsesTextOnlyContentUnchanged(t *testing.T) {
	msgs := translateMsgs(t, `{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"hello"},{"type":"input_text","text":"world"},
		{"type":"input_image","file_id":"file_1"}]}]}`)
	c := msgs[0].(map[string]any)["content"]
	s, ok := c.(string)
	if !ok {
		t.Fatalf("无可用图片时 content 应为字符串，实为 %T: %v", c, c)
	}
	if s != "hello\nworld" {
		t.Errorf("content=%q want %q", s, "hello\nworld")
	}
}

// 多图保序：text part 恒在最前，image part 按原始部件顺序排列
//（顺序影响模型对"第一张/第二张"的指代）。
func TestResponsesMultipleImagesOrder(t *testing.T) {
	msgs := translateMsgs(t, `{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_image","image_url":"data:image/png;base64,AA"},
		{"type":"input_text","text":"compare"},
		{"type":"input_image","image_url":"data:image/png;base64,BB"}]}]}`)
	arr := msgs[0].(map[string]any)["content"].([]any)
	if len(arr) != 3 {
		t.Fatalf("parts=%d want 3: %v", len(arr), arr)
	}
	if p := arr[0].(map[string]any); p["type"] != "text" || p["text"] != "compare" {
		t.Errorf("text 应在最前: %v", p)
	}
	u1 := arr[1].(map[string]any)["image_url"].(map[string]any)["url"]
	u2 := arr[2].(map[string]any)["image_url"].(map[string]any)["url"]
	if u1 != "data:image/png;base64,AA" || u2 != "data:image/png;base64,BB" {
		t.Errorf("图序漂移: %v %v", u1, u2)
	}
}

// 工具结果里的图片：chat 的 role:tool 只承载文本，图片挪到紧随其后的合成 user
// 消息（上游在非 user 轮丢弃图片，见 upstream/images.go 的 relocateAssistantImages）。
func TestResponsesFunctionCallOutputImages(t *testing.T) {
	msgs := translateMsgs(t, `{
		"model": "m",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "screenshot", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": [
				{"type": "input_text", "text": "shot taken"},
				{"type": "input_image", "image_url": "data:image/png;base64,QUJD"}
			]}
		]
	}`)
	if len(msgs) != 3 {
		t.Fatalf("msgs=%d want 3 (assistant,tool,user-images): %v", len(msgs), msgs)
	}
	if r := msgs[0].(map[string]any)["role"]; r != "assistant" {
		t.Errorf("msgs[0] role=%v", r)
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "shot taken" {
		t.Errorf("tool msg=%v", tool)
	}
	um := msgs[2].(map[string]any)
	if um["role"] != "user" {
		t.Fatalf("图片未挪到合成 user 消息: %v", um)
	}
	arr, ok := um["content"].([]any)
	if !ok || len(arr) != 1 || arr[0].(map[string]any)["type"] != "image_url" {
		t.Errorf("合成 user content=%v", um["content"])
	}
}

// 纯文本工具结果不额外合成消息（无图时消息序列与改写前逐字一致）。
func TestResponsesTextOnlyToolOutputNoExtraMessage(t *testing.T) {
	msgs := translateMsgs(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"18C cloudy"}]}`)
	if len(msgs) != 2 {
		t.Fatalf("msgs=%d want 2 (assistant,tool): %v", len(msgs), msgs)
	}
	if c := msgs[1].(map[string]any)["content"]; c != "18C cloudy" {
		t.Errorf("tool content=%v", c)
	}
}
