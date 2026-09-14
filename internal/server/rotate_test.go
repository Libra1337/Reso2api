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
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: "u1", maxReqs: 50}
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
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
