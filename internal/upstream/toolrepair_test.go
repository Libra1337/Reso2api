package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
)

func repairBody(t *testing.T, msgs string) []byte {
	t.Helper()
	return []byte(`{"model":"m","stream":true,"messages":[` + msgs + `]}`)
}

// 11148 修复：半截 assistant（tool_call 无结果）剥掉后重发应通过。
func TestRepairToolSequenceDropsUnansweredCalls(t *testing.T) {
	in := repairBody(t,
		`{"role":"user","content":"hi"}`+","+
			`{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]}`+","+
			`{"role":"user","content":"continue"}`)
	out, changed := repairToolSequence(in)
	if !changed {
		t.Fatal("should change")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2 (broken assistant dropped)", len(msgs))
	}
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
			t.Fatal("empty assistant must be dropped entirely")
		}
	}
}

// 部分配对：一批 tool_calls 里只要有一个没结果就整批删（部分保留上游仍 400）。
func TestRepairToolSequenceDropsWholeBatchIfAnyUnanswered(t *testing.T) {
	in := repairBody(t,
		`{"role":"assistant","content":"partial","tool_calls":[{"id":"a","type":"function","function":{"name":"f1","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f2","arguments":"{}"}}]}`+","+
			`{"role":"tool","tool_call_id":"a","content":"r1"}`+","+
			`{"role":"user","content":"go"}`)
	out, changed := repairToolSequence(in)
	if !changed {
		t.Fatal("should change")
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	var assistantKept bool
	var toolKept int
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if mm["role"] == "assistant" {
				if _, has := mm["tool_calls"]; !has {
					assistantKept = true // content 仍在，保留消息但删 tool_calls
				}
			}
			if mm["role"] == "tool" {
				toolKept++ // call_a 的结果：调用已删 → 孤儿结果也删
			}
		}
	}
	if !assistantKept {
		t.Fatal("assistant with content must be kept (tool_calls removed)")
	}
	if toolKept != 0 {
		t.Fatalf("orphan tool results kept=%d want 0", toolKept)
	}
}

// 完整配对：零改动。
func TestRepairToolSequenceKeepsPaired(t *testing.T) {
	in := repairBody(t,
		`{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}`+","+
			`{"role":"tool","tool_call_id":"a","content":"ok"}`+","+
			`{"role":"user","content":"go"}`)
	out, changed := repairToolSequence(in)
	if changed {
		t.Fatalf("paired sequence must pass through unchanged: %s", out)
	}
}

// 无工具流量：零改动零分配。
func TestRepairToolSequenceNoToolTraffic(t *testing.T) {
	in := repairBody(t, `{"role":"user","content":"hi"}`)
	if _, changed := repairToolSequence(in); changed {
		t.Fatal("plain chat must not be touched")
	}
}

func TestIsBrokenToolSequence(t *testing.T) {
	if !isBrokenToolSequence([]byte(`{"code":11148,"msg":"tool calls and tool results do not match"}`)) {
		t.Fatal("11148 must match")
	}
	if !isBrokenToolSequence([]byte(`{"extError":{"code":"tool_call_sequence_broken"}}`)) {
		t.Fatal("sequence marker must match")
	}
	if isBrokenToolSequence([]byte(`{"code":11101,"msg":"bad"}`)) {
		t.Fatal("other errors must not match")
	}
}

// 乱码回归（2026-09-15 用户实测）：每个 delta 同帧带 reasoning_content + reasoning
// 双字段时，累加型客户端把两路都拼进思考流 → "ThereThere's's" 逐段翻倍。
// 修复：流式与非流式都只发 reasoning_content 单字段。
func TestStreamEmitsSingleReasoningField(t *testing.T) {
	upstream := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"There\",\"reasoning\":\"There\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"'s\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if strings.Count(body, `"reasoning":`) != 0 {
		t.Fatalf("stream must not emit bare reasoning field: %s", body)
	}
	if strings.Count(body, `"reasoning_content":"There"`) != 1 {
		t.Fatalf("reasoning_content must be emitted once: %s", body)
	}
	if strings.Count(body, `"reasoning_content":"'s"`) != 1 {
		t.Fatalf("reasoning alias must fold into reasoning_content: %s", body)
	}
}

func TestAggregateEmitsSingleReasoningField(t *testing.T) {
	upstream := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ans\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think1\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"think2\"}}]}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "think1think2" {
		t.Fatalf("reasoning_content=%v", msg["reasoning_content"])
	}
	if _, has := msg["reasoning"]; has {
		t.Fatal("aggregate must not emit bare reasoning field")
	}
}

// Test11148StickySalt 分叉会话的盐必须粘住：首轮撞 11148 换盐重试成功后，
// 后续轮次应直接带同一盐出站（不再撞 11148、不再换随机盐冷启动）。
// 生产形态：压缩重写历史中部、首条 system+user 保留 → 缓存键稳定，但上游
// 会话内容分叉 → 11148。上游语义模拟：key 当会话标识，会话记录的历史必须
// 是新请求历史的前缀，否则按「start a new conversation」拒收。
func Test11148StickySalt(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	sessions := map[string][]string{} // cache key -> 已缓存的消息序列

	messagesOf := func(body []byte) []string {
		var obj struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &obj)
		out := make([]string, 0, len(obj.Messages))
		for _, m := range obj.Messages {
			b, _ := json.Marshal(m)
			out = append(out, string(b))
		}
		return out
	}

	c := testClient(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var obj struct {
			Key string `json:"prompt_cache_key"`
		}
		_ = json.Unmarshal(body, &obj)
		msgs := messagesOf(body)
		mu.Lock()
		keys = append(keys, obj.Key)
		stored, seen := sessions[obj.Key]
		diverged := false
		if seen {
			if len(msgs) < len(stored) {
				diverged = true
			} else {
				for i := range stored {
					if stored[i] != msgs[i] {
						diverged = true
						break
					}
				}
			}
		}
		if !diverged {
			sessions[obj.Key] = msgs
		}
		mu.Unlock()
		if diverged {
			return jsonResp(400, `{"code":11148,"msg":"tool calls and tool results do not match"}`), nil
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	conv := func(history ...string) []byte {
		msgs := make([]map[string]string, 0, len(history))
		msgs = append(msgs, map[string]string{"role": "system", "content": "固定系统提示"})
		msgs = append(msgs, map[string]string{"role": "user", "content": "首个问题"})
		for _, h := range history {
			msgs = append(msgs, map[string]string{"role": "assistant", "content": h})
		}
		b, _ := json.Marshal(map[string]any{
			"model":           "glm-5.2",
			"conversation_id": "conv-sticky-salt",
			"messages":        msgs,
		})
		return b
	}

	// 轮1：新会话直接成功
	if _, st, _, err := c.ChatStream(a, conv("回答一")); err != nil || st != 200 {
		t.Fatalf("turn1: status=%d err=%v", st, err)
	}
	// 轮2：压缩重写历史中部（首条 system/user 保留 → 键稳定）→ 11148 → 加盐重试成功
	if _, st, _, err := c.ChatStream(a, conv("压缩摘要：之前讨论了 X")); err != nil || st != 200 {
		t.Fatalf("turn2: status=%d err=%v", st, err)
	}
	// 轮3：在轮2 基础上续问 → 应直接带同一盐出站，不再撞 11148
	if _, st, _, err := c.ChatStream(a, conv("压缩摘要：之前讨论了 X", "回答二")); err != nil || st != 200 {
		t.Fatalf("turn3: status=%d err=%v", st, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 4 {
		t.Fatalf("upstream calls=%d (keys=%v), want 4", len(keys), keys)
	}
	if keys[0] != keys[1] {
		t.Fatalf("base key should be stable: %v", keys)
	}
	if keys[2] != keys[3] {
		t.Fatalf("salted key must be sticky after first resalt: %v", keys)
	}
	if keys[2] == keys[0] || !strings.HasPrefix(keys[2], keys[0]) {
		t.Fatalf("salted key must extend base: %v", keys)
	}
}
