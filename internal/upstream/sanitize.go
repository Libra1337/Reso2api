// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",                             // 身份句（截断前缀即可命中）
	"Main branch (",                                   // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码（PR #46）
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
// 身份句的匹配串不带结尾标点（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾，桌面版（claude-desktop-3p / Agent SDK）以逗号接后继内容——
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，原有标点原样保留）。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截；give→provide 一词之差即可绕过。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：请求体出现裸 11128 即整单拦截（与其上下文无关）。
		// 插入连字符保留可读性（零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
}

// sanitizeText 单段文本净化 = 已知句精确改写 + 通用 prose 变形。
func sanitizeText(text string) string {
	out := fingerprintRewrite(text)
	out = mutateProse(out)
	return out
}

// fingerprintRewrite 已知指纹句精确改写（一键换词，语义不变）；
// 不含已知指纹时原样返回（零分配快速路径）。
func fingerprintRewrite(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	return strings.TrimSpace(text)
}

// mutateProse 通用反逐字匹配变形：对 prose（自然语言）行的首词后追加一个空格。
//
// 背景：上游内容审核按逐字精确匹配黑名单模板句（实测仅扫 messages[].content，
// tool_calls.arguments 与 tool 输出不扫）。各 agent 客户端（Claude Code / Codex /
// ZCode / Cursor …）的提示词模板句无法穷举，故对 prose 行统一做一字节变形——
// 黑名单句子只要落在 prose 行内必然失配；额外空格对语义与 tokenization 无影响。
//
// prose 判定（保守，代码/表格/路径行不动）：
//   - 行首无缩进（缩进多为代码）
//   - 含 ≥2 个空格分词（单词行/标题锚点不动）
//   - 无代码特征：{}[]<>|`\\=;:#  至多含 ':' 一处（时间 09:00）与 '()'
//   - 字母占比 ≥ 60%（中文行天然跳过——无空格分词）
func mutateProse(s string) string {
	if !strings.Contains(s, " ") {
		return s
	}
	lines := strings.Split(s, "\n")
	changed := false
	for i, line := range lines {
		if len(line) < 24 { // 短行（标题/标签）不动
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if !isProseLine(line) {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp <= 0 || sp+1 >= len(line) || line[sp+1] == ' ' {
			continue
		}
		lines[i] = line[:sp+1] + " " + line[sp+1:]
		changed = true
	}
	if !changed {
		return s
	}
	return strings.Join(lines, "\n")
}

// isProseLine 判定一行是否自然语言散文。
func isProseLine(line string) bool {
	letters, runes := 0, 0
	for _, r := range line {
		runes++
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letters++
		case r == ' ' || r == ',' || r == '.' || r == '\'' || r == '"':
		case r == ':' || r == '(' || r == ')' || r == '-' || r == '?' || r == '!':
		default:
			return false // 含其他字符（代码/中文/路径）按非 prose 处理
		}
	}
	return runes >= 12 && letters*10 >= runes*6 // 字母占比≥60%
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...），快速路径漏掉时再落正则（(?i)）兜底。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeHdrRe.MatchString(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeMessages 净化 messages 中的 content；任一命中返回 true。
// 净化 = 已知句精确改写 + 通用 prose 变形（反逐字匹配，兜底未知模板句）。
// role=tool 的消息（工具结果：文件内容/命令输出）跳过——上游审核不扫工具输出，
// 变异纯属破坏数据（模型读到被插空格的文件内容，写回时会引入脏字符）。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "tool") {
			continue // 工具输出不扫（上游审核不扫工具输出，变异破坏数据）
		}
		// content 与 tool_calls 各自独立判断：content 可为 null（工具调用轮），
		// 早期版本 content 缺失即 continue，这类消息的 tool_calls 完全不被净化——
		// 历史里写进工具参数的被拦字符串（文件名、命令、写入内容）会原样漏出。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
// arguments 是字符串化的 JSON（不是对象），按文本走 sanitizeText 即可。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}
