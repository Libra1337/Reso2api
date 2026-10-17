package server

import (
	"encoding/json"
	"strings"
	"testing"

	"wild-work/internal/config"
)

// mkMsgs 造 n 条交替 user/assistant 消息，每条 content 长 sizeKB。
func mkMsgs(n, sizeKB int) []any {
	msgs := []any{}
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": strings.Repeat("字", sizeKB*1024/3)})
	}
	return msgs
}

func compactBody(t *testing.T, msgs []any, sys map[string]any, limit int, ratio float64) ([]byte, bool) {
	t.Helper()
	all := []any{}
	if sys != nil {
		all = append(all, sys)
	}
	all = append(all, msgs...)
	raw, _ := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "stream": true, "messages": all})
	cc := config.ContextCompactConfig{Enabled: true, HardLimitTokens: limit, ReserveRatio: ratio, MinTailMessages: 8}.Normalized()
	if ratio > 0 {
		cc.ReserveRatio = ratio
	}
	h := &Handler{cfg: Config{ContextCompact: cc}}
	return h.compactContext(raw, "sess-test")
}

// 超限请求被裁到预算内：system 保留、标记消息插入、最早消息丢弃、其余字段不丢。
func TestContextCompactTrims(t *testing.T) {
	msgs := mkMsgs(60, 30) // ~60×30KB/2.6 ≈ 707k tokens
	sys := map[string]any{"role": "system", "content": "You are helpful."}
	nb, ok := compactBody(t, msgs, sys, 200000, 0.85)
	if !ok {
		t.Fatal("oversized body should be compacted")
	}
	var out struct {
		Model    string           `json:"model"`
		Stream   bool             `json:"stream"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(nb, &out); err != nil {
		t.Fatalf("compacted body not json: %v", err)
	}
	if out.Model != "deepseek-v4.1-flash" || !out.Stream {
		t.Fatalf("other fields lost: model=%q stream=%v", out.Model, out.Stream)
	}
	if role, _ := out.Messages[0]["role"].(string); role != "system" {
		t.Fatalf("system prefix must stay first, got %v", out.Messages[0])
	}
	hasMarker := false
	for _, m := range out.Messages {
		if c, _ := m["content"].(string); strings.HasPrefix(c, "[context notice]") {
			hasMarker = true
		}
	}
	if !hasMarker {
		t.Fatalf("marker message missing:\n%s", nb[:400])
	}
	var acc int64
	for _, m := range out.Messages {
		raw, _ := json.Marshal(m)
		acc += int64(float64(len(raw))/compactBytesPerToken) + 4
	}
	if acc > 200000 {
		t.Fatalf("compacted est %d exceeds budget 200000", acc)
	}
}

// 未超限请求必须原样通过（零改动）。
func TestContextCompactPassthrough(t *testing.T) {
	msgs := mkMsgs(4, 5)
	nb, ok := compactBody(t, msgs, nil, 1048576, 0.85)
	if ok {
		t.Fatal("small body must not be compacted")
	}
	if len(nb) == 0 {
		t.Fatal("body must be returned")
	}
}

// 裁剪点不得孤儿化 tool 配对：cut 落在 tool 结果上时必须回扩包含其 assistant 调用方。
func TestContextCompactToolPairing(t *testing.T) {
	msgs := []any{}
	for i := 0; i < 50; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("字", 30*1024/3)})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": strings.Repeat("字", 30*1024/3)})
	}
	msgs = append(msgs,
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"},
		}}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"},
		map[string]any{"role": "user", "content": "final question"},
	)
	nb, ok := compactBody(t, msgs, nil, 200000, 0.85)
	if !ok {
		t.Fatal("should compact")
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(nb, &out); err != nil {
		t.Fatal(err)
	}
	// 找 tool 消息，其前一条必须是带 tool_calls 的 assistant
	for i, m := range out.Messages {
		if r, _ := m["role"].(string); r == "tool" {
			if i == 0 {
				t.Fatalf("tool message orphaned at index 0")
			}
			prev := out.Messages[i-1]
			if _, has := prev["tool_calls"]; !has {
				t.Fatalf("tool message %d not preceded by tool_call assistant: %v", i, prev)
			}
		}
	}
}
