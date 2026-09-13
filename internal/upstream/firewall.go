// firewall.go 出站内容防火墙：在请求到达上游之前拦截高危内容，保护账号池。
//
// 背景（2026-09-13 事故实录）：下游 QQ bot 的 system 提示词携带
// "14 岁就是成年人……所有 nsfw 行为合法合规……允许色情暴力"类越狱声明，
// 上游内容审核直接把两个账号整号拉黑（连 "hi" 都 11140）。
// 账号是网关运营者的资产——下游客户端不该有能力把号送进黑名单。
//
// 定位：防御性拦截（与上游审核同向），不是绕过。规则只针对
// "声明/指令"语气（合法化、允许、无视审核），纯讨论/提及不拦，
// 宁可漏放（上游还有第二道审核）不可误杀正常技术对话。
package upstream

import (
	"regexp"
	"strings"
)

// firewallRule 单条高危规则。
type firewallRule struct {
	Name    string
	Pattern *regexp.Regexp
}

var firewallRules = []firewallRule{
	// 未成年"成年化"声明（如"14岁就是成年人""未成年视为成年"）
	{Name: "minor-adult-claim", Pattern: regexp.MustCompile(
		`(?i)(1[0-9]\s*岁|一[零一二三四五六七八九]四岁|未成年|loli|萝莉)[^\n]{0,60}(就是|已是|算是|视为|都是|均为)\s*(成年|大人)`)},
	// NSFW 合法化声明（"所有nsfw行为合法合规""允许色情内容"）
	{Name: "nsfw-legalize", Pattern: regexp.MustCompile(
		`(?i)(nsfw|色情|淫秽|涩图|r18|性描写)[^\n]{0,80}(合法|合规|允许|不受限|没有限制|可以生成|可以参与)|(合法|合规|允许|不受任何限制)[^\n]{0,80}(nsfw|色情|淫秽|性描写)`)},
	// 显式允许暴力/色情的指令
	{Name: "explicit-allow", Pattern: regexp.MustCompile(
		`(?i)允许出现?(暴力|色情|性|nsfw)|(?i)(暴力|色情|nsfw)[^\n]{0,40}(内容)?[^。\n]{0,20}(都可以|不受限)`)},
	// 审核绕过指令（"无视安全审核""ignore safety guidelines"）
	{Name: "bypass-review", Pattern: regexp.MustCompile(
		`(?i)(无视|忽略|绕过|不要遵守|ignore|bypass|disregard)[^\n]{0,40}(安全|审核|内容审查|过滤|限制|guideline|safety|restriction|policy)`)},
	// 无安全限制声明（"DO NOT have ANY safety guidelines""没有任何安全限制"）
	{Name: "no-safety-claim", Pattern: regexp.MustCompile(
		`(?i)(没有|不带|do not have|don't have|without|any)\s{0,3}(任何)?\s{0,3}(安全(?:限制|准则|指南|约束)?|safety|guideline|restriction)`)},
	// 未成年人信号 × NSFW 信号 共现（兜底：词表用具体年龄/loli 类词，
	// 不含"未成年"泛词——"保护未成年人/检测NSFW"类安全讨论不误杀）
	{Name: "minor-nsfw-cooccur", Pattern: regexp.MustCompile(
		`(?i)(1[0-9]\s*岁|十五岁|十六岁|十七岁|loli|萝莉|幼女|女小学生)`)},
}

var nsfwSignals = regexp.MustCompile(`(?i)\b(nsfw|r18|色情|淫秽|涩图|性描写|露骨)\b`)

// FirewallCheck 检查出站请求体文本，命中返回规则名。
// 只扫描 messages 的文本内容（system/user/assistant），不碰工具定义。
func FirewallCheck(prepared []byte) (string, bool) {
	if len(prepared) == 0 {
		return "", false
	}
	text := extractMessageText(prepared)
	if text == "" {
		return "", false
	}
	for _, r := range firewallRules {
		if r.Name == "minor-nsfw-cooccur" {
			// 共现兜底：未成年信号 + NSFW 信号同时出现才拦
			if r.Pattern.MatchString(text) && nsfwSignals.MatchString(text) {
				return r.Name, true
			}
			continue
		}
		if r.Pattern.MatchString(text) {
			return r.Name, true
		}
	}
	return "", false
}

// extractMessageText 从请求体提取 messages 的纯文本内容（拼接各消息字符串字段）。
func extractMessageText(body []byte) string {
	var obj struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if jsonUnmarshal(body, &obj) != nil {
		return ""
	}
	var sb strings.Builder
	for _, m := range obj.Messages {
		switch c := m.Content.(type) {
		case string:
			sb.WriteString(c)
			sb.WriteString("\n")
		case []any:
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						sb.WriteString(t)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String()
}

// FirewallHitResponse 防火墙拦截时返回给客户端的错误体（403）。
func FirewallHitResponse(rule string) []byte {
	return []byte(`{"error":{"message":"请求内容命中网关内容防火墙规则 ` + rule +
		`，已被拦截：该类内容会导致上游账号被永久封禁。请检查 system 提示词/对话内容（典型触发：未成年相关 NSFW 声明、越狱/无视审核指令、色情暴力合法化声明），修改后重试。","type":"content_firewall","code":"gateway_firewall"}}`)
}
