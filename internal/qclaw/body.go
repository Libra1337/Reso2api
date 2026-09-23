// body.go 出站请求体改写。
package qclaw

import (
	"encoding/json"
	"strings"
)

// PrepareBody 改写发往 QClaw LLM 代理的 chat 请求体：
//  1. 兜底补 system 消息：上游校验强制要求 messages 含 system（缺失时
//     整单 invalid_request_error "invalid request"，实测 0.2.37）；
//  2. developer 角色归一为 system（同 WorkBuddy 渠道惯例）；
//  3. stream 透传（代理对流式/非流式均接受）。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	// 强制出站流式（客户端要非流式时由 Aggregate 聚合）：上游尊重
	// stream:false 返回整包 JSON 对象，而本渠道 Stream/Aggregate 管线
	// 统一按 SSE 处理。
	obj["stream"] = true
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return src
	}
	hasSystem := false
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			m["role"] = "system"
			role = "system"
		}
		if strings.EqualFold(strings.TrimSpace(role), "system") {
			hasSystem = true
		}
	}
	if !hasSystem {
		out := make([]any, 0, len(msgs)+1)
		out = append(out, map[string]any{"role": "system", "content": "You are a helpful assistant."})
		out = append(out, msgs...)
		obj["messages"] = out
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}
