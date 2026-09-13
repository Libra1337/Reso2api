// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6；风控指纹对齐官方桌面端
// （上游 workbuddy2api 1f78ad0/a65a36d/7b500f4：UA 三段式 + 用量归属四头
// + X-Device-Token 设备风控头）。
package upstream

import (
	"net/http"
	"os"
	"strings"

	"wild-work/internal/auth"
)

const (
	// defaultClientVersion UA 的 WorkBuddy 版本段与 X-IDE-Version：
	// 对齐官方 WorkBuddy Desktop 分发包版本（config upstream.client_version 可覆盖）。
	defaultClientVersion = "5.5.4"
	// defaultCliVersion UA 的 CLI 段版本：对齐官方内置 CLI 2.137.1
	//（config upstream.cli_version 可覆盖）。
	defaultCliVersion = "2.137.1"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

func originRefererFor(region string) string {
	if region == "global" {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本段。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本段。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// chatUA 组装 chat 出站 UA（官方桌面端三段式）：
// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`。官方无 UA 随机化，默认确定性。
func (c *Client) chatUA() string {
	return "WorkBuddy/" + c.clientVersion() + " WorkBuddy/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// billingUA billing/growth 域 UA：单段 `WorkBuddy/<ver>`（官方 banner 层固化形态）。
// ClientName 未配置时返回空（不设 UA，保持 Go 默认，避免形态突变）。
func (c *Client) billingUA() string {
	if c == nil || c.ClientName == "" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// CommonHeaders 设置所有 API 共享的请求头（字段取自快照，并发安全）。
func CommonHeaders(req *http.Request, s auth.HeaderSnapshot) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(s.Region)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
}

// setChatUA 包一层便于 CommonHeaders 使用者拿到 Client 语境的 UA。
func (c *Client) commonHeaders(req *http.Request, s auth.HeaderSnapshot) {
	CommonHeaders(req, s)
	req.Header.Set("User-Agent", c.chatUA())
}

// injectAttribution 注入用量归属头（仅 chat 路径，ChatHeaders 调用）。
// ClientName 非空（如 "WorkBuddy"）时四头齐全 + X-Agent-Purpose：
// X-IDE-Name/Type/Product = ClientName，X-IDE-Version = 客户端版本段；
// 空 = 保持 X-Product="SaaS" 旧形态（向后兼容，不突变归因）。
func (c *Client) injectAttribution(req *http.Request) {
	if c == nil || c.ClientName == "" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", c.ClientName)
	req.Header.Set("X-IDE-Type", c.ClientName)
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Product", c.ClientName)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（全局）> 文件兜底。
// 皆空/读失败返回空串（调用方不注入，优雅降级——容器内无桌面端 SDK）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// readDeviceTokenFile 读文件形态的设备 token（trim 首尾空白；失败返回空）。
func readDeviceTokenFile(fp string) string {
	raw, err := os.ReadFile(fp)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// injectDeviceToken 仅在取到非空 token 时注入。refresh/models 类请求不注入
// （鉴权头组带设备 token 无意义且可能被风控误判）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与官方 CLI 一致）。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth) {
	s := a.Snapshot()
	c.commonHeaders(req, s)
	if s.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	c.injectAttribution(req)
	c.injectDeviceToken(req, a)
}

// BillingHeaders billing/growth 接口请求头（字段取自快照，并发安全）。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	s := a.Snapshot()
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
		req.Header.Set("X-Tenant-Id", s.EnterpriseID)
	}
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	}
	if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
// 契约：唯一调用方 RefreshToken 已持 a 写锁，此处直读字段；
// 不可改为 Snapshot（其内部读锁会与外层写锁自死锁，RWMutex 不可重入）。
func RefreshHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, auth.HeaderSnapshot{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Domain:       a.Domain,
		Region:       auth.RegionOf(a.Domain),
	})
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	req.Header.Set("User-Agent", "WorkBuddy/"+defaultClientVersion+" WorkBuddy/"+defaultClientVersion+" CLI/"+defaultCliVersion)
}
