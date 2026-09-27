package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

func chatBodyWithConv(conv string) []byte {
	b := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]`
	if conv != "" {
		b += `,"metadata":{"conversation_id":"` + conv + `"}`
	}
	return []byte(b + `}`)
}

// 注入生成 wb2a-<uid8>-<convHex>；同账号同会话稳定，跨账号隔离。
func TestInjectPromptCacheKeyStableAndIsolated(t *testing.T) {
	b1 := InjectPromptCacheKey(chatBodyWithConv("conv-1"), "uid-abcdef-1234", "")
	var o1 map[string]any
	if err := json.Unmarshal(b1, &o1); err != nil {
		t.Fatal(err)
	}
	k1, _ := o1["prompt_cache_key"].(string)
	if !strings.HasPrefix(k1, "wb2a-uid-abcd-") {
		t.Fatalf("key=%q want wb2a-uid-abc-* prefix", k1)
	}
	// 同账号同会话 → 同 key
	b2 := InjectPromptCacheKey(chatBodyWithConv("conv-1"), "uid-abcdef-1234", "")
	var o2 map[string]any
	_ = json.Unmarshal(b2, &o2)
	if o2["prompt_cache_key"] != k1 {
		t.Fatalf("same conv must give same key: %v vs %v", o2["prompt_cache_key"], k1)
	}
	// 换账号 → 不同 key（跨账号绝不碰撞）
	b3 := InjectPromptCacheKey(chatBodyWithConv("conv-1"), "other-acct-99", "")
	var o3 map[string]any
	_ = json.Unmarshal(b3, &o3)
	if o3["prompt_cache_key"] == k1 {
		t.Fatal("different uid must give different key")
	}
	// 换会话 → 不同 key
	b4 := InjectPromptCacheKey(chatBodyWithConv("conv-2"), "uid-abcdef-1234", "")
	var o4 map[string]any
	_ = json.Unmarshal(b4, &o4)
	if o4["prompt_cache_key"] == k1 {
		t.Fatal("different conv must give different key")
	}
}

// 客户端显式 key 作会话源：同键同前缀稳定，同键不同 system 必须分开；
// 入参 conversationID 在 body 无会话字段时兜底。
func TestInjectPromptCacheKeyPriority(t *testing.T) {
	keyOf := func(body string, uid string) string {
		var o map[string]any
		_ = json.Unmarshal(InjectPromptCacheKey([]byte(body), uid, "conv"), &o)
		k, _ := o["prompt_cache_key"].(string)
		return k
	}
	mainAgent := `{"model":"m","prompt_cache_key":"client-key","messages":[{"role":"system","content":"You are Codex"},{"role":"user","content":"fix bug"}]}`
	reviewer := `{"model":"m","prompt_cache_key":"client-key","messages":[{"role":"system","content":"You are judging one planned action"},{"role":"user","content":"fix bug"}]}`
	k1 := keyOf(mainAgent, "u1")
	if !strings.HasPrefix(k1, "wb2a-u1-") {
		t.Fatalf("client key must be rewritten with account scope, got %v", k1)
	}
	if keyOf(mainAgent, "u1") != k1 {
		t.Fatal("same client key + same prefix must be stable")
	}
	if keyOf(reviewer, "u1") == k1 {
		t.Fatal("same client key with different system prefix must not share cache key")
	}
	if keyOf(mainAgent, "u2") == k1 {
		t.Fatal("same client key on different accounts must not share cache key")
	}
	fallback := InjectPromptCacheKey(chatBodyWithConv(""), "u1", "fallback-conv")
	var o2 map[string]any
	_ = json.Unmarshal(fallback, &o2)
	if k, _ := o2["prompt_cache_key"].(string); !strings.HasPrefix(k, "wb2a-u1-") {
		t.Fatalf("fallback conv key missing: %v", o2["prompt_cache_key"])
	}
}

// ChatStreamConv 出站链路注入缓存键（捕获上游请求体断言）。
func TestChatStreamConvInjectsCacheKey(t *testing.T) {
	var captured string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		captured = string(raw)
		return textResp(200, "data: [DONE]\n\n"), nil
	})
	body := chatBodyWithConv("conv-9")
	_, _, _, _ = c.ChatStreamConv(&auth.Auth{UID: "uid-x", AccessToken: "at"}, body, "")
	var o map[string]any
	if err := json.Unmarshal([]byte(captured), &o); err != nil {
		t.Fatalf("captured body not json: %s", captured)
	}
	if k, _ := o["prompt_cache_key"].(string); !strings.HasPrefix(k, "wb2a-") {
		t.Fatalf("outbound body missing prompt_cache_key: %s", captured)
	}
}

// 首条 user 前缀相同（共用 8KB system-reminder）但提问不同的两个会话，必须拿到不同键。
func TestInjectPromptCacheKeySharedLongPrefix(t *testing.T) {
	reminder := strings.Repeat("<system-reminder>AGENTS.md shared text</system-reminder>\n", 200)
	mk := func(q string) string {
		b, _ := json.Marshal(map[string]any{
			"model": "m",
			"messages": []any{
				map[string]any{"role": "system", "content": "You are ZCode"},
				map[string]any{"role": "user", "content": reminder + q},
			},
		})
		var o map[string]any
		_ = json.Unmarshal(InjectPromptCacheKey(b, "u1", ""), &o)
		k, _ := o["prompt_cache_key"].(string)
		return k
	}
	a, b := mk("给账号全部导入服务器"), mk("写一个排序算法")
	if a == "" || a == b {
		t.Fatalf("sessions sharing a long prefix must not share a key: %q vs %q", a, b)
	}
	if mk("给账号全部导入服务器") != a {
		t.Fatal("same session must keep a stable key")
	}
}
