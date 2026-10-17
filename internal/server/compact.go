// compact.go 超限上下文网关侧裁剪（config.context_compact）。
//
// 上游对单请求有硬上限（workbuddy 家族实测 1048576 tokens；超限回 11115
// "prompt is too long"，对客户端即 400 context_length_exceeded）。下游客户端
// （如 monoize 背后的聊天 UI）往往全量回放历史、不自我约束，会话只增不减，
// 一旦超限每次请求都 400，还容易被当故障无限重试（2026-10-17 实测一会话
// 15+ 连击换号打）。网关在出站前裁剪：保留 system 段 + 预算内最多的最近消息，
// 最早对话段丢弃，插入一条截断标记消息告知模型。未超限的请求原样通过——
// 零开销、不动前缀缓存。
//
// 传输层 gzip 之类压缩对 token 上限无效（上游按解码后的 token 计数），有效
// 的只有减少内容本身；LLM 摘要式压缩（旧对话摘要成一段）成本高、时延大，
// 留作后续可选，当前为确定性裁剪。
package server

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
)

// compactBytesPerToken 粗估系数（与 applyOverflow 同源：zh 混合语料 ~2.6 字节/token）。
const compactBytesPerToken = 2.6

// imageExtraTokens 每个图片内容项的保守附加 token（视觉项不占字节占 token）。
const imageExtraTokens = 1200

// compactContext 超限时裁剪请求体。返回（新 body, 是否发生裁剪）。
// 不超限或裁不动（无 messages / system 段自身超限 / 尾巴装不下最少条数）
// → 原样返回，交给错误路径报官方 context_length_exceeded。
func (h *Handler) compactContext(body []byte, session string) ([]byte, bool) {
	cc := h.cfg.ContextCompact.Normalized()
	if !cc.Enabled {
		return body, false
	}
	budget := int64(float64(cc.HardLimitTokens) * cc.ReserveRatio)
	last := int64(0)
	if session != "" {
		h.sessInMu.Lock()
		last = h.sessLastIn[session]
		h.sessInMu.Unlock()
	}
	est := int64(float64(len(body)) / compactBytesPerToken)
	if last > est {
		est = last // 会话上一轮真实 in_tokens 是更准的下界
	}
	if est <= budget {
		return body, false
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, false
	}
	rawMsgs, _ := root["messages"].([]any)
	if len(rawMsgs) == 0 {
		return body, false
	}

	// system/developer 前缀整体保留（角色设定不可丢）。
	sysEnd := 0
	for sysEnd < len(rawMsgs) {
		if role, _ := messageRole(rawMsgs[sysEnd]); role == "system" || role == "developer" {
			sysEnd++
			continue
		}
		break
	}
	if sysEnd >= len(rawMsgs) {
		return body, false
	}

	// 从尾部向前按预算收消息。
	var acc int64
	cut := len(rawMsgs)
	for cut > sysEnd {
		sz := messageTokens(rawMsgs[cut-1])
		if acc+sz > budget {
			break
		}
		acc += sz
		cut--
	}
	if cut <= sysEnd || len(rawMsgs)-cut < cc.MinTailMessages {
		return body, false // 单条消息巨长装不下尾巴：不硬拆消息体
	}
	// tool 配对安全：cut 落在 tool 结果上会孤儿化其 assistant tool_call，
	// 向前扩到配对调用方（连续多条 tool 结果全跨）。
	for cut > sysEnd {
		if role, _ := messageRole(rawMsgs[cut]); role != "tool" {
			break
		}
		cut--
	}
	dropped := cut - sysEnd
	if dropped <= 0 {
		return body, false
	}
	markerTokens := messageTokens(map[string]any{"role": "system", "content": "context notice"})
	var sysTokens int64
	for i := 0; i < sysEnd; i++ {
		sysTokens += messageTokens(rawMsgs[i])
	}
	if sysTokens+markerTokens+acc > int64(cc.HardLimitTokens) {
		return body, false // system 段自身就吃满预算：裁了也白裁
	}

	marker := map[string]any{
		"role": "system",
		"content": "[context notice] " + strconv.Itoa(dropped) +
			" earlier messages were omitted to fit the model context window. Continue with the recent messages below.",
	}
	msgs := make([]any, 0, sysEnd+1+(len(rawMsgs)-cut))
	msgs = append(msgs, rawMsgs[:sysEnd]...)
	msgs = append(msgs, marker)
	msgs = append(msgs, rawMsgs[cut:]...)
	root["messages"] = msgs
	nb, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	log.Printf("context compacted session=%.16s est_tokens=%d budget=%d dropped_msgs=%d kept_msgs=%d bytes %d->%d",
		session, est, budget, dropped, len(rawMsgs)-cut, len(body), len(nb))
	return nb, true
}

// messageRole 取消息 role 字段（非对象/缺 role 返回 ""）。
func messageRole(m any) (string, bool) {
	mm, ok := m.(map[string]any)
	if !ok {
		return "", false
	}
	r, _ := mm["role"].(string)
	return r, true
}

// messageTokens 单条消息 token 粗估：JSON 字节 / 2.6 + 图片项附加 + 每条固定开销。
func messageTokens(m any) int64 {
	raw, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	s := string(raw)
	n := int64(float64(len(raw)) / compactBytesPerToken)
	if k := strings.Count(s, "image_url"); k > 0 {
		n += imageExtraTokens * int64(k)
	}
	if k := strings.Count(s, "input_image"); k > 0 {
		n += imageExtraTokens * int64(k)
	}
	return n + 4
}
