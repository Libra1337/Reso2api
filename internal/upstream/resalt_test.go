package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// resaltCacheKey：有 prompt_cache_key → 换盐且其余字段不动；两次结果互不相同。
func TestResaltCacheKey(t *testing.T) {
	body := []byte(`{"model":"m","prompt_cache_key":"wb2a-u1-abcd1234","messages":[{"role":"user","content":"hi"}]}`)
	f1 := resaltCacheKey(body)
	if f1 == nil {
		t.Fatal("resalt must succeed with prompt_cache_key present")
	}
	var o1 map[string]any
	if err := json.Unmarshal(f1, &o1); err != nil {
		t.Fatalf("not json: %s", f1)
	}
	k1, _ := o1["prompt_cache_key"].(string)
	if !strings.HasPrefix(k1, "wb2a-u1-abcd1234-r") {
		t.Fatalf("salted key malformed: %q", k1)
	}
	if o1["model"] != "m" || o1["messages"] == nil {
		t.Fatal("other fields must be untouched")
	}
	f2 := resaltCacheKey(body)
	var o2 map[string]any
	_ = json.Unmarshal(f2, &o2)
	k2, _ := o2["prompt_cache_key"].(string)
	if k1 == k2 {
		t.Fatalf("two salts must differ: %q", k1)
	}
}

// 无 prompt_cache_key → nil（调用方放弃重试）。
func TestResaltCacheKeyNoKey(t *testing.T) {
	if got := resaltCacheKey([]byte(`{"model":"m","messages":[]}`)); got != nil {
		t.Fatalf("want nil, got %s", got)
	}
	if got := resaltCacheKey(nil); got != nil {
		t.Fatal("nil body must give nil")
	}
}

// 端到端：配对完整的请求遇 11148 → 不走 repair（changed=false），走 resalt 重发，
// 第二次（新 cache key）成功——客户端不可见错误。
func TestChatStreamConv11148FreshConversationRetry(t *testing.T) {
	var nReq int
	var lastKey string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		nReq++
		raw, _ := io.ReadAll(r.Body)
		var o map[string]any
		_ = json.Unmarshal(raw, &o)
		lastKey, _ = o["prompt_cache_key"].(string)
		if nReq == 1 {
			return jsonResp(400, `{"code":11148,"extError":{"code":"tool_call_sequence_broken"}}`), nil
		}
		return textResp(200, "data: [DONE]\n\n"), nil
	})
	// 完整配对的工具会话（repairToolSequence 无改动 → 落入 resalt 分支）。
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	rc, status, respBody, _ := c.ChatStreamConv(&auth.Auth{UID: "u", AccessToken: "at"}, body, "")
	if status != 200 || respBody != nil || rc == nil {
		t.Fatalf("retry must succeed, got status=%d body=%s rc=%v", status, respBody, rc)
	}
	_ = rc.Close()
	if nReq != 2 {
		t.Fatalf("expected exactly 2 upstream requests, got %d", nReq)
	}
	if !strings.Contains(lastKey, "-r") {
		t.Fatalf("second request must carry salted key, got %q", lastKey)
	}
}
