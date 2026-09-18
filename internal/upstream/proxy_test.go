package upstream

import (
	"net/http"
	"testing"
)

// setTestPool 写入测试池（直接操作内部状态，避开文件依赖）。
func setTestPool(t *testing.T, entries []ProxyEntry) {
	t.Helper()
	proxyPool.mu.Lock()
	proxyPool.entries = entries
	proxyPool.mu.Unlock()
}

// 空池 → 直连（nil）。
func TestProxyForEmptyPool(t *testing.T) {
	setTestPool(t, nil)
	if got := ProxyFor("uid-1"); got != nil {
		t.Fatalf("empty pool must give nil, got %v", got)
	}
}

// 同一 uid 稳定映射同一代理；不同 uid 均匀分散。
func TestProxyForStableAndDistributed(t *testing.T) {
	pool := make([]ProxyEntry, 10)
	for i := range pool {
		pool[i] = ProxyEntry{Addr: "10.0.0.1:800" + string(rune('0'+i)), User: "u", Pass: "p"}
	}
	setTestPool(t, pool)
	first := ProxyFor("uid-abc")
	for i := 0; i < 5; i++ {
		if got := ProxyFor("uid-abc"); got.String() != first.String() {
			t.Fatal("same uid must map to same proxy")
		}
	}
	// 60 个 uid 分散到 10 个出口：至少命中 7 个不同出口。
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		u := ProxyFor("uid-" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune('a'+i%7)))
		if u != nil {
			seen[u.Host] = true
		}
	}
	if len(seen) < 7 {
		t.Fatalf("distribution too skewed: %d outlets", len(seen))
	}
}

// disabled 代理不进池；全 disabled 等价空池直连。
func TestProxyForSkipsDisabled(t *testing.T) {
	setTestPool(t, []ProxyEntry{{Addr: "a:1", Disabled: true}, {Addr: "b:2"}})
	u := ProxyFor("x")
	if u == nil || u.Host != "b:2" {
		t.Fatalf("want b:2, got %v", u)
	}
	setTestPool(t, []ProxyEntry{{Addr: "a:1", Disabled: true}})
	if got := ProxyFor("x"); got != nil {
		t.Fatalf("all-disabled pool must give nil, got %v", got)
	}
}

// 认证形态：user/pass 进 URL userinfo。
func TestProxyForAuth(t *testing.T) {
	setTestPool(t, []ProxyEntry{{Addr: "1.2.3.4:8080", User: "ukin", Pass: "pw"}})
	u := ProxyFor("any")
	if u.User.String() != "ukin:pw" {
		t.Fatalf("auth missing: %v", u.User)
	}
}

// ProxyFunc：无 uid 头直连；有 uid 头分流到对应代理。
func TestProxyFuncByHeader(t *testing.T) {
	setTestPool(t, []ProxyEntry{{Addr: "9.9.9.9:3128"}})
	req, _ := http.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
	if got, _ := ProxyFunc(req); got != nil {
		t.Fatalf("no-uid request must be direct, got %v", got)
	}
	req.Header.Set("X-WB-Proxy-UID", "u1")
	got, _ := ProxyFunc(req)
	if got == nil || got.Host != "9.9.9.9:3128" {
		t.Fatalf("uid request must go via pool, got %v", got)
	}
}

// SaveProxiesTo/LoadProxies 往返 + ProxyList 拷贝（隔离 data 目录互不污染）。
func TestProxySaveLoadRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	entries := []ProxyEntry{{Addr: "1.1.1.1:80", User: "u", Pass: "p"}, {Addr: "2.2.2.2:80"}}
	if err := SaveProxiesTo(tmp+"/proxies.json", entries); err != nil {
		t.Fatal(err)
	}
	LoadProxies(tmp)
	got := ProxyList()
	if len(got) != 2 || got[0].Addr != "1.1.1.1:80" || got[0].User != "u" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}
