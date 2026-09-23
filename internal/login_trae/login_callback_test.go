package login_trae

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStartWithCallbackBaseURL 服务器形态：回调地址注入授权 URL，且不开本地监听。
func TestStartWithCallbackBaseURL(t *testing.T) {
	stateFP := filepath.Join(t.TempDir(), "state.json")
	u, err := StartWithCallbackBase(NewClient(), stateFP, "https://gw.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, url.QueryEscape("https://gw.example.com/oauth/callback/traework")) {
		t.Fatalf("授权 URL 未包含网关回调地址: %.200s", u)
	}
	if u == "" || !strings.Contains(u, "authorization?") {
		t.Fatalf("授权 URL 形态异常: %.120s", u)
	}
	// state 文件应含 PKCE verifier
	raw, err := os.ReadFile(stateFP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"codeVerifier"`) {
		t.Fatalf("state 缺 codeVerifier: %.120s", raw)
	}
}

// TestHandleCallbackWritesState 回调 handler 把凭证写进 state（GET 形态）。
func TestHandleCallbackWritesState(t *testing.T) {
	stateFP := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(stateFP, []byte(`{"machineId":"m","deviceId":"d","codeVerifier":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/oauth/callback/traework?refreshToken=RT&host=https%3A%2F%2Fapi.trae.com.cn", nil)
	HandleCallback(stateFP)(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "登录已完成") {
		t.Fatalf("回调响应异常: %d %.80s", w.Code, w.Body.String())
	}
	raw, _ := os.ReadFile(stateFP)
	if !strings.Contains(string(raw), `"refreshToken": "RT"`) {
		t.Fatalf("state 未写入 refreshToken: %.200s", raw)
	}
	if !strings.Contains(string(raw), `"host": "https://api.trae.com.cn"`) {
		t.Fatalf("state 未写入 host: %.200s", raw)
	}
}

// TestStartLocalStillWorks 本机形态（无 callbackBase）：URL 回调指向 127.0.0.1 一次性监听。
func TestStartLocalStillWorks(t *testing.T) {
	stateFP := filepath.Join(t.TempDir(), "state.json")
	u, err := Start(NewClient(), stateFP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, url.QueryEscape("http://127.0.0.1:")) {
		t.Fatalf("本机模式回调应指向 127.0.0.1: %.200s", u)
	}
}
