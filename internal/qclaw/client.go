// client.go QClaw 本地 AuthGateway LLM 代理客户端，实现 provider.Upstream。
package qclaw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// Client QClaw LLM 代理客户端。
type Client struct {
	HTTP *http.Client
	Base string // 默认 DefaultBase；请求时 auth.ApiHost 优先
}

// New 生产默认。
func New() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 7200 * time.Second}, // 对齐 QClaw provider 72000s 配置的精神，放宽到 2h
		Base: DefaultBase,
	}
}

// clientUA 出站 UA。见 ChatStream 内注释。
const clientUA = "wild-work-qclaw/1.0"

// baseOf 解析生效的网关地址：auth 文件 apiHost 优先，其次 Client.Base。
func (c *Client) baseOf(a *auth.Auth) string {
	if a != nil && a.ApiHost != "" {
		return a.ApiHost
	}
	if c != nil && c.Base != "" {
		return c.Base
	}
	return DefaultBase
}

// farFutureExpires 本地网关无 token 过期概念；auth 文件未带 expiresAt 时
// 由加载器写入远期时间，避免每次请求触发刷新。
const farFutureExpires = 1 << 34 // 2106 年附近（int64 秒安全范围）

// RefreshToken 本地网关无需刷新（凭据在网关侧保管）。远期化过期时间，
// 防止 keepalive 调度反复触发。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a.ExpiresAt < time.Now().Add(24*time.Hour).Unix() {
		a.ExpiresAt = farFutureExpires
	}
	return nil
}

// ChatStream 发 chat 请求，返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、respBody 为上游响应体；仅传输层失败返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	prepared := PrepareBody(body)
	url := c.baseOf(a) + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// 本地 AuthGateway 拉黑 Go 默认 UA（Go-http-client/* 一律 invalid request，
	// 实测 0.2.37；其余任意 UA 放行），必须显式设置。
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("qclaw chat_stream: transport error: %v", err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("qclaw chat_stream: upstream %d body=%s", resp.StatusCode, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// FetchModels 调动态模型接口（OpenAI 列表形态，含 contextWindow/maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	url := c.baseOf(a) + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("models http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextWindow int64  `json:"contextWindow"`
			MaxTokens     int64  `json:"maxTokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]provider.ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, provider.ModelInfo{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens})
	}
	return out, nil
}

// FetchModelPricing QClaw 网关不提供定价接口；返回空。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// UserResource 本地网关无积分接口；返回 0。
func (c *Client) UserResource(a *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin QClaw 无签到活动（同 Qoder 渠道处理）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return fmt.Errorf("qclaw: no checkin activity")
}

// Classify 错误分类：本地网关无会话态，401/403 也视为网关侧配置问题而非
// 账号失效；429 软限流；5xx 上游故障；其余 4xx 客户端侧。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status >= 500:
		return provider.ErrServer
	default:
		return provider.ErrClient
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
