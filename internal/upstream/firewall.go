// firewall.go 出站内容防火墙：违禁内容在到达上游之前一律 403，保护账号池。
//
// 规则体系对齐国内外大模型平台公开内容政策的收敛红线
// （OpenAI Usage Policy / Anthropic Acceptable Use / Google Prohibited Use /
// 腾讯·阿里内容安全规范）：
//
//	A 级（关键词命中即拦——正常对话几乎不会出现）：
//	  csam            未成年性化内容（全平台最高红线，零容忍）
//	  terror          暴恐宣传
//	B 级（主题词 × 意图动词共现才拦——保护编程/安全研究的正常讨论）：
//	  weapon-cbrn     大规模杀伤武器制造（炸弹/生化/核）
//	  drug-synthesis  毒品合成
//	  malware         恶意软件开发（非防御研究）
//	  nude-deepfake   真实人物裸体/深伪生成
//	  self-harm       自杀自残教唆（关怀求助不拦）
//	声明类（越狱/合法化声明，2026-09-13 烧号事故实战沉淀）：
//	  minor-adult-claim / nsfw-legalize / explicit-allow /
//	  bypass-review / no-safety-claim / minor-nsfw-cooccur
//
// 不可绕过：本检查挂在 ChatStream 出站最底层（先于网络请求、后于全部
// 改写逻辑），命中直接 return——不存在降级重试、提示词替换等旁路。
// 配置 features.content_firewall=false 可整体关闭（默认开启）。
package upstream

import (
	"regexp"
	"strings"
	"time"

	"wild-work/internal/auth"
)

// firewallRule 单条高危规则。
type firewallRule struct {
	Name    string
	Pattern *regexp.Regexp
	// Pair 非空时为主题词×意图动词双信号共现（B 级）
	Pair *regexp.Regexp
}

var firewallRules = []firewallRule{
	// ── A 级：命中即拦 ──────────────────────────────────────────
	{Name: "csam", Pattern: regexp.MustCompile(
		`(?i)(儿童色情|儿童性|幼女(交|裸|色)?|未成年(性|色情|裸)|loli(交|色|con)?|\br18\s*loli|child\s*(porn|sexual|abuse)|csam|cp\s*(色情|content)|萝莉(色|裸|控色)|"少年.*性爱)`)},
	{Name: "terror", Pattern: regexp.MustCompile(
		`(?i)(圣战宣言|isil|isis(的)?(宣传|宣言|招募)|塔利班宣传|暴恐袭击宣言|制作(炸弹背心|自杀式)|加入(圣战|isil|isis))`)},
	// ── 声明类（事故实战规则）───────────────────────────────────
	{Name: "minor-adult-claim", Pattern: regexp.MustCompile(
		`(?i)(1[0-9]\s*岁|一[零一二三四五六七八九]四岁|未成年|loli|萝莉)[^\n]{0,60}(就是|已是|算是|视为|都是|均为)\s*(成年|大人)`)},
	{Name: "nsfw-legalize", Pattern: regexp.MustCompile(
		`(?i)(nsfw|色情|淫秽|涩图|r18|性描写)[^\n]{0,80}(合法|合规|允许|不受限|没有限制|可以生成|可以参与)|(合法|合规|允许|不受任何限制)[^\n]{0,80}(nsfw|色情|淫秽|性描写)`)},
	{Name: "explicit-allow", Pattern: regexp.MustCompile(
		`(?i)允许出现?(暴力|色情|性|nsfw)`)},
	{Name: "bypass-review", Pattern: regexp.MustCompile(
		`(?i)(无视|忽略|绕过|不要遵守|ignore|bypass|disregard)[^\n]{0,40}(安全|审核|内容审查|过滤|限制|guideline|safety|restriction|policy)`)},
	{Name: "no-safety-claim", Pattern: regexp.MustCompile(
		`(?i)(没有|不带|do not have|don't have|without|any)\s{0,3}(任何)?\s{0,3}(安全(?:限制|准则|指南|约束)?|safety|guideline|restriction)`)},
	// ── B 级：主题词 × 意图动词共现 ────────────────────────────
	{Name: "weapon-cbrn",
		Pattern: regexp.MustCompile(`(?i)(炸弹|爆炸装置|炸药|土制炸弹|核武器|脏弹|生化武器|神经毒剂|沙林毒气|vx毒剂|蓖麻毒素|c4炸药|ied|\bbomb\b|explosive device)`),
		Pair:    regexp.MustCompile(`(?i)(制造|制作|合成|合成方法|教程|配方|原料|怎么造|如何制造|how to (make|build)|make a bomb|build.*explosive)`)},
	{Name: "drug-synthesis",
		Pattern: regexp.MustCompile(`(?i)(冰毒|甲基苯丙胺|海洛因|摇头丸|meth(amphetamine)?|heroin|合成大麻|k粉|氯胺酮(粉)?)`),
		Pair:    regexp.MustCompile(`(?i)(合成|制作|提纯|配方|工艺|制毒|how to (make|synthesize)|synthesize)`)},
	{Name: "malware",
		Pattern: regexp.MustCompile(`(?i)(勒索软件|ransomware|僵尸网络|botnet|键盘记录器|keylogger|挖矿木马|蠕虫病毒(攻击)|ddos(攻击工具)?)`),
		Pair:    regexp.MustCompile(`(?i)(编写|开发|制作|源码|免杀|植入|用来攻击|攻击(他人|公司|别人)|\bwrite\b|\bcreate\b|\bdevelop\b|how to (write|create|deploy))`)},
	{Name: "nude-deepfake",
		Pattern: regexp.MustCompile(`(?i)(裸照|裸体|nude\s*(photo|pic|image|video)|deepfake|换脸(色情|裸)|ai去衣|脱衣(ai|软件))`),
		Pair:    regexp.MustCompile(`(?i)(生成|合成|制作|伪造|p(出来|成|图)|make|generate|create)`)},
	{Name: "self-harm",
		Pattern: regexp.MustCompile(`(?i)(自杀方法|自杀教程|怎么自杀|如何自杀|无痛自杀|自残方法|how to (kill myself|commit suicide|self.?harm)|suicide method)`)},
	// ── 兜底：具体未成年词 × NSFW 词共现 ────────────────────────
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
		switch {
		case r.Name == "minor-nsfw-cooccur":
			// 共现兜底：未成年信号 + NSFW 信号同时出现才拦
			if r.Pattern.MatchString(text) && nsfwSignals.MatchString(text) {
				return r.Name, true
			}
		case r.Pair != nil:
			// B 级：主题词与意图动词都命中才拦（防御研究/游戏编程不误杀）
			if r.Pattern.MatchString(text) && r.Pair.MatchString(text) {
				return r.Name, true
			}
		default:
			if r.Pattern.MatchString(text) {
				return r.Name, true
			}
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
	return []byte(`{"error":{"message":"请求内容命中网关内容防火墙规则 [` + rule +
		`]，已被拦截：该类内容违反大模型平台使用政策，会导致上游账号被永久封禁。请修改后重试。","type":"content_firewall","code":"gateway_firewall"}}`)
}

// FirewallEvent 一次防火墙拦截记录（内存环形，面板展示用）。
type FirewallEvent struct {
	At      int64  `json:"at"` // Unix 秒
	Rule    string `json:"rule"`
	UID     string `json:"uid,omitempty"`
	Model   string `json:"model,omitempty"`
	Snippet string `json:"snippet,omitempty"` // 列表摘要（前 80 字符）
	Content string `json:"content,omitempty"` // 完整内容（截 4000 字，弹窗展示）
}

const firewallEventCap = 500

// recordFirewallHit 记录一次拦截（环形截断）。
func (c *Client) recordFirewallHit(a *auth.Auth, rule, model string, prepared []byte) {
	content := extractMessageText(prepared)
	snippet := strings.ReplaceAll(content, "\n", " ")
	if r := []rune(snippet); len(r) > 80 {
		snippet = string(r[:80]) + "…"
	}
	if r := []rune(content); len(r) > 4000 {
		content = string(r[:4000]) + "\n…（内容过长截断）"
	}
	uid := ""
	if a != nil {
		uid = a.UID
	}
	c.fwMu.Lock()
	defer c.fwMu.Unlock()
	c.fwHits = append(c.fwHits, FirewallEvent{
		At: time.Now().Unix(), Rule: rule, UID: uid, Model: model,
		Snippet: snippet, Content: content,
	})
	if len(c.fwHits) > firewallEventCap {
		c.fwHits = c.fwHits[len(c.fwHits)-firewallEventCap:]
	}
}

// FirewallEvents 返回命中事件（旧→新）副本。
func (c *Client) FirewallEvents() []FirewallEvent {
	c.fwMu.Lock()
	defer c.fwMu.Unlock()
	out := make([]FirewallEvent, len(c.fwHits))
	copy(out, c.fwHits)
	return out
}

// FirewallEnabled 当前防火墙是否启用。
func (c *Client) FirewallEnabled() bool { return c.ContentFirewall }

// extractModel 从请求体提取模型名（事件展示用）。
func extractModel(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if jsonUnmarshal(body, &obj) != nil {
		return ""
	}
	return obj.Model
}
