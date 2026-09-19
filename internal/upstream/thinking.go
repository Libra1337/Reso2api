// thinking.go DeepSeek 思维链开启：出站请求体注入 thinking:{type:"enabled"} + 默认档位。
//
// 根因（issue #43，Hermes 逆向官方客户端 codebuddy.js 已确认）：
// 官方客户端对 deepseek 系模型标记 thinkingFormat:"deepseek" + requiresReasoningContentOnAssistantMessages，
// 发请求时「开思考」必须显式带 thinking:{type:"enabled"}，否则上游默认按不思考应答
// （思维链不返回）。网关 payload 层此前完全不感知该字段，透传请求没有这个开关
// → 上游不给思维链；glm/kimi 走其他 thinkingFormat（qwen 系 enable_thinking 或默认开）所以正常。
//
// 打回修复（Hermes #43 验收实测）：
//
//	thinking.type=enabled 单一字段不足——真实上游 deepseek-v4-flash 对「不带 reasoning_effort」的裸请求
//	仍然按不思考应答（reasoning_content 长度 0），带 reasoning_effort:high 才有思维链。
//	逆向 codebuddy.js 证实：isThinkingEnabled = !!(reasoning_summary || reasoning_effort || reasoning?.effort)，
//	case "deepseek" 的 enabled 分支在实际出站里同时保留 reasoning_effort，官方「开思考」= thinking.type:enabled
//	+ 某档 effort；默认档来自 reasoning.defaultEffort ?? 兜底 "high"（configure thinking 无来源时 warn fallback to 'high'）。
//
// 行为对齐官方客户端（两路组合）：
//   - thinking.type 已显式 enabled / disabled → 客户端显式控制，绝不覆盖；disabled 时照抄 case 行为
//     删 reasoning_effort（snake/camel 双字段）。enabled 但缺 effort → 补默认档（官方 configure 行为）。
//   - 无 thinking / thinking.type 空 / 已有 reasoning_effort → 注入 {type:"enabled"} + 补默认档。
//   - 显式 reasoning_effort 一律不覆盖、不降级（降级交给 payload.go normalizeReasoningEffort）。
//   - 非 deepseek 模型（glm/kimi/qwen 等）→ 零改动。
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort 官方客户端默认档兜底（configure thinking 无来源时 warn fallback to 'high'，
// REASONING_SUPPLEMENTS.defaultEffort 亦为 "high"）。补入后走 normalizeReasoningEffort 降级管线，
// 模型不支持 high 时自动落到 ≤high 的最高支持档。
const defaultDeepSeekEffort = "high"

// isDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体；
// 前缀匹配对齐官方 thinkingFormat:"deepseek" 的判定口径，避免漏注。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// backfillReasoningContent DeepSeek 多轮一致性：历史 assistant 消息带 reasoning 痕迹时，
// 上游要求后续请求所有 assistant 消息都带 reasoning_content 字段（string，可为空串）
// ——即 requiresReasoningContentOnAssistantMessages（官方客户端 matches 规则）。
//
// 规则（对齐官方客户端逻辑）：
//   - 会话内任一 assistant 消息带非空 reasoning（string）或已有 reasoning_content 字段
//     → 所有 assistant 消息确保有 reasoning_content（string）：
//   - reasoning 非空且无 reasoning_content → 复制 reasoning 值
//   - 已有 reasoning_content → 原样保留（不覆盖）
//   - 两者皆无 → 补空串 ""
//   - 任何 assistant 均无 reasoning 痕迹 → 零改动（不白白加字段）。
//
// 仅 deepseek 模型生效（thinkingFormat:deepseek + requiresReasoningContent）。
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// 第一遍：检测是否有任何 reasoning 痕迹（非空 reasoning 或已有 reasoning_content）。
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return
	}
	// 第二遍：所有 assistant 消息补/复制 reasoning_content 字段。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue // 已有 → 不覆盖
		}
		if r, ok := msg["reasoning"].(string); ok {
			msg["reasoning_content"] = r
		} else {
			msg["reasoning_content"] = ""
		}
	}
}

// injectThinking 按 DeepSeek 思维链开关规则改写请求体。非 deepseek 零改动。
//
// 核心逻辑（对齐官方客户端）：
//   - 「开思考」必须 thinking.type=enabled + 有 effort 档位（Hermes #43 打回证据）。
//   - 显式 thinking.type 非空 → 客户端显式控制：enabled 缺 effort 时补默认档；
//     disabled 尊重并删 reasoning_effort（snake/camel 双字段）。
//   - 无 thinking / type 空 / 已有 effort → 注入 enabled 并补默认档（已有 effort 不覆盖）。
func injectThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	// 带图请求不注入思考（客户端显式开启的照常尊重）：视觉请求的思考期
	// 长达 5-20s（视觉编码+推理），下游客户端/中转普遍只等首字节 ~1s、
	// 正文 ~8-15s，等不到正文就掐流（实测 monoize 客户端 2h 掐 716 个请求，
	// 含 3.2MB 带图请求；上游与网关全程正常，回答被客户端放弃）。
	// 带图直答把正文首字压到 ~2-4s。
	if requestHasImage(obj) {
		if _, ok := obj["thinking"].(map[string]any); !ok {
			return
		}
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	// 显式控制分支：type 非空（enabled/disabled 均为明确意图）→ 不改 type。
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return // disabled：关思考且不带任何 effort（照抄客户端 case 行为）
		}
		ensureDeepSeekEffort(obj) // 显式 enabled 缺 effort → 补默认档
		applyThinkingBudget(obj)  // 显式开思考同样受小预算保护（自带 budget 不覆盖）
		stripSmallMaxTokens(obj)  // 过小 max_tokens 剥离（budget 不可靠，直接剥）
		return
	}
	// 无 thinking（或 thinking 非法非对象值）或 thinking 对象 type 缺失/为空：
	// 注入 enabled（客户端 case "deepseek" 行为）。有 reasoning_effort 也走此分支
	// （effort 保留给既有降级逻辑，开关照开）。
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj)
	applyThinkingBudget(obj)
	stripSmallMaxTokens(obj)
}

// minDeepSeekMaxTokens 小输出预算阈值：deepseek 开思考时 max_tokens 低于该值
// 直接剥离（对齐 cline2api-workers 对同症的处理——Cline 免费 deepseek 通道
// 带 max_tokens 一律 500 空响应，剥离后正常）。
//
// 实测 WorkBuddy 上游：max_tokens=300 时 thinking 可烧穿全部预算
//（budget_tokens 上游不保证尊重，10 连发 3 次 think=300 content 空），
// 剥掉 max_tokens 后思考自然结束、正文必有。剥离去掉的是客户端的截断上限，
// 自然停止长度不受影响（测试类请求实际总输出 100-400 token）。
const minDeepSeekMaxTokens = 1024

// stripSmallMaxTokens deepseek 开思考时剥离过小的 max_tokens（见
// minDeepSeekMaxTokens）。须在 thinking.type 确定为 enabled 后调用。
func stripSmallMaxTokens(obj map[string]any) {
	mt, ok := obj["max_tokens"].(float64)
	if !ok || mt <= 0 || mt >= minDeepSeekMaxTokens {
		return
	}
	delete(obj, "max_tokens")
}

// applyThinkingBudget 小 max_tokens 预算下限制思考预算，防思维链吃光输出预算
// 导致正文为空（finish_reason=length、content 空串）。实测：客户端测试类请求
// 只给 300 预算，effort=high 的思考可烧 300+ token，正文零余量；上游对
// budget_tokens 大致尊重（实测 budget=100 → 思考 113 token 后正常出正文）。
// 仅当：thinking.type=enabled、body 带正整数 max_tokens 且 ≤ 4096（大预算
// 无需限制）、thinking 未自带 budget_tokens（客户端显式预算不覆盖）时注入
// budget = max(64, max_tokens/3)，正文保留 ≥2/3 余量。
func applyThinkingBudget(obj map[string]any) {
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		return
	}
	if typ, _ := th["type"].(string); !strings.EqualFold(strings.TrimSpace(typ), "enabled") {
		return
	}
	if _, has := th["budget_tokens"]; has {
		return
	}
	mt, ok := obj["max_tokens"].(float64)
	if !ok || mt <= 0 || mt > 4096 {
		return
	}
	budget := int64(mt / 3)
	if budget < 64 {
		budget = 64
	}
	th["budget_tokens"] = budget
}

// ensureDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底）。
// 已有任一 effort → 不覆盖（显式档位不做任何改写，降级交给 normalizeReasoningEffort）。
func ensureDeepSeekEffort(obj map[string]any) {
	_, hasSnake := obj["reasoning_effort"]
	if hasSnake {
		return
	}
	_, hasCamel := obj["reasoningEffort"]
	if hasCamel {
		return
	}
	obj["reasoning_effort"] = defaultDeepSeekEffort
}
