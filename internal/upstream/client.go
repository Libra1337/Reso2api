// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/prompt"
	"wild-work/internal/provider"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind = provider.ErrKind

const (
	ErrNone        = provider.ErrNone        // 成功
	ErrHardCredit  = provider.ErrHardCredit  // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate    = provider.ErrSoftRate    // 429 软限流 → 短冷却
	ErrSessionDead = provider.ErrSessionDead // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound    = provider.ErrNotFound    // 404 上游偶发 → 短冷却不累计 errCount（防雪崩）
	ErrServer      = provider.ErrServer      // 5xx 上游故障
	ErrClient      = provider.ErrClient      // 其他 4xx / 业务错误
)

// Error 带分类的上游错误。
type Error = provider.Error

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client
	// BillingHTTP 供账单/签到接口使用（短超时，慢网络下避免面板操作长时间假死）。
	// 为 nil 时回退到 HTTP。
	BillingHTTP *http.Client
	// StreamHTTP 聊天流专用客户端（Timeout=0，仅响应头/空闲超时）；
	// 为 nil 时回退 HTTP（注意 HTTP 的总超时会掐断长输出）。
	StreamHTTP *http.Client

	ChatBaseCN      string
	BillingBaseCN   string
	ChatBaseGlobal  string
	BillingBaseGlob string
	// WebBaseCN 成长中心 Web 域（任务领奖 / Web 端事件上报用，
	// 与 billing 域 codebuddy.cn 不同：领奖端点在 workbuddy.cn）。
	WebBaseCN string

	// SanitizeFingerprints 开启后出站消息内容做指纹脱敏（sanitize.go）。
	SanitizeFingerprints bool

	// ContentFirewall 开启后出站前做内容防火墙检查（firewall.go）：
	// 拦截会导致上游整号拉黑的高危内容（未成年+NSFW、越狱声明），保护账号池。
	ContentFirewall bool

	// PromptMode 系统提示词策略（internal/prompt）：
	//   - "custom"：出站前用 PromptText 替换客户端 system/developer（源头消灭 system 指纹误报）；
	//   - "passthrough"（默认）：透传客户端原始 system；被 11128 内容拦截时
	//     进入降级窗口（至次日 00:00 CST）换 Degraded 中性提示词并在同请求内重试一次。
	PromptMode string
	// PromptText custom 模式使用的提示词文本（空 = 内置默认）。
	PromptText string

	// ── 设备/客户端风控指纹（对齐官方桌面端，来自上游 1f78ad0/a65a36d/7b500f4）──
	// ClientName 用量归属名（如 "WorkBuddy"）：非空时 chat 注入
	// X-Agent-Purpose=conversation + X-IDE-Name/Type/Version + X-Product 四头组，
	// billing/growth 注入单段 WorkBuddy UA；空 = 保持旧 SaaS 兼容形态。
	ClientName string
	// ClientVersion/CliVersion UA 版本段（空 = 内置默认 5.5.4 / 2.137.1）。
	ClientVersion string
	CliVersion    string
	// DeviceToken X-Device-Token 全局兜底（auth 每号 > 此处 > DeviceTokenFile）。
	DeviceToken string
	// DeviceTokenFile 设备 token 文件兜底路径。
	DeviceTokenFile string

	// degradeMu/degradeUntil 11128 降级窗口截止（passthrough 模式）。
	degradeMu    sync.Mutex
	degradeUntil time.Time

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	effortsMu sync.RWMutex
	efforts   map[string][]string
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	out := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		out[k] = v
	}
	return out
}

// setEfforts 刷新 effort 能力缓存（FetchModels 调用）。
func (c *Client) setEfforts(m map[string][]string) {
	c.effortsMu.Lock()
	c.efforts = m
	c.effortsMu.Unlock()
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	// 短 RPC（刷新/签到/余额/模型）：总超时 120s
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	// 聊天流专用：无总超时（长输出不被掐断），仅约束响应头与空闲连接
	streamTr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       300 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:            &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:      &http.Client{Transport: streamTr},
		BillingHTTP:     &http.Client{Timeout: 30 * time.Second, Transport: tr},
		ChatBaseCN:      "https://copilot.tencent.com",
		BillingBaseCN:   "https://www.codebuddy.cn",
		WebBaseCN:       "https://www.workbuddy.cn",
		ChatBaseGlobal:  "https://www.workbuddy.ai",
		BillingBaseGlob: "https://www.workbuddy.ai",
	}
}

// streamClient 返回聊天流使用的 HTTP 客户端（无总超时，防止长输出被掐断）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.HTTP
}

// webBase 成长中心 Web 域（默认 www.workbuddy.cn）。
func (c *Client) webBase() string {
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// billingClient 返回账单接口用的 HTTP 客户端。
func (c *Client) billingClient() *http.Client {
	if c.BillingHTTP != nil {
		return c.BillingHTTP
	}
	return c.HTTP
}

func (c *Client) chatBase(region string) string {
	if region == "global" {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

func (c *Client) billingBase(region string) string {
	if region == "global" {
		return c.BillingBaseGlob
	}
	return c.BillingBaseCN
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	return c.doJSONWith(c.HTTP, req)
}

// doJSONBilling 用短超时账单客户端发请求。
func (c *Client) doJSONBilling(req *http.Request) (json.RawMessage, error) {
	return c.doJSONWith(c.billingClient(), req)
}

func (c *Client) doJSONWith(client *http.Client, req *http.Request) (json.RawMessage, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("workbuddy refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no refreshToken")
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	url := c.chatBase(auth.RegionOf(a.Domain)) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		err := fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	log.Printf("workbuddy refresh success uid=%s refresh_rotated=%t expires_at=%d", a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

// degradeActive 11128 降级窗口是否生效。
func (c *Client) degradeActive() bool {
	c.degradeMu.Lock()
	defer c.degradeMu.Unlock()
	return time.Now().Before(c.degradeUntil)
}

// degradeTrigger 触发降级窗口至次日 00:00（CST）。
func (c *Client) degradeTrigger() {
	next := time.Now().In(time.FixedZone("CST", 8*3600)).Add(24 * time.Hour)
	next = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, next.Location())
	c.degradeMu.Lock()
	c.degradeUntil = next
	c.degradeMu.Unlock()
}

// isFingerprintBlock 11128：指纹误报拦截（system 模板句参与判定），
// 换中性 system 重试有效，且值得进入降级窗口。
func isFingerprintBlock(body []byte) bool {
	return strings.Contains(string(body), "11128")
}

// isSafetyBlock 11140：内容安全审核（"request illegal"）——用户/对话内容
// 本身触发，换 system 无效（实测：降级提示词下仍被拦）。不重试、不触发
// 降级窗口，直接透传让客户端自知；降级窗口若因此触发只会污染无关流量。
func isSafetyBlock(body []byte) bool {
	return strings.Contains(string(body), "11140")
}

// applyPrompt 按模式做出站前系统提示词改写（返回改写后的 body 与是否已降级）。
func (c *Client) applyPrompt(body []byte) ([]byte, bool) {
	switch c.PromptMode {
	case "custom":
		return prompt.Rewrite(body, c.PromptText), false
	default: // passthrough
		if c.degradeActive() {
			return prompt.Rewrite(body, prompt.Degraded), true
		}
		return body, false
	}
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))、err 为 nil；
// 只有传输层失败才返回 err。
// 11128 指纹误报自愈：passthrough 模式首遇触发降级窗口并换中性提示词
// 同请求重试一次；custom 模式 system 已被替换，重试中性兜底。
// 11140 内容审核不在此列（换 system 无效，见 isSafetyBlock）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	prepared, degraded := c.applyPrompt(PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot()))
	// 内容防火墙：高危内容不出网关（上游拉黑是整号永久的，代价不可逆）。
	if c.ContentFirewall {
		if rule, hit := FirewallCheck(prepared); hit {
			log.Printf("FIREWALL uid=%s rule=%s -> blocked (content not sent upstream)", a.UID, rule)
			return nil, http.StatusForbidden, FirewallHitResponse(rule), nil
		}
	}
	rc, status, respBody, err = c.chatOnce(a, prepared)
	if err != nil || status < 400 {
		return rc, status, respBody, err
	}
	if isSafetyBlock(respBody) {
		// 内容审核：透传（客户端可见 displayMsg），不罚号不重试不降级
		log.Printf("chat_stream uid=%s: safety-blocked (11140, content review) -> passthrough", a.UID)
		return rc, status, respBody, err
	}
	if (status == http.StatusBadRequest || status == http.StatusForbidden) && isFingerprintBlock(respBody) && !degraded {
		log.Printf("chat_stream uid=%s: content-blocked (11128, status %d) -> degraded prompt retry", a.UID, status)
		c.degradeTrigger()
		return c.chatOnce(a, prompt.Rewrite(prepared, prompt.Degraded))
	}
	return rc, status, respBody, err
}

// chatOnce 单次上游 chat 请求（无重试语义）。
func (c *Client) chatOnce(a *auth.Auth, prepared []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// 调试开关：WB2A_DUMP_REQ 非空时把发往上游的最终请求体落盘（含净化/注入后的
	// 形态），供离线二分定位指纹命中行。不设置时零开销。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(prepared) >= 4096 {
		_ = os.MkdirAll("data", 0o755)
		if err := os.WriteFile("data/last_request.json", prepared, 0o600); err != nil {
			log.Printf("dump req: %v", err)
		}
	}
	snap := a.Snapshot()
	url := c.chatBase(snap.Region) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a)
	resp, err := c.streamClient().Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		// 诊断：安全策略命中时记录脱敏后仍被拦的载荷片段（定位未知新指纹/审核触发内容）
		if isFingerprintBlock(raw) || isSafetyBlock(raw) {
			log.Printf("content-block payload snippet uid=%s: %.8000s", a.UID, prepared)
		}
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo = provider.ModelInfo

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	snap := a.Snapshot()
	url := c.chatBase(snap.Region) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+snap.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(snap.Region)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.chatUA())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID               string
		Name             string
		MaxInputTokens   int64
		MaxOutputTokens  int64
		Disabled         bool
		SupportedEfforts []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID               string
			Name             string
			MaxInputTokens   int64
			MaxOutputTokens  int64
			Disabled         bool
			SupportedEfforts []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	efforts := make(map[string][]string, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
		})
		if len(m.SupportedEfforts) > 0 {
			efforts[m.ID] = m.SupportedEfforts
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	c.setEfforts(efforts)
	return out, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	url := c.billingBase(a.Region()) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	c.BillingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// UserResourceDetail 查询账号积分明细（所有套餐条目）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	url := c.billingBase(a.Region()) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	c.BillingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		return 0, nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CreateTime          int64  `json:"CreateTime"`   // 毫秒
					CycleEndTime        string `json:"CycleEndTime"` // 周期到期文案
					ExpiredTime         string `json:"ExpiredTime"`  // 资源到期文案
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, nil, fmt.Errorf("resource parse: %w", err)
	}
	var total int64
	items := make([]provider.ResourceItem, 0, len(resp.Response.Data.Accounts))
	for _, acct := range resp.Response.Data.Accounts {
		var total_, used, remain int64
		switch {
		case acct.CycleCapacitySize > 0:
			total_, used, remain = acct.CycleCapacitySize, acct.CycleCapacityUsed, acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			total_, used, remain = acct.CycleCapacityRemain+acct.CycleCapacityUsed, acct.CycleCapacityUsed, acct.CycleCapacityRemain
		default:
			total_, used, remain = acct.CapacitySize, acct.CapacityUsed, acct.CapacityRemain
		}
		if remain < 0 {
			remain = 0
		}
		total += remain
		gotAt := acct.CreateTime / 1000
		expire := acct.CycleEndTime
		if expire == "" {
			expire = acct.ExpiredTime
		}
		items = append(items, provider.ResourceItem{
			Name:     acct.PackageName,
			Total:    total_,
			Used:     used,
			Remain:   remain,
			GotAt:    gotAt,
			ExpireAt: expire,
		})
	}
	// 新获得的排最前：猫猫/签到收益以同名"裂变包"入账，最新的在最上面才好辨认
	sort.Slice(items, func(i, j int) bool {
		if items[i].GotAt != items[j].GotAt {
			return items[i].GotAt > items[j].GotAt
		}
		return items[i].Remain > items[j].Remain
	})
	return total, items, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	log.Printf("workbuddy checkin start uid=%s", a.UID)
	url := c.billingBase(a.Region()) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	_, err = c.doJSONBilling(req)
	if err != nil {
		log.Printf("workbuddy checkin failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("workbuddy checkin success uid=%s", a.UID)
	return nil
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（WorkBuddy 上游已是 OpenAI SSE，直接透传）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error { return Stream(w, r) }

// Aggregate 实现 provider.Upstream（WorkBuddy OpenAI SSE 聚合）。
func (c *Client) Aggregate(r io.Reader) (map[string]any, error) { return Aggregate(r) }

// FetchModelPricing 从 /console/enterprises/personal/models 拉取模型积分倍率。
// 返回全量模型定价（含 credits 字段），不受 cli agent 过滤限制。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	snap := a.Snapshot()
	url := c.chatBase(snap.Region) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+snap.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(snap.Region)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.chatUA())
	req.Header.Set("X-User-Id", a.UID)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID      string   `json:"id"`
				Name    string   `json:"name"`
				Credits string   `json:"credits"` // "x0.79 credits"
				Tags    []string `json:"tags"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("pricing parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("pricing api code=%d", env.Code)
	}
	out := make([]provider.ModelPricing, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		rate := parseCredits(m.Credits)
		if rate <= 0 && m.ID != "auto" {
			continue // hunyuan-chat 等非计费模型
		}
		note := ""
		for _, tag := range m.Tags {
			if strings.HasPrefix(tag, "badge:") {
				note = strings.TrimPrefix(tag, "badge:")
			}
		}
		out = append(out, provider.ModelPricing{
			Model:   m.ID,
			Channel: "workbuddy",
			Rate:    rate,
			Note:    note,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pricing api returned empty models")
	}
	return out, nil
}

// parseCredits 解析 "x0.08 credits" → 0.08。
func parseCredits(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 去掉 "x" 前缀和 " credits" 后缀
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, " credits")
	s = strings.TrimSpace(s)
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
