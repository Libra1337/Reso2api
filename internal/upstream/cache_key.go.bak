// cache_key.go 注入上游 prompt_cache_key 字段（费用优化，上游实测费用降 ~17×）。
//
// 移植自 workbuddy2api a751b18：同一客户端对同一账号的连续请求复用上游
// 前缀缓存（同段 8k 前缀 credit≈0.34 → 命中后 ≈0.02）。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自网关解析的会话标识）
//
// 安全约束——按账号隔离：生成键格式 `wb2a-<uid8>-<convHex>`，uid8 是账号
// UID 前 8 字符，跨账号绝不相同 → 缓存键绝不碰撞（跨账号复用同一 cache key
// 会让上游命中错账号的前缀缓存、泄露对方对话）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	} else if md, ok := obj["metadata"].(map[string]any); ok {
		// metadata 嵌套形态（Reso/responses 层的会话标识位置）
		if v := strField(md, "conversation_id"); v != "" {
			conv = v
		} else if v := strField(md, "conversationId"); v != "" {
			conv = v
		}
	}
	obj["prompt_cache_key"] = buildCacheKey(uid, conv)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成 `wb2a-<uid8>-<convHex>`：uid8 账号隔离段 +
// convHex = sha256(uid + "|" + conversation)[:16] hex 会话段（同账号同会话
// 稳定、不同会话不同；空会话仍保留账号隔离段）。
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	convHex := hex.EncodeToString(sum[:16])
	return "wb2a-" + uid8 + "-" + convHex
}

func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
