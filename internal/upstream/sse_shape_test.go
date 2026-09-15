package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseFrames 解析 Stream 输出为帧列表（[DONE] 记为 nil）。
func parseFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, ln := range strings.Split(body, "\n") {
		if !strings.HasPrefix(ln, "data: ") {
			continue
		}
		p := ln[6:]
		if p == "[DONE]" {
			frames = append(frames, nil)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(p), &m); err != nil {
			t.Fatalf("bad frame %q: %v", p, err)
		}
		frames = append(frames, m)
	}
	return frames
}

// glm 真实形态：噪音帧全字段空值、role 只在末帧出现、usage 附在 finish 帧。
const glmToolFixture = "data: {\"id\":\"cmb-1\",\"object\":\"chat.completion.chunk\",\"created\":1789075546,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\",\"tool_calls\":[]}}]}\n\n" +
	"data: {\"id\":\"cmb-1\",\"object\":\"chat.completion.chunk\",\"created\":1789075546,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\",\"tool_calls\":[{\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"},\"index\":0}]}}]}\n\n" +
	"data: {\"id\":\"cmb-1\",\"object\":\"chat.completion.chunk\",\"created\":1789075546,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\",\"tool_calls\":[{\"function\":{\"name\":\"\",\"arguments\":\"{\\\"city\\\":\"},\"index\":0}]}}]}\n\n" +
	"data: {\"id\":\"cmb-1\",\"object\":\"chat.completion.chunk\",\"created\":1789075546,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\",\"tool_calls\":[{\"function\":{\"name\":\"\",\"arguments\":\"\\\"Paris\\\"}\"},\"index\":0}]}}]}\n\n" +
	"data: {\"id\":\"cmb-1\",\"object\":\"chat.completion.chunk\",\"created\":1789075546,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"total_tokens\":11}}\n\n" +
	"data: [DONE]\n\n"

// 回归：glm role 在末帧出现时，输出首个 delta 帧必须强制携带 role:"assistant"，
// 否则严格的 OpenAI→Anthropic 转换器无法开启 assistant 消息（工具调用解析失败）。
func TestStreamRoleOnFirstDeltaFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(glmToolFixture)); err != nil {
		t.Fatal(err)
	}
	frames := parseFrames(t, rec.Body.String())
	if len(frames) < 4 {
		t.Fatalf("frames=%d", len(frames))
	}
	var first *map[string]any
	for i := range frames {
		if frames[i] == nil {
			continue
		}
		if ch, ok := frames[i]["choices"].([]any); ok && len(ch) > 0 {
			if _, hasDelta := ch[0].(map[string]any)["delta"]; hasDelta {
				f := &frames[i]
				first = f
				break
			}
		}
	}
	if first == nil {
		t.Fatal("no delta frame")
	}
	d := (*first)["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if d["role"] != "assistant" {
		t.Errorf("first delta frame role=%v, want assistant", d["role"])
	}
	// 首个 tool_calls 帧携带完整 id/type/name
	var toolFrame map[string]any
	for _, f := range frames {
		if f == nil {
			continue
		}
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 {
			if d2, ok := ch[0].(map[string]any)["delta"].(map[string]any); ok {
				if _, has := d2["tool_calls"]; has {
					toolFrame = d2
					break
				}
			}
		}
	}
	if toolFrame == nil {
		t.Fatal("no tool_calls frame")
	}
	tcs, ok := toolFrame["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		t.Fatalf("tool_calls missing: %v", toolFrame)
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_a" || tc["type"] != "function" {
		t.Errorf("tool call meta=%v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn.name=%v", fn["name"])
	}
}

// 回归：每个数据帧都携带 id/object/created/model（OpenAI 规范）。
func TestStreamEveryFrameHasHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(glmToolFixture)); err != nil {
		t.Fatal(err)
	}
	for i, f := range parseFrames(t, rec.Body.String()) {
		if f == nil {
			continue
		}
		for _, k := range []string{"id", "object", "created", "model"} {
			if _, ok := f[k]; !ok {
				t.Errorf("frame[%d] missing %s: %v", i, k, f)
			}
		}
	}
}

// 回归：usage 单独成帧时补 "choices":[]（防严格客户端 choices[0] 越界）。
func TestStreamUsageOnlyFrameGetsChoices(t *testing.T) {
	raw := "data: {\"id\":\"u1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"u1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"u1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"usage\":{\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	frames := parseFrames(t, rec.Body.String())
	var usageFrame map[string]any
	for _, f := range frames {
		if f == nil {
			continue
		}
		if _, has := f["usage"]; has {
			if _, hasCh := f["choices"]; !hasCh {
				t.Fatalf("usage frame missing choices: %v", f)
			}
			ch, _ := f["choices"].([]any)
			if len(ch) != 0 {
				t.Fatalf("usage frame choices should be empty: %v", f)
			}
			usageFrame = f
		}
	}
	if usageFrame == nil {
		t.Fatal("no usage frame found")
	}
}

// 回归：上游 error 帧必须透传，不得被白名单重建吞掉。
func TestStreamErrorFramePassthrough(t *testing.T) {
	raw := "data: {\"id\":\"e1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"boom\"}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) || !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("error frame dropped: %q", rec.Body.String())
	}
}

// 回归（2026-09-15）：推理内容单字段输出 reasoning_content。
// 双字段（reasoning_content + reasoning 同帧同值）会被累加型客户端拼成
// 逐段翻倍乱码（"ThereThere's's"），故上游只发 reasoning 派也折叠为
// reasoning_content 单字段。
func TestStreamReasoningDualField(t *testing.T) {
	raw := "data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking hard\"}}]}\n\n" +
		"data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"thinking hard"`) {
		t.Errorf("reasoning_content missing: %q", body)
	}
	if strings.Contains(body, `"reasoning":`) {
		t.Errorf("bare reasoning field must not be emitted: %q", body)
	}
	// 上游只发 reasoning（OpenRouter 派）时折叠为 reasoning_content 单字段
	raw2 := "data: {\"id\":\"r2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"alt style\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec2 := httptest.NewRecorder()
	if err := Stream(rec2, strings.NewReader(raw2)); err != nil {
		t.Fatal(err)
	}
	b2 := rec2.Body.String()
	if !strings.Contains(b2, `"reasoning_content":"alt style"`) {
		t.Errorf("alt-style reasoning not folded into reasoning_content: %q", b2)
	}
	if strings.Contains(b2, `"reasoning":`) {
		t.Errorf("alt-style must not emit bare reasoning: %q", b2)
	}
}

// 回归：`data:[DONE]`（无空格）不再产生双 [DONE]。
func TestStreamDoneWithoutSpace(t *testing.T) {
	raw := "data: {\"id\":\"d1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data:[DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(rec.Body.String(), "[DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d, want 1: %q", n, rec.Body.String())
	}
}

// 回归：截断流（无 [DONE]、无 finish_reason）必须报错而非伪装成完整响应——
// 与流式路径截断判定同口径；调用方（chat/anthropic 非流式）据此回 502。
func TestAggregateTruncatedToolCall(t *testing.T) {
	raw := "data: {\"id\":\"t1\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"},\"index\":0}]}}]}\n\n"
	_, err := Aggregate(strings.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err=%v, want truncated error", err)
	}
	// 完整形态（finish_reason + [DONE]）正常聚合，finish=tool_calls
	full := raw +
		"data: {\"id\":\"t1\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v, want tool_calls", choice["finish_reason"])
	}
}

// 回归：choices[].message 形态的 tool_calls 也合并（非流式回包兜底）。
func TestAggregateMessageToolCalls(t *testing.T) {
	raw := "data: {\"id\":\"m1\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"X\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"X"}` {
		t.Errorf("fn=%v", fn)
	}
}
