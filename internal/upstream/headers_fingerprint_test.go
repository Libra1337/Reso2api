package upstream

import (
	"net/http"
	"testing"

	"wild-work/internal/auth"
)

// UA 三段式与用量归属头组（对齐官方桌面端）。
func TestFingerprintHeaders(t *testing.T) {
	c := &Client{ClientName: "WorkBuddy"}
	req, _ := http.NewRequest("POST", "https://x", nil)
	a := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: "dev-1"}
	c.ChatHeaders(req, a)

	if ua := req.Header.Get("User-Agent"); ua != "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1" {
		t.Errorf("chat UA = %q", ua)
	}
	if v := req.Header.Get("X-Agent-Purpose"); v != "conversation" {
		t.Errorf("X-Agent-Purpose = %q", v)
	}
	for _, h := range []string{"X-IDE-Name", "X-IDE-Type", "X-Product"} {
		if req.Header.Get(h) != "WorkBuddy" {
			t.Errorf("%s = %q", h, req.Header.Get(h))
		}
	}
	if v := req.Header.Get("X-IDE-Version"); v != "5.5.4" {
		t.Errorf("X-IDE-Version = %q", v)
	}
	if v := req.Header.Get("X-Device-Token"); v != "dev-1" {
		t.Errorf("X-Device-Token = %q", v)
	}

	// billing：单段 UA + 设备头
	req2, _ := http.NewRequest("GET", "https://x", nil)
	c.BillingHeaders(req2, a)
	if ua := req2.Header.Get("User-Agent"); ua != "WorkBuddy/5.5.4" {
		t.Errorf("billing UA = %q", ua)
	}
	if req2.Header.Get("X-Device-Token") != "dev-1" {
		t.Error("billing should carry device token")
	}
}

// 未配置 ClientName：保持旧兼容形态（X-Product=SaaS，无 X-IDE-*，billing 无 UA）。
func TestFingerprintHeadersLegacy(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://x", nil)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "at"})
	if req.Header.Get("X-Product") != "SaaS" {
		t.Errorf("X-Product = %q", req.Header.Get("X-Product"))
	}
	if req.Header.Get("X-Agent-Purpose") != "" || req.Header.Get("X-IDE-Name") != "" {
		t.Error("legacy mode must not set attribution headers")
	}
	if req.Header.Get("X-Device-Token") != "" {
		t.Error("no token configured, must not inject")
	}
	req2, _ := http.NewRequest("GET", "https://x", nil)
	c.BillingHeaders(req2, &auth.Auth{})
	if req2.Header.Get("User-Agent") != "" {
		t.Errorf("billing UA = %q, want empty in legacy mode", req2.Header.Get("User-Agent"))
	}
}

// 版本覆盖与设备 token 优先级（auth 每号 > 全局）。
func TestFingerprintOverrides(t *testing.T) {
	c := &Client{ClientName: "WorkBuddy", ClientVersion: "9.9.9", CliVersion: "1.0.0", DeviceToken: "global-tok"}
	if ua := c.chatUA(); ua != "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/1.0.0" {
		t.Errorf("UA override = %q", ua)
	}
	req, _ := http.NewRequest("POST", "https://x", nil)
	c.ChatHeaders(req, &auth.Auth{DeviceToken: "per-account"})
	if req.Header.Get("X-Device-Token") != "per-account" {
		t.Errorf("device token priority wrong: %q", req.Header.Get("X-Device-Token"))
	}
}
