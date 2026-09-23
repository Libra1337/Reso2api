// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. developer 角色归一为 system（上游 role 白名单，命中即 400 code=11128）
//  4. reasoning_effort 按模型 supportedEfforts 降级
//  5. 可选：消息内容指纹脱敏（sanitize.go）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBody 兼容旧调用：不脱敏、不降级。
func PrepareBody(src []byte) []byte {
	return PrepareBodyOpt(src, false)
}

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为还原（仅强制 stream + 归一化）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEfforts(src, sanitize, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	normalizeStop(obj)
	// max_completion_tokens -> max_tokens 翻译（吸收上游 PR #116）：OpenAI 新别名
	// （o-series 起引入），DeepSeek Harness 等新客户端只发别名；WorkBuddy 上游只认
	// max_tokens，别名透传被忽略后回落默认输出上限（实测 32000），长流任务被截。
	// 显式 max_tokens 优先（别名只删）；别名非正数值不翻译。
	translateMaxCompletionTokens(obj)
	// 多模态图片裸 base64 补全 data URL 前缀（见 images.go）：部分客户端发图
	// 不带 "data:image/xxx;base64," 前缀，上游模型提供方直接 400 code=11133，
	// 表现为视觉模型无法使用视觉功能。
	normalizeImageURLs(obj)
	// assistant 轮的图片上游直接丢弃（见 images.go）：挪到紧随其后的合成
	// user 消息，模型才可见。
	relocateAssistantImages(obj)
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	injectThinking(obj)
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	//（requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeStop 归一 stop 参数形态：部分客户端发字符串（stop:"..."），
// 上游严格要求数组（Request.stop []string），字符串原样透传必 400 code=11101。
// string → [string]；空数组/空字符串删除；[]string 原样。
func normalizeStop(obj map[string]any) {
	v, present := obj["stop"]
	if !present {
		return
	}
	switch sv := v.(type) {
	case string:
		if strings.TrimSpace(sv) == "" {
			delete(obj, "stop")
			return
		}
		obj["stop"] = []string{sv}
	case []any:
		out := make([]string, 0, len(sv))
		for _, item := range sv {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				out = append(out, str)
			}
		}
		if len(out) == 0 {
			delete(obj, "stop")
			return
		}
		obj["stop"] = out
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128（实测当前为精确小写匹配；用 EqualFold+TrimSpace
// 容错匹配，防上游后续收紧大小写/空白变体）。developer 是 OpenAI 新规范里
// system 的别名（Codex / Cursor 等新客户端用它承载 system 级指令），
// 改写为 system 不丢语义。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 workbuddy2api PR #116）。
// 规则：显式 max_tokens 优先（别名只删不译）；别名值为 0/null/负数/非数值不翻译
// （0/null 语义是「未设置」，走上游默认；负数是非法值，翻译等于把垃圾搬进 max_tokens）；
// 翻译后删别名字段（减少 body 体积与排障噪音）。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return
	}
	// json.Unmarshal 数字 -> float64（整数去整后回写，避免科学计数法进上游 body）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}
