package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/upstream"
	"wild-work/internal/provider"
)

type rotateUpstream struct {
	mu    sync.Mutex
	calls []string
	limit map[string]bool
}

func (u *rotateUpstream) RefreshToken(*auth.Auth) error { return nil }
func (u *rotateUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (u *rotateUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *rotateUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *rotateUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *rotateUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *rotateUpstream) Classify(status int, body string) provider.ErrKind {
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		return provider.ErrClient
	}
	return provider.ErrNone
}
func (u *rotateUpstream) Stream(http.ResponseWriter, io.Reader) error { return nil }
func (u *rotateUpstream) Aggregate(io.Reader) (map[string]any, error) { return nil, nil }

func (u *rotateUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	limit := u.limit[a.UID]
	u.mu.Unlock()
	if limit {
		body := `{"code":6004,"msg":"glm-5.3 用量超限，将在 2099-01-01 18:00:00 重置"}`
		return nil, http.StatusTooManyRequests, []byte(body), nil
	}
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	return io.NopCloser(strings.NewReader(sse)), http.StatusOK, nil, nil
}

func TestDispatchChatRotatesOnModelRateLimit(t *testing.T) {
	p := pool.New("")
	a1 := &auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	a2 := &auth.Auth{UID: "u2", AccessToken: "t2", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 90)
	up := &rotateUpstream{limit: map[string]bool{"u1": true}}
	h := NewHandler(Config{
		Pool:      p,
		Upstream:  up,
		MaxRotate: 5,
	})
	rt := h.cfg.Runtimes[provider.WorkBuddy]
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	h.sticky[h.stickyKey(rt.Kind, upstream.ConversationFingerprint(body))] = &stickyEntry{uid: "u1", maxReqs: 50, lastUsed: time.Now()}
	rec := httptest.NewRecorder()
	rc, uid, ok := h.dispatchChat(rt, time.Now(), "workbuddy/glm-5.3", body, rec)
	if !ok {
		t.Fatalf("dispatch failed status=%d body=%s", rec.Code, rec.Body.String())
	}
	defer rc.Close()
	if uid != "u2" {
		t.Fatalf("uid=%s want u2", uid)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.calls) < 2 || up.calls[len(up.calls)-1] != "u2" {
		t.Fatalf("calls=%v want rotate onto u2", up.calls)
	}
	if !p.CooledForModel("u1", "glm-5.3") {
		t.Fatal("u1 glm-5.3 should be cooling")
	}
	if rec.Code != 200 && rec.Body.Len() != 0 {
		t.Fatalf("client saw error status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDispatchChatDoesNotReturn6004WhenAnotherAccountWorks(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "t2", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	up := &rotateUpstream{limit: map[string]bool{"u1": true}}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"workbuddy/glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("client saw 6004: %s", rec.Body.String())
	}
	var peek struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &peek)
	if len(peek.Error) > 0 {
		t.Fatalf("error envelope: %s", rec.Body.String())
	}
}

type contentBlockUpstream struct{ rotateUpstream }

func (u *contentBlockUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	body := `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过安全审核"}}`
	return nil, http.StatusForbidden, []byte(body), nil
}

// 内容审核拦截必须回 400：monoize 类中转把 403 当渠道永久故障并熔断整条上游。
func TestContentBlockReturns400(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	h := NewHandler(Config{Pool: p, Upstream: &contentBlockUpstream{}, MaxRotate: 5})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"workbuddy/glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Code != "content_policy_violation" {
		t.Fatalf("body=%s want content_policy_violation", rec.Body.String())
	}
}

// 会话 A 的 6004 换号不得牵连会话 B：粘性按会话分组，
// 换号只冷该会话自己的上游前缀缓存（prompt_cache_key 含 uid）。
func TestStickyIsPerConversation(t *testing.T) {
	p := pool.New("")
	for _, id := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: id, AccessToken: "t-" + id, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	}
	up := &rotateUpstream{limit: map[string]bool{}}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5})
	rt := h.cfg.Runtimes[provider.WorkBuddy]

	convA := []byte(`{"model":"glm-5.3","conversation_id":"A","messages":[{"role":"user","content":"a"}]}`)
	convB := []byte(`{"model":"glm-5.3","conversation_id":"B","messages":[{"role":"user","content":"b"}]}`)

	// 预置：A、B 各自粘在 u1/u2
	h.sticky[h.stickyKey(rt.Kind, upstream.ConversationFingerprint(convA))] = &stickyEntry{uid: "u1", maxReqs: 50, lastUsed: time.Now()}
	h.sticky[h.stickyKey(rt.Kind, upstream.ConversationFingerprint(convB))] = &stickyEntry{uid: "u2", maxReqs: 50, lastUsed: time.Now()}

	// A 的粘性账号 u1 被 6004 限流 → A 换到别的号
	up.limit["u1"] = true
	rec := httptest.NewRecorder()
	rc, uidA, ok := h.dispatchChat(rt, time.Now(), "workbuddy/glm-5.3", convA, rec)
	if !ok {
		t.Fatalf("convA failed status=%d body=%s", rec.Code, rec.Body.String())
	}
	rc.Close()
	if uidA == "u1" {
		t.Fatal("convA should rotate off u1")
	}

	// B 的粘性必须不受影响：仍走 u2
	rec = httptest.NewRecorder()
	rc, uidB, ok := h.dispatchChat(rt, time.Now(), "workbuddy/glm-5.3", convB, rec)
	if !ok {
		t.Fatalf("convB failed status=%d body=%s", rec.Code, rec.Body.String())
	}
	rc.Close()
	if uidB != "u2" {
		t.Fatalf("convB uid=%s want u2 (unrelated rotation must not move it)", uidB)
	}
}

// 内容审核（客户端侧错误）不清粘性：账号健康，会话下一轮继续用同号保缓存。
func TestContentBlockKeepsSticky(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "t2", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	h := NewHandler(Config{Pool: p, Upstream: &contentBlockUpstream{}, MaxRotate: 5})
	rt := h.cfg.Runtimes[provider.WorkBuddy]

	conv := []byte(`{"model":"glm-5.3","conversation_id":"C","messages":[{"role":"user","content":"c"}]}`)
	key := h.stickyKey(rt.Kind, upstream.ConversationFingerprint(conv))
	h.sticky[key] = &stickyEntry{uid: "u1", maxReqs: 50, lastUsed: time.Now()}

	rec := httptest.NewRecorder()
	if _, _, ok := h.dispatchChat(rt, time.Now(), "workbuddy/glm-5.3", conv, rec); ok {
		t.Fatal("content block should fail dispatch")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	h.stickyMu.RLock()
	e := h.sticky[key]
	h.stickyMu.RUnlock()
	if e == nil || e.uid != "u1" {
		t.Fatalf("sticky=%v want u1 retained (client-side error must not clear sticky)", e)
	}
}

// 上游特征掩码：模型列表不暴露渠道前缀 / owned_by；带前缀旧名仍可路由。
func TestModelListMasked(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	h := NewHandler(Config{Pool: p, Upstream: &rotateUpstream{}, MaxRotate: 5})
	list := h.modelList()
	if len(list) == 0 {
		t.Fatal("empty model list")
	}
	for _, m := range list {
		id, _ := m["id"].(string)
		if strings.Contains(id, "workbuddy/") || strings.Contains(id, "traework/") || strings.Contains(id, "qoder/") {
			t.Fatalf("masked list must not expose channel prefix: %q", id)
		}
		if ob, _ := m["owned_by"].(string); ob == "workbuddy" {
			t.Fatalf("owned_by must be neutral, got %q", ob)
		}
	}
	// 掩码关闭时恢复前缀
	h2 := NewHandler(Config{Pool: p, Upstream: &rotateUpstream{}, MaxRotate: 5})
	off := false
	h2.maskUpstream = off
	_ = off
	found := false
	for _, m := range h2.modelList() {
		if id, _ := m["id"].(string); strings.HasPrefix(id, "workbuddy/") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mask off must expose channel-prefixed ids")
	}
}

// 上游错误报文掩码：客户端拿到中性 OpenAI 错误，不含上游 code/文案。
func TestUpstreamErrorMasked(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	up := &paramErrUpstream{}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"workbuddy/glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "11133") || strings.Contains(body, "codebuddy") || strings.Contains(body, "WorkBuddy") {
		t.Fatalf("upstream identity leaked: %s", body)
	}
	if !strings.Contains(body, `"api_error"`) {
		t.Fatalf("want neutral OpenAI error, got: %s", body)
	}
}

type paramErrUpstream struct{ rotateUpstream }

func (u *paramErrUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, http.StatusBadRequest,
		[]byte(`{"code":11133,"msg":"invalid params by www.codebuddy.cn","requestId":"wb-xyz"}`), nil
}

// 响应指纹掩码：SSE id 换 chatcmpl-、model 回显请求名、usage 剥非标字段。
func TestSSEMaskWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	mw := newSSEMaskWriter(rec, "glm-5.3")
	in := "data: {\"id\":\"cmb-abc\",\"model\":\"glm-5.3\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"cmb-abc\",\"model\":\"glm-5.3\",\"object\":\"chat.completion.chunk\",\"usage\":{\"prompt_tokens\":14,\"completion_tokens\":20,\"credit\":0.01,\"prompt_cache_hit_tokens\":900,\"prompt_cache_miss_tokens\":14,\"completion_thinking_tokens\":18,\"cache_creation_input_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":0}},\"choices\":[]}\n\n" +
		"data: [DONE]\n\n"
	if n, err := mw.Write([]byte(in)); err != nil || n != len(in) {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	mw.Flush()
	out := rec.Body.String()
	if strings.Contains(out, "cmb-") || strings.Contains(out, "credit") ||
		strings.Contains(out, "prompt_cache_hit_tokens") || strings.Contains(out, "completion_thinking_tokens") {
		t.Fatalf("fingerprint leaked:\n%s", out)
	}
	if !strings.Contains(out, "chatcmpl-") {
		t.Fatalf("id not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "\"cached_tokens\":900") {
		t.Fatalf("cache hit not mapped to cached_tokens:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("DONE frame lost:\n%s", out)
	}
	// 同流 id 稳定
	first := strings.Index(out, "chatcmpl-")
	second := strings.Index(out[first+1:], "chatcmpl-")
	if second >= 0 {
		a := out[first : first+22]
		b := out[first+1+second : first+1+second+22]
		if a != b {
			t.Fatalf("id must be stable within stream: %q vs %q", a, b)
		}
	}
}

// 非流式掩码：resp map 的 id/model/usage 同样清洗。
func TestMaskAggregateResp(t *testing.T) {
	resp := map[string]any{
		"id": "cmb-xyz", "model": "glm-5.3",
		"usage": map[string]any{"prompt_tokens": 10, "credit": 0.5, "prompt_cache_hit_tokens": float64(8)},
	}
	maskAggregateResp(resp, "glm-5.3")
	if id, _ := resp["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id=%v", resp["id"])
	}
	u := resp["usage"].(map[string]any)
	if _, ok := u["credit"]; ok {
		t.Fatal("credit must be stripped")
	}
	if _, ok := u["prompt_cache_hit_tokens"]; ok {
		t.Fatal("prompt_cache_hit_tokens must be stripped")
	}
	if u["cached_tokens"] != float64(8) {
		t.Fatalf("cached_tokens=%v", u["cached_tokens"])
	}
}
