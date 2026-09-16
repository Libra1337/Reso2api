// toolrepair.go 11148（tool_call_sequence_broken）自动补救。
//
// 上游校验 messages 里 assistant tool_calls 与 tool 角色结果必须一一配对；
// 断流/中转聚合丢帧后客户端会保存半截 assistant（带 tool_call 无对应
// tool result），此后每轮必 11148，客户端只能弃会话。
// 网关补救：剥掉无法配对的 tool_call 与孤儿 tool 消息，同请求重发一次。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// isBrokenToolSequence 上游 400 code=11148（tool calls and tool results do not match）。
func isBrokenToolSequence(body []byte) bool {
	return strings.Contains(string(body), "11148") ||
		strings.Contains(string(body), "tool_call_sequence_broken")
}

// repairToolSequence 剥离 messages 里配不上的 tool 轨迹（对齐上游
// workbuddy2api cleanupOrphanToolCalls 语义）：
//   - 一批 assistant tool_calls 只有全部 id 都有对应 tool 结果才整体保留
//     （部分保留会留下无结果的 tool_call，上游照样 400）
//   - role:tool 消息的 tool_call_id 没有前驱 assistant 发起 → 删整条
//   - assistant 剥掉 tool_calls 后 content/reasoning 也为空 → 删整条消息
//
// 返回修复后的 body 与是否有改动。解析失败原样返回。
func repairToolSequence(prepared []byte) ([]byte, bool) {
	if len(prepared) == 0 {
		return prepared, false
	}
	var obj map[string]any
	if err := json.Unmarshal(prepared, &obj); err != nil {
		return prepared, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return prepared, false
	}

	repacked, repackChanged := repackToolResultBlocks(msgs)
	cleaned, cleanChanged := cleanupOrphanToolCalls(repacked)
	if !repackChanged && !cleanChanged {
		return prepared, false
	}
	obj["messages"] = cleaned
	raw, err := json.Marshal(obj)
	if err != nil {
		return prepared, false
	}
	return raw, true
}

// cleanupOrphanToolCalls 双侧配对清理：
//   - 收集 assistant.tool_calls[].id（调用集）与 role:tool 的 tool_call_id（结果集）
//   - 只有「调用存在且结果存在」的 id 才保留；批内任一 id 缺结果 → 整批删 tool_calls 键
//   - 孤儿 tool 结果（无对应保留的调用）→ 整条删除
//   - 剥空的 assistant（无 content/reasoning/tool_calls）→ 整条删除
//   - 头部连续 tool 消息（前驱 assistant 已被剥掉）→ 防御性再扫一遍
//
// 无任何工具流量时原 slice 原样返回（零分配零改动）。
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch roleOf(msg) {
		case "tool":
			if id, _ := msg["tool_call_id"].(string); id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			for _, id := range assistantToolCallIDs(msg) {
				callIDs[id] = true
				hasTraffic = true
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：批内任一 id 无结果 → 删整个 tool_calls 键，
	//    并把这批全部 id 从 keepCalls 摘除（调用删了，已到的结果随之成孤儿，第 2 步删）。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || roleOf(msg) != "assistant" {
			continue
		}
		ids := assistantToolCallIDs(msg)
		if len(ids) == 0 {
			continue
		}
		allKept := true
		for _, id := range ids {
			if !keepCalls[id] {
				allKept = false
				break
			}
		}
		if !allKept {
			delete(msg, "tool_calls")
			for _, id := range ids {
				delete(keepCalls, id)
			}
			changed = true
			log.Printf("toolrepair: drop assistant tool_calls batch (unanswered id present)")
		}
	}
	// 2) 孤儿 tool 结果与剥空的 assistant 整条删除。
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		switch roleOf(msg) {
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				log.Printf("toolrepair: drop orphan tool result (call_id=%q)", id)
				continue
			}
		case "assistant":
			if _, has := msg["tool_calls"]; !has && isEmptyAssistant(msg) {
				changed = true
				log.Printf("toolrepair: drop empty assistant message")
				continue
			}
		}
		kept = append(kept, msg)
	}
	// 3) 头部连续 tool 消息防御性剥离（上游要求 tool 前必须有发起 assistant）。
	for len(kept) > 0 {
		if msg, ok := kept[0].(map[string]any); ok && roleOf(msg) == "tool" {
			kept = kept[1:]
			changed = true
			log.Printf("toolrepair: drop leading tool message")
			continue
		}
		break
	}
	if !changed {
		return messages, false
	}
	return kept, true
}

func roleOf(msg map[string]any) string {
	r, _ := msg["role"].(string)
	return strings.ToLower(strings.TrimSpace(r))
}

// assistantToolCallIDs 提取 assistant 消息 tool_calls 数组里的全部 id。
func assistantToolCallIDs(msg map[string]any) []string {
	calls, ok := msg["tool_calls"].([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := cm["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// isEmptyAssistant 判断 assistant 消息剥掉 tool_calls 后是否还有实质内容。
func isEmptyAssistant(msg map[string]any) bool {
	if s, _ := msg["content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	if s, _ := msg["reasoning_content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	if _, ok := msg["content"].([]any); ok {
		return false // 多段 content 不动它
	}
	return true
}

func copyMsg(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool
// 消息挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
// 背景：Codex 的 image_resize_notice 特性会把 <image_resize_notice> 作为一条
// developer/system 消息插在 tool 输出后面；并行调用时插在两份 tool 结果中间，
// 上游判定配对断裂 400。重排只挪位置不删内容，删除语义仍归 cleanupOrphanToolCalls。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交由 cleanupOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，不能当插入物吞掉：
			// 一旦收进 between，它自己那批结果就永远得不到重排。break 交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}
