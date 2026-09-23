package qclaw

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPrepareBodyInjectsSystem 上游要求 messages 必含 system；缺失时补入，
// 已有时不动，developer 归一为 system。
func TestPrepareBodyInjectsSystem(t *testing.T) {
	noSys := `{"model":"modelroute","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	out := PrepareBody([]byte(noSys))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应补入 system，消息数=%d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("首条应为 system，got %v", first["role"])
	}

	hasSys := `{"model":"pool-hy3-preview","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`
	out2 := PrepareBody([]byte(hasSys))
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if l := len(obj2["messages"].([]any)); l != 2 {
		t.Fatalf("已有 system 不应再补，消息数=%d", l)
	}

	dev := `{"model":"modelroute","messages":[{"role":"developer","content":"d"},{"role":"user","content":"u"}]}`
	out3 := PrepareBody([]byte(dev))
	var obj3 map[string]any
	_ = json.Unmarshal(out3, &obj3)
	m0 := obj3["messages"].([]any)[0].(map[string]any)
	if m0["role"] != "system" {
		t.Fatalf("developer 应归一 system，got %v", m0["role"])
	}
}

// TestAggregateParseRealChunks 用实测 QClaw 流式 chunk 形态验证聚合：
// delta.content / delta.reasoning_content 累积、finish 与 usage 透传。
func TestAggregateParseRealChunks(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"用户要求"}}],"created":1790134063,"id":"msg-abc","model":"modelroute","object":"chat.completion.chunk"}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"你好呀朋友"}}],"id":"msg-abc","model":"modelroute"}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	c := New()
	resp, err := c.Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	if resp["id"] != "msg-abc" || resp["model"] != "modelroute" {
		t.Fatalf("id/model 错误: %v %v", resp["id"], resp["model"])
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "你好呀朋友" {
		t.Fatalf("content 聚合错误: %v", msg["content"])
	}
	if msg["reasoning_content"] != "用户要求" {
		t.Fatalf("reasoning 聚合错误: %v", msg["reasoning_content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason 错误: %v", choice["finish_reason"])
	}
	u := resp["usage"].(map[string]any)
	if u["prompt_tokens"].(float64) != 12 {
		t.Fatalf("usage 错误: %v", u)
	}
}
