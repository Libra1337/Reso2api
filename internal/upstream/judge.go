// judge.go 内容防火墙外部审查（对齐 monoize moderation_judge）。
//
// 关键词命中只做提示，真正拦不拦由外部 LLM 判定：
//
//	porn / political → 拦截
//	benign / uncertain / 调用失败 / 未配置 → 放行（fail-open）
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	judgeTextBudget = 6000
	judgeReasonMax  = 500
	judgeMaxTokens  = 512
	judgeRetryMax   = 1

	JudgePorn      = "porn"
	JudgePolitical = "political"
	JudgeBenign    = "benign"
	JudgeUncertain = "uncertain"
)

const judgeSystemPrompt = `You are the content-safety judge of an LLM API gateway. Your job is to decide what the request text is trying to accomplish, not which words appear in it.

Take your time: reason about the text's intent, context, and phrasing before answering. You may write that analysis as plain text first. Then end your reply with EXACTLY ONE JSON object on its own last line and nothing after it:
{"category": "...", "reason": "..."}

Categories:
- "porn": the text seeks to produce, continue, or roleplay sexually explicit content. Anything sexualizing minors is always "porn".
- "political": the text seeks to produce politically illegal content such as subverting state power, inciting separatism, or extremist propaganda.
- "benign": everything else. Discussing, mentioning, reporting on, prohibiting, or studying sensitive topics (news, education, law, moderation policy, technical work) is "benign" even when it quotes prohibited words. Agent or tool system prompts, developer configuration, and defensive security policy text (security testing, CTF, refusing attacks) are "benign".
- "uncertain": you genuinely cannot decide. Use it freely — an undecided text must never be forced into "porn" or "political".

In "reason" state the concrete evidence: what the text asks for, and why that makes it blocking or not. One to three sentences, always written in Simplified Chinese (简体中文), regardless of the request text's language.`

var judgeCategories = []string{JudgePorn, JudgePolitical, JudgeBenign, JudgeUncertain}

// judgeTransport 进程级连接池。审查每次 new http.Client 但不换 Transport，
// 避免对 grok2api 反复 TCP/TLS 握手（线上冷启动曾卡到 11–18s 超时）。
var judgeTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          32,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

func judgeHTTP(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: judgeTransport}
}

// JudgeConfig 外部审查配置。Enabled 且三项均非空才视为可用。
type JudgeConfig struct {
	Enabled   bool
	BaseURL   string
	APIKey    string
	Model     string
	TimeoutMS int
}

func (c JudgeConfig) Active() bool {
	return c.Enabled &&
		strings.TrimSpace(c.BaseURL) != "" &&
		strings.TrimSpace(c.APIKey) != "" &&
		strings.TrimSpace(c.Model) != ""
}

func (c JudgeConfig) endpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/chat/completions"
}

func (c JudgeConfig) timeout() time.Duration {
	ms := c.TimeoutMS
	if ms <= 0 {
		ms = 4000
	}
	return time.Duration(ms) * time.Millisecond
}

func (c *Client) JudgeSnapshot() JudgeConfig {
	c.judgeMu.RLock()
	defer c.judgeMu.RUnlock()
	return c.Judge
}

func (c *Client) SetJudge(cfg JudgeConfig) {
	c.judgeMu.Lock()
	c.Judge = cfg
	c.judgeMu.Unlock()
}

// JudgeVerdict 外部审查结论。
type JudgeVerdict struct {
	Category string
	Reason   string
}

func (v JudgeVerdict) Blocks() bool {
	return v.Category == JudgePorn || v.Category == JudgePolitical
}

func (v JudgeVerdict) Keyword() string {
	switch v.Category {
	case JudgePorn:
		return "色情"
	case JudgePolitical:
		return "政治"
	default:
		return firewallFallbackKeyword
	}
}

func buildJudgeUserMessage(hits []string, texts []string) string {
	budget := judgeTextBudget
	var b strings.Builder
	if len(hits) > 0 {
		hint := "Keyword matcher flagged: " + strings.Join(hits, ", ") + "\n\n"
		n := utf8.RuneCountInString(hint)
		if n > budget {
			hint = string([]rune(hint)[:budget])
			n = budget
		}
		b.WriteString(hint)
		budget -= n
	}
	for i, text := range texts {
		if budget <= 0 {
			break
		}
		if i > 0 {
			sep := "\n---\n"
			sn := utf8.RuneCountInString(sep)
			if sn > budget {
				break
			}
			b.WriteString(sep)
			budget -= sn
		}
		runes := []rune(text)
		if len(runes) > budget {
			runes = runes[:budget]
		}
		b.WriteString(string(runes))
		budget -= len(runes)
	}
	return b.String()
}

func parseJudgeVerdict(content string) (JudgeVerdict, bool) {
	best, ok := JudgeVerdict{}, false
	for i := 0; i < len(content); i++ {
		if content[i] != '{' {
			continue
		}
		rest := content[i:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			continue
		}
		var obj struct {
			Category string `json:"category"`
			Reason   string `json:"reason"`
		}
		if json.Unmarshal([]byte(rest[:end+1]), &obj) != nil {
			continue
		}
		cat := ""
		for _, known := range judgeCategories {
			if obj.Category == known {
				cat = known
				break
			}
		}
		if cat == "" {
			continue
		}
		reason := obj.Reason
		if rs := []rune(reason); len(rs) > judgeReasonMax {
			reason = string(rs[:judgeReasonMax])
		}
		best = JudgeVerdict{Category: cat, Reason: reason}
		ok = true
	}
	return best, ok
}

func callJudge(cfg JudgeConfig, hits []string, texts []string) (JudgeVerdict, error) {
	if !cfg.Active() {
		return JudgeVerdict{}, fmt.Errorf("judge inactive")
	}
	body, err := json.Marshal(map[string]any{
		"model":       cfg.Model,
		"temperature": 0,
		"max_tokens":  judgeMaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": judgeSystemPrompt},
			{"role": "user", "content": buildJudgeUserMessage(hits, texts)},
		},
	})
	if err != nil {
		return JudgeVerdict{}, err
	}
	var last error
	for attempt := 0; attempt <= judgeRetryMax; attempt++ {
		v, err := doJudgeOnce(cfg, body)
		if err == nil {
			return v, nil
		}
		last = err
		if attempt == judgeRetryMax || !judgeRetryable(err) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return JudgeVerdict{}, last
}

func judgeRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// 超时再试会把 18s 预算翻倍，客户端更卡，直接 fail-open。
	if strings.Contains(msg, "Timeout") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "Client.Timeout") {
		return false
	}
	if strings.HasPrefix(msg, "judge HTTP 502") || strings.HasPrefix(msg, "judge HTTP 503") || strings.HasPrefix(msg, "judge HTTP 429") {
		return true
	}
	if strings.HasPrefix(msg, "judge request failed:") {
		return true
	}
	return false
}

func doJudgeOnce(cfg JudgeConfig, body []byte) (JudgeVerdict, error) {
	req, err := http.NewRequest(http.MethodPost, cfg.endpoint(), bytes.NewReader(body))
	if err != nil {
		return JudgeVerdict{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.APIKey))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := judgeHTTP(cfg.timeout()).Do(req)
	if err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return JudgeVerdict{}, fmt.Errorf("judge HTTP %d", resp.StatusCode)
	}
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Choices) == 0 {
		return JudgeVerdict{}, fmt.Errorf("judge response has no assistant content")
	}
	v, ok := parseJudgeVerdict(env.Choices[0].Message.Content)
	if !ok {
		return JudgeVerdict{}, fmt.Errorf("judge verdict not parseable")
	}
	return v, nil
}
