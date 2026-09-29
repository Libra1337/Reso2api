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
//  1. body 已带 prompt_cache_key → 作会话源（并入账号与前缀摘要后重写）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自网关解析的会话标识）
//  4. 全都没有 → 用请求前缀（首条 system/developer + 首条 user 消息）哈希
//
// 安全约束——按账号隔离 + 按前缀隔离：
// 生成键格式 `wb2a-<uid8>-<convHex>`，uid8 是账号 UID 前 8 字符，跨账号绝不
// 相同；convHex 在无会话标识时取请求前缀哈希，前缀不同的请求绝不共享缓存键
// （此前空会话统一落一个固定键，导致同账号不同请求互相命中错前缀缓存、
// 输出串入其他会话内容，即「你好」测试返回 Java 代码片段的根因）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 客户端自带键不能原样透传：Codex 等客户端让主 agent 与审查子 agent
	// 共用同一会话键，但两者 system 前缀不同——上游按键认会话，前缀不同的
	// 请求共享一个键就会串入对方内容（09-27 实测同键下出现两种 system）。
	// 客户端键只作会话源，再并入账号与前缀摘要，保证「同键同前缀」才共享。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		obj["prompt_cache_key"] = buildCacheKey(uid, "client:"+existing+"|"+prefixDigest(obj))
		out, err := json.Marshal(obj)
		if err != nil {
			return body
		}
		return out
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
	if conv != "" {
		// 会话标识同样可能被多个子 agent 共用，并入前缀摘要隔离。
		conv = conv + "|" + prefixDigest(obj)
	} else {
		// 无任何会话标识：退化为「请求前缀哈希」——只有前缀真正相同的请求
		// （同会话的连续轮次、system+首条 user 一致）才共享缓存，语义上
		// 与上游前缀缓存的命中条件对齐，杜绝跨会话串缓存。
		conv = prefixDigest(obj)
		if conv == "" {
			// 兜底：极端空消息体，用账号内随机段强制不与他人共享。
			conv = "rand:" + hex.EncodeToString(randomBytes())
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

// ConversationFingerprint 返回请求的稳定会话指纹，供会话级粘性路由使用。
// 优先级与 InjectPromptCacheKey 的会话源完全一致：
//  1. 客户端 prompt_cache_key（哈希收口，防超长键撑爆内存）
//  2. conversation_id / conversationId / metadata 嵌套形态
//  3. 请求前缀摘要（首条 system + 首条 user 全文哈希）
//  4. 全都没有 → 空串（调用方退化为渠道级粘性）
//
// 同一会话的连续请求指纹稳定；不同会话几乎必然不同——粘性路由按它分组，
// 换号只影响单个会话的缓存键，不再牵连其他会话。
func ConversationFingerprint(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		sum := sha256.Sum256([]byte(existing))
		return "pk:" + hex.EncodeToString(sum[:16])
	}
	if v := strField(obj, "conversation_id"); v != "" {
		return "conv:" + v
	}
	if v := strField(obj, "conversationId"); v != "" {
		return "conv:" + v
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if v := strField(md, "conversation_id"); v != "" {
			return "conv:" + v
		}
		if v := strField(md, "conversationId"); v != "" {
			return "conv:" + v
		}
	}
	return prefixDigest(obj)
}

// prefixDigest 以首条 system/developer 消息 + 首条 user 消息的全文做哈希源。
// 多轮对话中这段前缀稳定不变，与上游前缀缓存的命中粒度一致。
// 必须全文哈希：ZCode/Claude Code 类客户端的首条 user 前 8KB 是各会话相同的
// system-reminder（AGENTS.md 等），真正区分会话的提问在其后，截断会让不同
// 会话撞同一个键、上游串入别的会话内容。
func prefixDigest(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	var firstInstr, firstUser string
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			if firstInstr == "" {
				firstInstr = contentText(mm["content"])
			}
		} else if role == "user" {
			if firstUser == "" {
				firstUser = contentText(mm["content"])
			}
			break // 首条 user 之后的内容每轮变化，不进哈希
		}
	}
	src := firstInstr + "\x00" + firstUser
	if strings.TrimSpace(src) == "\x00" {
		return ""
	}
	sum := sha256.Sum256([]byte(src))
	return "pfx:" + hex.EncodeToString(sum[:16])
}

// contentText 把消息 content 统一转成文本（string 或多段数组形态）。
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
