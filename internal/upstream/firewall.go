// firewall.go 出站内容防火墙：目标只有两个——防止账号被封禁拉黑，
// 以及对灰色内容留观测痕迹。按内容类别分两种动作：
//
//	block   拦截（403，请求不出网关）：色情类（实证烧号：未成年成年化/
//	        NSFW 合法化声明曾致两号整号拉黑）与政治暴恐红线。
//	observe 仅标记（请求照常转发上游）：科技/危害类（武器/毒品/恶意
//	        软件/深伪/自杀）与越狱声明——无实证致封证据，标记进事件流
//	        供观测；真有风险时上游 11140 逐请求反馈 + 熔断兜底。
//
// 命中均记录事件（时间/规则/账号/全文），面板「防火墙」页可见。
// 配置 features.content_firewall=false 可整体关闭（默认开启）。
package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"wild-work/internal/auth"
)

// firewallRule 单条规则；Observe=true 时命中只记录事件不拦截。
type firewallRule struct {
	Name    string
	Pattern *regexp.Regexp
	// Pair 非空时为主题词×意图动词双信号共现（降低误报）
	Pair    *regexp.Regexp
	Observe bool
}

// 防火墙动作。
const (
	ActionBlock   = "block"   // 拦截：403，请求不出网关
	ActionObserve = "observe" // 标记：放行，记录事件
)

var firewallRules = []firewallRule{
	// -- 色情红线（block）：实证烧号 + 未成年保护 --
	{Name: "csam", Pattern: regexp.MustCompile(
		`(?i)(儿童色情|儿童性|幼女(交|裸|色)?|未成年(性|色情|裸)|loli(交|色|con)?|child\s*(porn|sexual|abuse)|csam|萝莉(色|裸))`)},
	{Name: "minor-adult-claim", Pattern: regexp.MustCompile(
		`(?i)(1[0-9]\s*岁|一[零一二三四五六七八九]四岁|未成年|loli|萝莉)[^
]{0,60}(就是|已是|算是|视为|都是|均为)\s*(成年|大人)`)},
	{Name: "nsfw-legalize", Pattern: regexp.MustCompile(
		`(?i)(nsfw|色情|淫秽|涩图|r18|性描写)[^
]{0,80}(合法|合规|不受限|没有限制)|(合法|合规|不受任何限制)[^
]{0,80}(nsfw|色情|淫秽|性描写)`)},
	{Name: "explicit-allow", Pattern: regexp.MustCompile(
		`(?i)允许出现?(暴力|色情|性|nsfw)`)},
	// -- 兜底：具体未成年词 x NSFW"允许/生成"语境（拒绝型人设不命中）--
	{Name: "minor-nsfw-cooccur", Pattern: regexp.MustCompile(
		`(?i)(1[0-9]\s*岁|十五岁|十六岁|十七岁|loli|萝莉|幼女|女小学生)`)},
	// -- 政治暴恐红线（block）--
	{Name: "terror", Pattern: regexp.MustCompile(
		`(?i)(圣战宣言|isil|isis(的)?(宣传|宣言|招募)|暴恐袭击宣言|制作(炸弹背心|自杀式)|加入(圣战|isil|isis))`)},
	// -- 科技/危害类（observe：仅标记，放行）--
	{Name: "weapon-cbrn", Observe: true,
		Pattern: regexp.MustCompile(`(?i)(炸弹|爆炸装置|炸药|土制炸弹|核武器|脏弹|生化武器|神经毒剂|沙林毒气|vx毒剂|蓖麻毒素|c4炸药|\bied\b|\bbomb\b|explosive device)`),
		Pair:    regexp.MustCompile(`(?i)(制造|制作|合成|配方|原料|怎么造|如何制造|how to (make|build)|make a bomb|build.*explosive)`)},
	{Name: "drug-synthesis", Observe: true,
		Pattern: regexp.MustCompile(`(?i)(冰毒|甲基苯丙胺|海洛因|摇头丸|\bmeth(amphetamine)?\b|heroin|合成大麻|k粉|氯胺酮(粉)?)`),
		Pair:    regexp.MustCompile(`(?i)(合成|制作|提纯|配方|工艺|制毒|how to (make|synthesize)|synthesize)`)},
	{Name: "malware", Observe: true,
		Pattern: regexp.MustCompile(`(?i)(勒索软件|ransomware|僵尸网络|botnet|键盘记录器|keylogger|挖矿木马|蠕虫病毒(攻击)|ddos(攻击工具)?)`),
		Pair:    regexp.MustCompile(`(?i)(编写|开发|制作|源码|免杀|植入|用来攻击|攻击(他人|公司|别人)|\bwrite\b|\bcreate\b|\bdevelop\b|how to (write|create|deploy))`)},
	{Name: "nude-deepfake", Observe: true,
		Pattern: regexp.MustCompile(`(?i)(裸照|裸体|nude\s*(photo|pic|image|video)|deepfake|换脸(色情|裸)|ai去衣|脱衣(ai|软件))`),
		Pair:    regexp.MustCompile(`(?i)(生成|合成|制作|伪造|p(出来|成|图)|make|generate|create)`)},
	{Name: "self-harm", Observe: true, Pattern: regexp.MustCompile(
		`(?i)(自杀方法|自杀教程|怎么自杀|如何自杀|无痛自杀|自残方法|how to (kill myself|commit suicide|self.?harm)|suicide method)`)},
	// -- 越狱声明（observe：仅标记，放行）--
	{Name: "bypass-review", Observe: true, Pattern: regexp.MustCompile(
		`(?i)(无视|忽略|绕过|不要遵守)[^
]{0,40}(安全|审核|内容审查|过滤|限制)`)},
	{Name: "bypass-review-en", Observe: true, Pattern: regexp.MustCompile(
		`(?i)\b(ignore|disregard|bypass)\s+(all\s+|any\s+|the\s+|your\s+|their\s+|its\s+)*(previous\s+|prior\s+|above\s+|earlier\s+)*(user\s+|system\s+|safety\s+|content\s+|security\s+)*(instructions?|prompts?|rules|guardrails|guidelines|policies?|restrictions?|filters?|safety)`)},
	{Name: "no-safety-claim", Observe: true, Pattern: regexp.MustCompile(
		`(?i)(没有|不带|do not have|don't have|without|any)\s{0,3}(任何)?\s{0,3}(安全(?:限制|准则|指南|约束)?|safety|guideline|restriction)`)},
	// -- 政治敏感（observe：仅标记，红线暴恐已在上）--
	{Name: "politics", Observe: true,
		Pattern: regexp.MustCompile(`(?i)(习近平|毛泽东|邓小平|共产党|国务院|政治局|中央政府|国家领导人)`),
		Pair:    regexp.MustCompile(`(?i)(推翻|打倒|颠覆|暗杀|去死|独裁|腐败|舞弊|邪恶)`)},
}

var nsfwSignals = regexp.MustCompile(`(?i)(nsfw|r18|色情|淫秽|涩图|性描写|露骨)`)

// nsfwAllowContext NSFW 的"允许/生成"意图语境（cooccur 兜底要求命中）：
// 动词主动请求/生产 NSFW 内容才算。
var nsfwAllowContext = regexp.MustCompile(
	`(?i)(可以|允许|要|画|生成|制作|来点|想要|发[一两张个]?|写)[^\n]{0,12}(nsfw|r18|色情|涩图|性描写)`)

// nsfwDenyContext NSFW 的"拒绝/防护"语境（命中则不拦）：拒绝型人设与
// 安全防护讨论（检测/过滤/审核 NSFW）都属安全语境。
var nsfwDenyContext = regexp.MustCompile(
	`(?i)((不发|不会发|拒绝|禁止|不允许|不让发|检测|过滤|识别|审核|防护|保护|拦截)[^\n]{0,14}(nsfw|r18|色情|涩图|性描写))|(((nsfw|r18|色情|涩图|性描写)[^\n]{0,10}(绝对)?不发))`)

// FirewallCheck 检查出站请求体文本，命中返回规则名与命中片段
// （动词与政策词前后各带 40 字上下文，供面板展示"为什么命中"）。
// 只扫描 messages 的文本内容（system/user/assistant），不碰工具定义。
func FirewallCheck(prepared []byte) (rule, excerpt, action string) {
	if len(prepared) == 0 {
		return "", "", ""
	}
	text := extractMessageText(prepared)
	if text == "" {
		return "", "", ""
	}
	// block 规则优先评估；observe 规则记录首个命中（block 优先级更高）
	observeHit := func() (string, string, string) {
		for _, r := range firewallRules {
			if !r.Observe {
				continue
			}
			if r.Pair != nil {
				loc := r.Pattern.FindStringIndex(text)
				ploc := r.Pair.FindStringIndex(text)
				if loc != nil && ploc != nil {
					lo, hi := loc[0], ploc[1]
					if ploc[0] < lo {
						lo, hi = ploc[0], loc[1]
					}
					return r.Name, excerptAt(text, lo, hi), ActionObserve
				}
				continue
			}
			if l := r.Pattern.FindStringIndex(text); l != nil {
				return r.Name, excerptAt(text, l[0], l[1]), ActionObserve
			}
		}
		return "", "", ""
	}
	for _, r := range firewallRules {
		if r.Observe {
			continue
		}
		switch {
		case r.Name == "minor-nsfw-cooccur":
			// 共现兜底：未成年信号 + NSFW"允许/生成"语境同时出现才拦。
			// 拒绝型人设（"NSFW 的图我绝对不发"）与安全讨论不命中。
			if r.Pattern.MatchString(text) && nsfwSignals.MatchString(text) && nsfwAllowContext.MatchString(text) && !nsfwDenyContext.MatchString(text) {
				return r.Name, excerptAround(text, r.Pattern), ActionBlock
			}
		default:
			if loc := r.Pattern.FindStringIndex(text); loc != nil {
				return r.Name, excerptAt(text, loc[0], loc[1]), ActionBlock
			}
		}
	}
	if term, loc := findLynshenKeyword(text); term != "" {
		return "hint:" + term, excerptAt(text, loc[0], loc[1]), ActionBlock
	}
	return observeHit()
}

// lynshenHintDrop 日常/商务/乐理词：子串命中会把正常请求打进审查，打爆 grok2api 额度。
var lynshenHintDrop = map[string]struct{}{
	"限量": {},
	"刺激": {},
	"和弦": {},
	"铃声": {},
	"兼职": {},
	"媚外": {},
}

// findLynshenKeyword 对齐 lynshen.org 词表。中文仍做无边界子串；纯 ASCII 词要求词边界，
// 避免 sm 命中 wasm/small、anal 命中 analysis。命中后由外部审查判定。
func findLynshenKeyword(text string) (term string, loc []int) {
	if text == "" || len(lynshenKeywordHints) == 0 {
		return "", nil
	}
	hay := strings.ToLower(text)
	bestStart, bestEnd := -1, -1
	bestTerm := ""
	for _, t := range lynshenKeywordHints {
		if t == "" {
			continue
		}
		if _, drop := lynshenHintDrop[t]; drop {
			continue
		}
		i, end := hintMatchSpan(hay, t)
		if i < 0 {
			continue
		}
		if bestStart < 0 || i < bestStart || (i == bestStart && end > bestEnd) {
			bestStart, bestEnd, bestTerm = i, end, t
		}
	}
	if bestStart < 0 {
		return "", nil
	}
	return bestTerm, []int{bestStart, bestEnd}
}

func hintMatchSpan(hay, term string) (start, end int) {
	if isASCIIHint(term) {
		from := 0
		for {
			i := strings.Index(hay[from:], term)
			if i < 0 {
				return -1, -1
			}
			i += from
			end := i + len(term)
			if asciiWordBoundary(hay, i, end) {
				return i, end
			}
			from = i + 1
		}
	}
	i := strings.Index(hay, term)
	if i < 0 {
		return -1, -1
	}
	return i, i + len(term)
}

func isASCIIHint(term string) bool {
	for i := 0; i < len(term); i++ {
		if term[i] > 127 {
			return false
		}
	}
	return true
}

func asciiWordBoundary(hay string, start, end int) bool {
	leftOK := start == 0 || !asciiWordChar(hay[start-1])
	rightOK := end == len(hay) || !asciiWordChar(hay[end])
	return leftOK && rightOK
}

func asciiWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// excerptAround 取规则首个命中点前后各 40 字。
func excerptAround(text string, re *regexp.Regexp) string {
	loc := re.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	return excerptAt(text, loc[0], loc[1])
}

// excerptAt 取 [lo,hi) 前后各 40 字、压平换行。
func excerptAt(text string, lo, hi int) string {
	s := lo - 40
	if s < 0 {
		s = 0
	}
	e := hi + 40
	if e > len(text) {
		e = len(text)
	}
	out := strings.ReplaceAll(text[s:e], "\n", " ")
	return strings.TrimSpace(out)
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

// firewallClientMsg 返回给调用方 / 中转站的固定文案。
// [关键词] 填分类词（色情 / nsfw / 暴恐 等），绝不填内部规则名、业务 code、账号。
const firewallClientMsg = "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。"

const firewallFallbackKeyword = "违禁词"

// firewallRuleKeywords 把内部规则名映射成可展示的分类词。
var firewallRuleKeywords = map[string]string{
	"csam":               "色情",
	"minor-adult-claim":  "色情",
	"minor-nsfw-cooccur": "色情",
	"nsfw-legalize":      "nsfw",
	"explicit-allow":     "nsfw",
	"nude-deepfake":      "nsfw",
	"terror":             "暴恐",
	"weapon-cbrn":        "暴力",
	"drug-synthesis":     "毒品",
	"malware":            "恶意软件",
	"self-harm":          "自杀",
	"politics":           "政治",
	"bypass-review":      "违禁词",
	"bypass-review-en":   "违禁词",
	"no-safety-claim":    "违禁词",
}

// contentBlockKeywords 从上游审核文案抽出分类词（大小写不敏感，按优先级）。
var contentBlockKeywords = []string{
	"色情", "porn", "nsfw", "adult",
	"暴恐", "terror",
	"暴力", "violence",
	"政治", "politics",
	"赌博", "gambling",
	"毒品", "drug",
	"违禁词",
}

// FirewallKeyword 把内部规则名或已是展示词的标签收成客户端关键词。
// 未知内部名（csam / xxx-yyy）一律回「违禁词」，不得出站。
func FirewallKeyword(rule string) string {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return firewallFallbackKeyword
	}
	if kw, ok := firewallRuleKeywords[rule]; ok {
		return kw
	}
	if strings.HasPrefix(rule, "hint:") {
		term := strings.TrimPrefix(rule, "hint:")
		if term != "" {
			return term
		}
	}
	for _, kw := range contentBlockKeywords {
		if strings.EqualFold(rule, kw) {
			return kw
		}
	}
	return firewallFallbackKeyword
}

// ContentBlockKeyword 从上游 11128/11140 等审核 body 抽出分类词；抽不到则「违禁词」。
// 不返回业务 code、账号、冷却语义。
func ContentBlockKeyword(body string) string {
	text := body
	var env struct {
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) == nil {
		switch {
		case strings.TrimSpace(env.Error.Message) != "":
			text = env.Error.Message
		case strings.TrimSpace(env.Msg) != "":
			text = env.Msg
		case strings.TrimSpace(env.Message) != "":
			text = env.Message
		}
	}
	lower := strings.ToLower(text)
	for _, kw := range contentBlockKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return kw
		}
	}
	return firewallFallbackKeyword
}

// IsContentPolicyBlock 上游内容审核拦截（11140 安全审核 / 11128 指纹策略）。
func IsContentPolicyBlock(body string) bool {
	return strings.Contains(body, "11140") || strings.Contains(body, "11128")
}

// FirewallHitResponse 防火墙拦截时返回给客户端的错误体（403）。
// 形状对齐 OpenAI 内容违规错误（type=invalid_request_error /
// code=content_policy_violation）：中转站对该标准形状原样透传 message，
// 不会改写成"无可用渠道/模型不存在"或账号冷却之类的笼统报错。
func FirewallHitResponse(rule string) []byte {
	kw := FirewallKeyword(rule)
	return []byte(`{"error":{"message":"` + fmt.Sprintf(firewallClientMsg, kw) + `","type":"invalid_request_error","param":null,"code":"content_policy_violation"}}`)
}

// FirewallEvent 一次防火墙拦截记录（内存环形，面板展示用）。
type FirewallEvent struct {
	At      int64  `json:"at"` // Unix 秒
	Rule    string `json:"rule"`
	UID     string `json:"uid,omitempty"`
	Nick    string `json:"nick,omitempty"`
	Model   string `json:"model,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Content string `json:"content,omitempty"`
	Match   string `json:"match,omitempty"`
	Observe bool   `json:"observe,omitempty"`
	Keyword string `json:"keyword,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Entry   string `json:"entry,omitempty"`
	Judge   string `json:"judge,omitempty"`
}

// FirewallHitMeta 外部审查附加信息。
type FirewallHitMeta struct {
	Keyword string
	Verdict string
	Reason  string
	Entry   string
	Judge   string
}

const firewallEventCap = 500

func (c *Client) recordFirewallHit(a *auth.Auth, rule, model, matchExcerpt string, prepared []byte, observe bool) {
	c.recordFirewallHitMeta(a, rule, model, matchExcerpt, prepared, observe, FirewallHitMeta{Entry: "chat"})
}

func (c *Client) recordFirewallHitMeta(a *auth.Auth, rule, model, matchExcerpt string, prepared []byte, observe bool, meta FirewallHitMeta) {
	content := extractMessageText(prepared)
	snippet := strings.ReplaceAll(content, "\n", " ")
	if r := []rune(snippet); len(r) > 80 {
		snippet = string(r[:80]) + "…"
	}
	uid, nick := "", ""
	if a != nil {
		uid = a.UID
		nick = a.Nickname
	}
	if meta.Entry == "" {
		meta.Entry = "chat"
	}
	ev := FirewallEvent{
		At: time.Now().Unix(), Rule: rule, UID: uid, Nick: nick, Model: model,
		Snippet: snippet, Content: content, Match: matchExcerpt, Observe: observe,
		Keyword: meta.Keyword, Verdict: meta.Verdict, Reason: meta.Reason,
		Entry: meta.Entry, Judge: meta.Judge,
	}
	c.fwMu.Lock()
	c.fwHits = append(c.fwHits, ev)
	if len(c.fwHits) > firewallEventCap {
		c.fwHits = c.fwHits[len(c.fwHits)-firewallEventCap:]
	}
	c.fwMu.Unlock()
	appendFirewallLog(ev)
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
