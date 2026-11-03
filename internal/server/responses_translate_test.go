package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回归：response.created/in_progress 的 response.output 必须是数组。
// nil 切片序列化成 null，monoize 等严格中转直接 400
// responses_invalid_event: response.created.response.output must be an array.
func TestResponsesStreamCreatedOutputIsArray(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "",
		"data: [DONE]", "",
	}, "\n")
	rec := httptest.NewRecorder()
	_, _ = (&Handler{}).responsesRelay(rec, io.NopCloser(strings.NewReader(upstream)), "gpt-5", true, time.Now())
	body := rec.Body.String()
	for _, evt := range []string{"event: response.created", "event: response.in_progress"} {
		i := strings.Index(body, evt)
		if i < 0 {
			t.Fatalf("missing %s:\n%s", evt, body)
		}
		frame := body[i:]
		if end := strings.Index(frame, "\n\n"); end >= 0 {
			frame = frame[:end]
		}
		if strings.Contains(frame, `"output":null`) || !strings.Contains(frame, `"output":[]`) {
			t.Errorf("%s must carry output array:\n%s", evt, frame)
		}
	}
	if !strings.Contains(body, `"output":[{`) || !strings.Contains(body, `"type":"message"`) {
		t.Errorf("completed output missing:\n%s", body)
	}
}

// 回归：user 消息的 input_image 必须翻译成 chat image_url，不得被丢弃。
func TestTranslateResponsesPreservesUserImage(t *testing.T) {
	raw := []byte(`{
		"model":"m",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"看图"},
			{"type":"input_image","image_url":"data:image/png;base64,QUJD","detail":"high"}
		]}]
	}`)
	out, model, err := translateResponsesToChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if model != "m" {
		t.Fatalf("model=%q", model)
	}
	var chat struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" {
		t.Fatalf("messages=%s", out)
	}
	var parts []map[string]any
	if err := json.Unmarshal(chat.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content not multimodal: %s", chat.Messages[0].Content)
	}
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("parts=%v", parts)
	}
	iu, _ := parts[1]["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,QUJD" || iu["detail"] != "high" {
		t.Fatalf("image_url=%v", iu)
	}
}

// 回归：function_call_output 里的图片在 chat 只能进合成 user 消息——
// tool 消息保持纯文本，且必须排在该批工具结果之后（tool_call 配对不断裂）。
func TestTranslateResponsesMovesToolResultImage(t *testing.T) {
	raw := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"截图看看"},
			{"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_text","text":"已截图"},
				{"type":"input_image","image_url":"data:image/png;base64,QUJD"}
			]}
		]
	}`)
	out, _, err := translateResponsesToChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role      string          `json:"role"`
			ToolCalls []any           `json:"tool_calls"`
			CallID    string          `json:"tool_call_id"`
			Content   json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range chat.Messages {
		roles = append(roles, m.Role)
	}
	if got, want := strings.Join(roles, ","), "user,assistant,tool,user"; got != want {
		t.Fatalf("roles=%s want %s\n%s", got, want, out)
	}
	var toolText string
	if err := json.Unmarshal(chat.Messages[2].Content, &toolText); err != nil || toolText != "已截图" {
		t.Fatalf("tool content=%s (%v)", chat.Messages[2].Content, err)
	}
	if chat.Messages[2].CallID != "c1" {
		t.Fatalf("tool_call_id=%q", chat.Messages[2].CallID)
	}
	var imgParts []map[string]any
	if err := json.Unmarshal(chat.Messages[3].Content, &imgParts); err != nil {
		t.Fatalf("synthetic user content=%s", chat.Messages[3].Content)
	}
	if len(imgParts) != 1 || imgParts[0]["type"] != "image_url" {
		t.Fatalf("imgParts=%v", imgParts)
	}
}

// 回归：并行工具调用中，第一条输出带图时不得把 tool 序列打断
// （assistant → tool(c1) → user(image) → tool(c2) 会被上游按工具序错误拒绝）。
// 图片必须攒到整批工具结果之后：assistant → tool,tool → user(images)。
func TestTranslateResponsesKeepsToolSequenceContiguous(t *testing.T) {
	raw := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"两张图"},
			{"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},
			{"type":"function_call","call_id":"c2","name":"shot","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_text","text":"第一张"},
				{"type":"input_image","image_url":"data:image/png;base64,QUJD"}
			]},
			{"type":"function_call_output","call_id":"c2","output":"第二张"}
		]
	}`)
	out, _, err := translateResponsesToChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role      string          `json:"role"`
			ToolCalls []any           `json:"tool_calls"`
			CallID    string          `json:"tool_call_id"`
			Content   json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range chat.Messages {
		roles = append(roles, m.Role)
	}
	if got, want := strings.Join(roles, ","), "user,assistant,tool,tool,user"; got != want {
		t.Fatalf("roles=%s want %s\n%s", got, want, out)
	}
	if len(chat.Messages[1].ToolCalls) != 2 {
		t.Fatalf("tool_calls=%d want 2", len(chat.Messages[1].ToolCalls))
	}
	if chat.Messages[2].CallID != "c1" || chat.Messages[3].CallID != "c2" {
		t.Fatalf("tool order c1=%q c2=%q", chat.Messages[2].CallID, chat.Messages[3].CallID)
	}
	var imgParts []map[string]any
	if err := json.Unmarshal(chat.Messages[4].Content, &imgParts); err != nil {
		t.Fatalf("synthetic user content=%s", chat.Messages[4].Content)
	}
	if len(imgParts) != 1 || imgParts[0]["type"] != "image_url" {
		t.Fatalf("imgParts=%v", imgParts)
	}
}
