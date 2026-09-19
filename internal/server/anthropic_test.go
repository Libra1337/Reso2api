package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 翻译：system/多轮/tool_use/tool_result → chat 消息序列与 tools 定义。
func TestTranslateAnthropicToChat(t *testing.T) {
	src := `{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": "Weather?"},
			{"role": "assistant", "content": [
				{"type":"text", "text": "Let me check."},
				{"type":"thinking", "thinking": "internal"},
				{"type":"tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "Paris"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": "18C cloudy"},
				{"type":"text", "text": "Thanks, summarize."}
			]}
		],
		"tools": [{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"tool_choice": {"type": "auto"},
		"stream": true
	}`
	chat, model, err := translateAnthropicToChat([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-4" {
		t.Errorf("model=%s", model)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("msgs=%d want 5 (system,user,assistant,tool,user-text)", len(msgs))
	}
	if msgs[3].(map[string]any)["role"] != "tool" || msgs[4].(map[string]any)["role"] != "user" {
		t.Errorf("tool_result split wrong: %v %v", msgs[3], msgs[4])
	}
	// system
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("msgs[0] role=%v", msgs[0].(map[string]any)["role"])
	}
	// assistant 带 tool_calls 且 arguments 为 JSON 字符串
	am := msgs[2].(map[string]any)
	tcs := am["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Errorf("tool_call=%v", fn)
	}
	// tools 翻译
	tools := m["tools"].([]any)
	ofn := tools[0].(map[string]any)["function"].(map[string]any)
	if ofn["name"] != "get_weather" {
		t.Errorf("tools=%v", ofn)
	}
	if _, has := ofn["parameters"]; !has {
		t.Errorf("input_schema -> parameters missing")
	}
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
	if m["stream"] != true {
		t.Errorf("stream must be forced true")
	}
}

// 流式中继：thinking → text → tool_use 完整事件序列与 stop_reason。
func TestAnthropicRelayStream(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking hard"}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"Answer: "}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"sunny"}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""},"index":0}]}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"city\":"},"index":0}]}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"Paris\"}"},"index":0}]}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`, "",
		"data: [DONE]", "",
	}, "\n")
	rec := httptest.NewRecorder()
	usage := anthropicRelay(rec, io.NopCloser(strings.NewReader(upstream)), "glm-5.3")
	body := rec.Body.String()

	for _, want := range []string{
		"event: message_start",
		"event: content_block_start", `"type":"thinking"`,
		"event: content_block_delta", `"thinking_delta"`, `"thinking hard"`,
		`"type":"text"`, `"text_delta"`, `"sunny"`,
		`"type":"tool_use"`, `"call_a"`, `"get_weather"`, `"input_json_delta"`, `"partial_json":"\"Paris\"}"`,
		"event: content_block_stop",
		"event: message_delta", `"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "event: message_stop"); n != 1 {
		t.Errorf("message_stop count=%d", n)
	}
	if usage == nil || num(usage["completion_tokens"]) != 20 {
		t.Errorf("usage=%v", usage)
	}
	// 块序号顺序递增：thinking=0, text=1, tool=2
	if !strings.Contains(body, `"content_block":{"signature":"","thinking":"","type":"thinking"},"index":0`) {
		t.Errorf("thinking block index wrong:\n%s", body)
	}
}

// 回归：上游流中途截断（无 finish_reason/[DONE]）必须发 error 事件，
// 不得伪装成完整回复（表现为客户端"思考链断链"）。
func TestAnthropicRelayTruncated(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"partial thinking"}}]}`, "",
		// 没有 finish_reason、没有 [DONE]，流就此终止
	}, "\n")
	rec := httptest.NewRecorder()
	anthropicRelay(rec, io.NopCloser(strings.NewReader(upstream)), "glm-5.3")
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("truncated stream must emit error event:\n%s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("message_stop still required for protocol closure:\n%s", body)
	}
	// 正常流不应出现 error 事件
	rec2 := httptest.NewRecorder()
	anthropicRelay(rec2, io.NopCloser(strings.NewReader(
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), "glm-5.3")
	if strings.Contains(rec2.Body.String(), "event: error") {
		t.Errorf("normal stream must not emit error event:\n%s", rec2.Body.String())
	}
}

// 回归：思考与工具调用交错导致工具块重开时，id/name 保留原值（不合成 call_N）。
func TestAnthropicRelayToolBlockReopenKeepsIdentity(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_real","type":"function","function":{"name":"get_weather","arguments":"{\"ci"},"index":0}]}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"wait, check again"}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"ty\":\"Paris\"}"},"index":0}]}}]}`, "",
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "",
		"data: [DONE]", "",
	}, "\n")
	rec := httptest.NewRecorder()
	anthropicRelay(rec, io.NopCloser(strings.NewReader(upstream)), "glm-5.3")
	body := rec.Body.String()
	// 第二次 tool 块 start 仍带原 id 与 name
	if !strings.Contains(body, `"id":"call_real","input":{},"name":"get_weather","type":"tool_use"`) {
		t.Errorf("reopened tool block lost identity:\n%s", body)
	}
	// 拼接后的 partial_json 还原完整参数
	if !strings.Contains(body, `"partial_json":"ty\":\"Paris\"}"`) {
		t.Errorf("args fragment missing:\n%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason wrong:\n%s", body)
	}
}

// 非流式：Aggregate → Anthropic message（thinking + text + tool_use + stop_reason）。
func TestAnthropicFromAggregate(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "It is sunny.",
				"reasoning_content": "thought process",
				"tool_calls": []any{map[string]any{
					"id": "call_x", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Paris"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	}
	out := anthropicFromAggregate(resp, "kimi-k3-1")
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("out=%v", out)
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", out["stop_reason"])
	}
	content := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content=%v", content)
	}
	if content[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("block0=%v", content[0])
	}
	if content[1].(map[string]any)["type"] != "text" {
		t.Errorf("block1=%v", content[1])
	}
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_x" || tu["name"] != "get_weather" {
		t.Errorf("block2=%v", tu)
	}
	if in, _ := tu["input"].(map[string]any); in["city"] != "Paris" {
		t.Errorf("input=%v", tu["input"])
	}
	u := out["usage"].(map[string]any)
	if u["input_tokens"] != 5 || u["output_tokens"] != 7 {
		t.Errorf("usage=%v", u)
	}
}

// 图片块翻译：Anthropic base64/url image → OpenAI image_url part（不再丢弃为 [image omitted]）。
func TestTranslateAnthropicImages(t *testing.T) {
	src := `{
		"model": "claude-sonnet-4",
		"max_tokens": 100,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "What is in this image?"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "QUJD"}},
			{"type": "image", "source": {"type": "url", "url": "https://example.com/x.png"}}
		]}]
	}`
	chat, _, err := translateAnthropicToChat([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	s := string(chat)
	if strings.Contains(s, "[image omitted]") {
		t.Fatalf("图片被丢弃: %.200s", s)
	}
	if !strings.Contains(s, `"data:image/jpeg;base64,QUJD"`) {
		t.Fatalf("base64 图片未转 data URL: %.300s", s)
	}
	if !strings.Contains(s, `"https://example.com/x.png"`) {
		t.Fatalf("url 图片未透传: %.300s", s)
	}
	if !strings.Contains(s, `"type":"image_url"`) {
		t.Fatalf("无 image_url part: %.300s", s)
	}
	// 纯文本消息仍是字符串 content，不被多模态化
	chat2, _, err := translateAnthropicToChat([]byte(`{"model":"claude-sonnet-4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(chat2, &p)
	if len(p.Messages) != 1 || string(p.Messages[0].Content) != `"hi"` {
		t.Fatalf("纯文本 content 形态改变: %.200s", chat2)
	}
}
