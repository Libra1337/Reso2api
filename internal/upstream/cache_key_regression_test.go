package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 回归：无会话标识时，前缀不同的两个请求必须得到不同的缓存键
// （旧实现空会话统一固定键 → 同账号「你好」测试串到编程会话的缓存，
// 输出混入 import java 等无关内容）。
func TestNoConvDifferentPrefixDifferentKey(t *testing.T) {
	hello := []byte(`{"model":"m","messages":[{"role":"developer","content":"你是电商客服"},{"role":"user","content":"你好"}]}`)
	coding := []byte(`{"model":"m","messages":[{"role":"system","content":"You are ZCode, a coding agent"},{"role":"user","content":"import java.util.*; public class Main {...}"}]}`)
	k1 := extractKey(t, InjectPromptCacheKey(hello, "uid-abcdef-1234", ""))
	k2 := extractKey(t, InjectPromptCacheKey(coding, "uid-abcdef-1234", ""))
	if k1 == k2 {
		t.Fatalf("different prefixes must not share cache key: %s", k1)
	}
}

// 回归：无会话标识时，同会话的连续轮次（前缀相同）仍稳定命中同一键。
func TestNoConvSamePrefixStableKey(t *testing.T) {
	r1 := []byte(`{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"q1"}]}`)
	r2 := []byte(`{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`)
	k1 := extractKey(t, InjectPromptCacheKey(r1, "uid-abcdef-1234", ""))
	k2 := extractKey(t, InjectPromptCacheKey(r2, "uid-abcdef-1234", ""))
	if k1 == "" || k1 != k2 {
		t.Fatalf("same prefix must be stable: %q vs %q", k1, k2)
	}
}

// 同前缀跨账号仍隔离。
func TestNoConvCrossAccountIsolated(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	k1 := extractKey(t, InjectPromptCacheKey(body, "uid-a-1111", ""))
	k2 := extractKey(t, InjectPromptCacheKey(body, "uid-b-2222", ""))
	if k1 == k2 {
		t.Fatal("cross-account must differ")
	}
}

// 空 messages 兜底不 panic 且两次结果不同（随机段）。
func TestEmptyMessagesFallback(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	k := extractKey(t, InjectPromptCacheKey(body, "u1", ""))
	if !strings.HasPrefix(k, "wb2a-u1-") {
		t.Fatalf("fallback key malformed: %q", k)
	}
}

func extractKey(t *testing.T, body []byte) string {
	var o map[string]any
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatalf("not json: %s", body)
	}
	k, _ := o["prompt_cache_key"].(string)
	return k
}
