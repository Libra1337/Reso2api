// adapt.go — 上游契约缺口的网关侧适配（尽力而为层）。
//
// 依据 2026-10-28 KVV 验证报告（LynShen 网关，kimi-k3）：上游在 K3 新版
// 对话模板上有四类缺口——动态工具（消息内 tools）一律 400/忽略、
// json_schema 不写进 prompt 也不约束输出、thinking.effort 与
// reasoning_effort 并存时不按契约忽略后者、以 assistant 开头的会话 400。
// 上游认标准顶层 tools（工具 Schema 408/408 全过）、认 json_object、
// 识图通路正常——网关在出站前把缺口的语义翻译成上游认得的形式。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// AdaptUpstreamGaps 出站契约适配总入口（调用方已用 strings.Contains 预筛）。
func AdaptUpstreamGaps(obj map[string]any) {
	normalizeEffortPrecedence(obj)
	hoistMessageTools(obj)
	assistantHeadStub(obj)
	emulateJSONSchema(obj)
}

// normalizeEffortPrecedence 官方契约：thinking.effort 与 reasoning_effort
// 并存时后者应被忽略（KVV 实测上游未忽略，两个档位都进 prompt）。
// 实现：thinking.effort 存在时以其覆盖 reasoning_effort（snake/camel 双写）
// ——覆盖而非删除，兼顾 deepseek 系「必须带 reasoning_effort 才开思考」
// 的上游行为（见 thinking.go）。
func normalizeEffortPrecedence(obj map[string]any) {
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		return
	}
	effort, _ := th["effort"].(string)
	if strings.TrimSpace(effort) == "" {
		return
	}
	if cur, _ := obj["reasoning_effort"].(string); cur != effort {
		obj["reasoning_effort"] = effort
		delete(obj, "reasoningEffort")
		log.Printf("adapt: effort precedence thinking.effort=%s overrides reasoning_effort=%s", effort, cur)
	}
}

// hoistMessageTools 动态工具提升：把消息内嵌的 tools 数组合并到顶层
// tools（上游对消息内 tools 一律 400/忽略，顶层 tools 完整支持）。
// 提升后变空（content/reasoning 全空）的 system/developer 消息删除
// ——上游对空 content 的 system 消息固定 400（KVV 直连实测）。
func hoistMessageTools(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	var hoisted []any
	drop := map[int]bool{}
	for i, mi := range msgs {
		msg, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		tools, ok := msg["tools"].([]any)
		if !ok || len(tools) == 0 {
			continue
		}
		hoisted = append(hoisted, tools...)
		delete(msg, "tools")
		if msgBareAfterHoist(msg) {
			drop[i] = true
		}
	}
	if len(hoisted) == 0 {
		return
	}
	top, _ := obj["tools"].([]any)
	obj["tools"] = append(append(make([]any, 0, len(top)+len(hoisted)), top...), hoisted...)
	if len(drop) > 0 {
		kept := make([]any, 0, len(msgs)-len(drop))
		for i, mi := range msgs {
			if !drop[i] {
				kept = append(kept, mi)
			}
		}
		obj["messages"] = kept
	}
	log.Printf("adapt: hoisted %d message-embedded tools to top-level (dropped %d emptied messages)", len(hoisted), len(drop))
}

// msgBareAfterHoist tools 被提升后消息是否已无信息量（content/reasoning 空）。
// user/tool 消息保留（上游接受非空系统位之外的形态，删 user 改变语义）。
func msgBareAfterHoist(m map[string]any) bool {
	role, _ := m["role"].(string)
	lr := strings.ToLower(strings.TrimSpace(role))
	if lr != "system" && lr != "developer" {
		return false
	}
	for _, k := range []string{"content", "reasoning", "reasoning_content"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return false
		}
		if a, ok := m[k].([]any); ok && len(a) > 0 {
			return false
		}
	}
	for k := range m {
		switch k {
		case "role", "content", "reasoning", "reasoning_content":
		default:
			return false // 还有其他字段，不判空
		}
	}
	return true
}

// assistantHeadStub 上游对 assistant 开头的会话固定 400（KVV 直连实测）。
// 预置一条最小 user 占位让会话可用；文本固定且极短，对计量的扰动可预期。
func assistantHeadStub(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	msg, ok := msgs[0].(map[string]any)
	if !ok {
		return
	}
	if roleOf(msg) == "assistant" {
		obj["messages"] = append([]any{map[string]any{"role": "user", "content": "(continue)"}}, msgs...)
		log.Printf("adapt: prepended user stub before assistant-head conversation")
	}
}

// emulateJSONSchema json_schema 结构化输出仿真：上游 json_schema 不写进
// prompt 也不约束输出，但 json_object 正常。改写为 json_object 并把
// Schema 以指令文本注入消息尾部（近因位置，约束力最强）。
// schema 缺失/非法时不动（交上游按原样处理）。
func emulateJSONSchema(obj map[string]any) {
	rf, ok := obj["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		return
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		return
	}
	schema, ok := js["schema"].(map[string]any)
	if !ok || len(schema) == 0 {
		return
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return
	}
	obj["response_format"] = map[string]any{"type": "json_object"}
	msgs, _ := obj["messages"].([]any)
	instr := map[string]any{
		"role": "system",
		"content": "Respond with a single JSON object that strictly conforms to this JSON Schema. " +
			"Do not include any text outside the JSON.\n" + string(raw),
	}
	obj["messages"] = append(msgs, instr)
	log.Printf("adapt: json_schema emulated via json_object + injected schema instruction (%dB)", len(raw))
}
