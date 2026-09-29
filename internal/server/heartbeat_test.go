package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
)

// 宽限期内的快速失败：原状态码与响应体透传，不出现心跳。
func TestHeartbeatFastErrorKeepsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := startSSEHeartbeat(rec, openAIErrFrame)
	writeOpenAIError(hb, http.StatusBadRequest, "invalid_request", "blocked")
	w := hb.Stop()
	if w != http.ResponseWriter(rec) {
		t.Fatalf("uncommitted Stop should return original writer")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("unexpected heartbeat: %q", rec.Body.String())
	}
}

// 派发超过宽限期：提交 200 + SSE 头并周期心跳；之后的错误转为 SSE 错误帧。
func TestHeartbeatSlowErrorBecomesFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := &sseHeartbeat{w: rec, errFrame: openAIErrFrame, hdr: http.Header{},
		stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	go hb.run(10*time.Millisecond, 10*time.Millisecond)
	time.Sleep(45 * time.Millisecond)
	writeOpenAIError(hb, http.StatusServiceUnavailable, "no_healthy_account", "all accounts unavailable")
	hb.Stop()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	out := rec.Body.String()
	if n := strings.Count(out, ": keepalive\n\n"); n < 2 {
		t.Fatalf("keepalive count = %d, body %q", n, out)
	}
	if !strings.Contains(out, `data: {"error":{"code":"no_healthy_account"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing error frame: %q", out)
	}
}

// 派发超过宽限期后成功：中继 writer 屏蔽重复 WriteHeader，正文接在心跳后。
func TestHeartbeatSlowSuccessRelay(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := &sseHeartbeat{w: rec, errFrame: responsesErrFrame, hdr: http.Header{},
		stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	go hb.run(5*time.Millisecond, time.Hour)
	time.Sleep(25 * time.Millisecond)
	w := hb.Stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusTeapot) // 应被忽略
	_, _ = w.Write([]byte("event: response.created\ndata: {}\n\n"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), ": keepalive\n\nevent: response.created") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if _, ok := w.(http.Flusher); !ok {
		t.Fatalf("relay writer must stay flushable")
	}
}

func TestErrFrameShapes(t *testing.T) {
	body := []byte(`{"error":{"message":"all accounts rate-limited","type":"api_error","code":"model_rate_limited"}}`)
	a := string(anthropicErrFrame(http.StatusTooManyRequests, body))
	if !strings.HasPrefix(a, "event: error\ndata: ") || !strings.Contains(a, `"rate_limit_error"`) || !strings.Contains(a, "all accounts rate-limited") {
		t.Fatalf("anthropic frame = %q", a)
	}
	r := string(responsesErrFrame(http.StatusTooManyRequests, body))
	if !strings.Contains(r, `"code":"model_rate_limited"`) || !strings.Contains(r, `"type":"error"`) {
		t.Fatalf("responses frame = %q", r)
	}
}

// slowUpstream 首次打开流前阻塞 delay（模拟上游等响应头）。
type slowUpstream struct {
	rotateUpstream
	delay time.Duration
}

func (u *slowUpstream) ChatStream(a *auth.Auth, b []byte) (io.ReadCloser, int, []byte, error) {
	time.Sleep(u.delay)
	return u.rotateUpstream.ChatStream(a, b)
}

func (u *slowUpstream) Stream(w http.ResponseWriter, r io.Reader) error {
	_, err := io.Copy(w, r)
	return err
}

// 端到端：流式请求派发超过宽限期时，客户端先收到 200 + 心跳，再收到正文。
func TestChatCompletionsHeartbeatDuringSlowDispatch(t *testing.T) {
	old := dispatchHeartbeatGrace
	dispatchHeartbeatGrace = 20 * time.Millisecond
	defer func() { dispatchHeartbeatGrace = old }()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	h := NewHandler(Config{Pool: p, Upstream: &slowUpstream{delay: 80 * time.Millisecond}, MaxRotate: 3})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.HasPrefix(out, ": keepalive\n\n") || !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("body = %q", out)
	}
}
