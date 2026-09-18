// proxy.go 出站代理池：多出口分流 + 面板热更新。
//
// 设计：
//   - 代理列表存 data/proxies.json（{"proxies":[{"addr","user","pass","disabled"}]}），
//     面板保存后热重载，改代理不用重启进程；
//   - 分流：账号 UID 做 FNV-1a 哈希 → % 池大小 → 固定落到一个代理。同一账号
//     始终同一出口 IP（上游风控视角 IP 稳定），池更新时分布重排但映射仍确定；
//   - 池空 / 账号命中的代理 disabled → 直连（保留裸机出口兜底）；
//   - 认证：HTTP 代理用 Proxy-Authorization（Basic）；Webshare 为 HTTP 代理。
package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProxyEntry 单个代理出口。
type ProxyEntry struct {
	Addr     string `json:"addr"` // host:port
	User     string `json:"user,omitempty"`
	Pass     string `json:"pass,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Note     string `json:"note,omitempty"`
}

// proxyState 代理池状态（热重载）。
type proxyState struct {
	mu      sync.RWMutex
	entries []ProxyEntry // enabled-only 视图由 accessor 现算
	path    string
}

var proxyPool proxyState

// LoadProxies 从 data/proxies.json 加载代理池；文件不存在视为空池（直连）。
func LoadProxies(dataDir string) {
	proxyPool.mu.Lock()
	defer proxyPool.mu.Unlock()
	proxyPool.path = filepath.Join(dataDir, "proxies.json")
	raw, err := os.ReadFile(proxyPool.path)
	if err != nil {
		proxyPool.entries = nil
		return
	}
	var obj struct {
		Proxies []ProxyEntry `json:"proxies"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		log.Printf("[proxy] proxies.json 解析失败，忽略（保持原池）")
		return
	}
	proxyPool.entries = obj.Proxies
	log.Printf("[proxy] 代理池加载 %d 个出口", len(enabledProxiesLocked()))
}

// SaveProxies 覆写代理池并落盘（面板保存入口）。
func SaveProxies(entries []ProxyEntry) error {
	return SaveProxiesTo(ensurePath(), entries)
}

// SaveProxiesTo 写入指定路径并更新内存池（data 目录由调用方决定）。
func SaveProxiesTo(path string, entries []ProxyEntry) error {
	proxyPool.mu.Lock()
	proxyPool.path = path
	raw, err := json.MarshalIndent(struct {
		Proxies []ProxyEntry `json:"proxies"`
	}{entries}, "", "  ")
	if err != nil {
		proxyPool.mu.Unlock()
		return err
	}
	if err := os.WriteFile(proxyPool.path, raw, 0o600); err != nil {
		proxyPool.mu.Unlock()
		return err
	}
	proxyPool.entries = entries
	n := len(enabledProxiesLocked())
	proxyPool.mu.Unlock()
	log.Printf("[proxy] 代理池更新 %d 个出口", n)
	return nil
}

// ProxyList 返回当前池的拷贝（面板展示）。
func ProxyList() []ProxyEntry {
	proxyPool.mu.RLock()
	defer proxyPool.mu.RUnlock()
	out := make([]ProxyEntry, len(proxyPool.entries))
	copy(out, proxyPool.entries)
	return out
}

// ensurePath path 为空时补默认（data 目录）。仅面板保存路径会用。
func ensurePath() string {
	if proxyPool.path != "" {
		return proxyPool.path
	}
	return "data/proxies.json"
}

func enabledProxiesLocked() []ProxyEntry {
	out := make([]ProxyEntry, 0, len(proxyPool.entries))
	for _, e := range proxyPool.entries {
		if !e.Disabled && e.Addr != "" {
			out = append(out, e)
		}
	}
	return out
}

// ProxyFor 账号 uid → 固定代理。池空返回 nil（直连）。
func ProxyFor(uid string) *url.URL {
	proxyPool.mu.RLock()
	enabled := enabledProxiesLocked()
	proxyPool.mu.RUnlock()
	if len(enabled) == 0 {
		return nil
	}
	sum := sha256.Sum256([]byte(uid))
	idx := int(sum[0]) | int(sum[1])<<8 // 65536 取模，池内均匀
	e := enabled[idx%len(enabled)]
	u := &url.URL{Scheme: "http", Host: e.Addr}
	if e.User != "" {
		u.User = url.UserPassword(e.User, e.Pass)
	}
	return u
}

// ProxyFunc 生成 http.Transport.Proxy 函数：按请求头里的账号 uid 分流。
// chatOnce / doJSON 发请求前把 uid 放进 request context（ctxKeyProxyUID）。
func ProxyFunc(r *http.Request) (*url.URL, error) {
	uid := r.Header.Get("X-WB-Proxy-UID")
	if uid == "" {
		return nil, nil // 无账号上下文（如面板代理测试）：直连
	}
	return ProxyFor(uid), nil
}

// BasicProxyAuth 给 HTTP 代理补 Proxy-Authorization（Go 标准 Transport 会对
// http/https 目标自动带 CONNECT 隧道认证；此函数用于显式测试拨号）。
func BasicProxyAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// DialTest 拨号测试：经指定代理访问出口 IP 回显服务，返回 (出口IP, 耗时ms, err)。
// 面板「测试」按钮用；不进账号分流路径。
func DialTest(e ProxyEntry) (string, int64, error) {
	u := &url.URL{Scheme: "http", Host: e.Addr}
	if e.User != "" {
		u.User = url.UserPassword(e.User, e.Pass)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	client := &http.Client{Timeout: 12 * time.Second, Transport: tr}
	start := time.Now()
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		// ipify 失败再试 ifconfig.me（任一成功即认为代理可用）
		resp, err = client.Get("http://ifconfig.me/ip")
		if err != nil {
			return "", 0, err
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", 0, err
	}
	ip := strings.TrimSpace(string(raw))
	if len(ip) == 0 || len(ip) > 45 || strings.Contains(ip, " ") {
		return "", 0, fmt.Errorf("出口响应异常: %.60s", ip)
	}
	return ip, time.Since(start).Milliseconds(), nil
}
