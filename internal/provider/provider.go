// Package provider 定义不同上游（workbuddy / traework）共用的最小接口。
// 只抽取 server/scheduler 必需能力，避免为未来平台过度设计。
package provider

import (
	"fmt"
	"io"
	"net/http"

	"wild-work/internal/auth"
)

// Kind 平台标识，同时也是模型名前缀。
type Kind string

const (
	WorkBuddy Kind = "workbuddy"
	TraeWork  Kind = "traework"
	Qoder     Kind = "qoder"
	QClaw     Kind = "qclaw"
)

func (k Kind) String() string { return string(k) }

// ErrKind 错误分类，驱动 pool 冷却状态机。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额/权益不足 → 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 登录态失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却不累计 errCount
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// ModelInfo 动态/静态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// ModelPricing 模型积分定价（从上游 API 拉取）。
type ModelPricing struct {
	Model   string  `json:"model"`
	Channel string  `json:"channel"`
	Rate    float64 `json:"rate"`
	Note    string  `json:"note,omitempty"` // 折扣/标签说明
}

// Upstream 是 server/scheduler 依赖的最小上游能力集合。
type Upstream interface {
	RefreshToken(a *auth.Auth) error
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	FetchModels(a *auth.Auth) ([]ModelInfo, error)
	FetchModelPricing(a *auth.Auth) ([]ModelPricing, error)
	UserResource(a *auth.Auth) (int64, error)
	UserResourceDetail(a *auth.Auth) (int64, []ResourceItem, error)
	DailyCheckin(a *auth.Auth) error
	Classify(status int, body string) ErrKind
	Stream(w http.ResponseWriter, r io.Reader) error
	Aggregate(r io.Reader) (map[string]any, error)
}

// ConversationalUpstream 由支持会话感知出站的渠道可选实现：
// conversationID 参与 prompt_cache_key 注入（同账号同会话命中上游前缀缓存，
// 费用显著降低）。未实现该接口的渠道走 ChatStream（无缓存键）。
type ConversationalUpstream interface {
	ChatStreamConv(a *auth.Auth, body []byte, conversationID string) (rc io.ReadCloser, status int, respBody []byte, err error)
}

// ChatStreamConv 会话感知出站的入口适配：渠道实现 ConversationalUpstream
// 时走会话版本，否则回落普通 ChatStream。
func ChatStreamConv(u Upstream, a *auth.Auth, body []byte, conversationID string) (io.ReadCloser, int, []byte, error) {
	if cu, ok := u.(ConversationalUpstream); ok {
		return cu.ChatStreamConv(a, body, conversationID)
	}
	return u.ChatStream(a, body)
}

// ResourceItem 积分明细条目。
type ResourceItem struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`
	// GotAt 获得时间（Unix 秒；条目创建时刻）。猫猫领养/每日签到等收益
	// 都以"裂变包"条目入账且同名，靠时间才能辨认哪条是最新收益。
	GotAt int64 `json:"got_at"`
	// ExpireAt 到期时间（上游文案原样，如 "2026-10-13 06:29:23"）。
	ExpireAt string `json:"expire_at"`
}
