package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// 重会话分散：两个大上下文会话必须落到不同账号（上游单账号缓存容量
// 有限，挤同账号会互相 LRU 踢缓存——2026-10-30 实测 100%/1.3% 交替）。
func TestHeavySessionsDistributeAcrossAccounts(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "t2", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)
	up := &rotateUpstream{}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5})
	rt := h.cfg.Runtimes[provider.WorkBuddy]

	big := func(tag string) []byte {
		return []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"` + tag + `:` +
			strings.Repeat("重", 260000/3) + `"}]}`) // ~260KB ≈ 118k tokens ≥ heavy 门槛
	}
	sess1 := upstream.ConversationFingerprint(big("one"))
	sess2 := upstream.ConversationFingerprint(big("two"))
	if sess1 == sess2 {
		t.Fatal("两个会话指纹不应相同")
	}
	_, uid1, ok := h.dispatchChat(rt, time.Now(), "glm-5.3-flash", big("one"), httptest.NewRecorder(), sess1)
	if !ok {
		t.Fatal("dispatch1 failed")
	}
	_, uid2, ok := h.dispatchChat(rt, time.Now(), "glm-5.3-flash", big("two"), httptest.NewRecorder(), sess2)
	if !ok {
		t.Fatal("dispatch2 failed")
	}
	t.Logf("heavy session1 -> %s, session2 -> %s", uid1, uid2)
	if uid1 == uid2 {
		t.Fatalf("两个重会话落到了同一账号 %s(应分散)", uid1)
	}
	// 重会话条目带 heavy 标记,轻会话不带
	if !h.sticky[h.stickyKey(rt.Kind, sess1)].heavy {
		t.Fatal("重会话粘性条目应带 heavy 标记")
	}
	light := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	lsess := upstream.ConversationFingerprint(light)
	if _, _, ok := h.dispatchChat(rt, time.Now(), "glm-5.3", light, httptest.NewRecorder(), lsess); !ok {
		t.Fatal("light dispatch failed")
	}
	if h.sticky[h.stickyKey(rt.Kind, lsess)].heavy {
		t.Fatal("轻会话不应标记 heavy")
	}
}

// heavyLoadOf 只统计 1h 内活跃的重会话条目。
func TestHeavyLoadOfActive(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	now := time.Now()
	h.sticky["a"] = &stickyEntry{uid: "u1", heavy: true, lastUsed: now}
	h.sticky["b"] = &stickyEntry{uid: "u1", heavy: true, lastUsed: now.Add(-2 * time.Hour)} // 过期不计
	h.sticky["c"] = &stickyEntry{uid: "u1", heavy: false, lastUsed: now}                   // 轻会话不计
	h.sticky["d"] = &stickyEntry{uid: "u2", heavy: true, lastUsed: now}
	if n := h.heavyLoadOf("u1"); n != 1 {
		t.Fatalf("u1 heavyLoad=%d want 1", n)
	}
	if n := h.heavyLoadOf("u2"); n != 1 {
		t.Fatalf("u2 heavyLoad=%d want 1", n)
	}
}


// 轮换路径同样体量分散:已有大前缀的账号在换号时被避开。
func TestRotationAvoidsLoadedAccount(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "rich", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "quiet", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.SetCredits("rich", 1000)
	p.SetCredits("quiet", 10) // rich 积分碾压,普通选号必落 rich
	up := &rotateUpstream{}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5})
	rt := h.cfg.Runtimes[provider.WorkBuddy]
	// 账号 rich 已绑一个 500k 大前缀会话
	h.sticky["loaded"] = &stickyEntry{uid: "rich", lastUsed: time.Now(), heavy: true, estTok: 500000}
	// 新的中等会话(120k ≥ distributeMinTokens)落号应避开 rich
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"` + strings.Repeat("转", 120000) + `"}}]}`)
	sess := upstream.ConversationFingerprint(body)
	_, uid, ok := h.dispatchChat(rt, time.Now(), "glm-5.3-flash", body, httptest.NewRecorder(), sess)
	if !ok {
		t.Fatal("dispatch failed")
	}
	if uid != "quiet" {
		t.Fatalf("中等会话应避开已载账号 rich,实际落 %s", uid)
	}
}
