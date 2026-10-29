package server

import (
	"testing"
	"time"

	"wild-work/internal/config"
)

// 快淘汰模型(glm-5.3-flash)走加密保活节奏:闲置 2 分钟即到期,而常规模型
// 按默认 6 分钟;ping 间隔 3 分钟、上限 15 轮。
func TestKeepaliveFastEvictCadence(t *testing.T) {
	cfg := config.KeepaliveConfig{Enabled: true, MinTokens: 100}.Normalized()
	s := newKeepaliveStore(t.TempDir(), cfg)
	now := time.Now()
	s.entries["sess-flash"] = &keepaliveEntry{
		kind: "workbuddy", uid: "u1", model: "glm-5.3-flash",
		lastSeen: now.Add(-3 * time.Minute), // 闲置 3 分钟
	}
	s.entries["sess-normal"] = &keepaliveEntry{
		kind: "workbuddy", uid: "u2", model: "glm-5.3",
		lastSeen: now.Add(-3 * time.Minute),
	}
	due := s.sweepDue(now)
	flashDue, normalDue := false, false
	for _, e := range due {
		if e.model == "glm-5.3-flash" {
			flashDue = true
		} else {
			normalDue = true
		}
	}
	if !flashDue {
		t.Fatal("glm-5.3-flash 闲置 3 分钟应已到期(fastEvict 2min)")
	}
	if normalDue {
		t.Fatal("常规模型闲置 3 分钟不应到期(默认 6min)")
	}
}

// 快淘汰模型允许的轮数上限更高(15),窗口内持续续命。
func TestKeepaliveFastEvictMaxPings(t *testing.T) {
	cfg := config.KeepaliveConfig{Enabled: true, MinTokens: 100}.Normalized()
	if _, _, mp := cadenceFor(cfg, "glm-5.3-flash"); mp != fastEvictMaxPings {
		t.Fatalf("flash maxPings=%d want %d", mp, fastEvictMaxPings)
	}
	if _, _, mp := cadenceFor(cfg, "glm-5.3"); mp != cfg.MaxPings {
		t.Fatalf("常规模型应走默认 maxPings=%d, got %d", cfg.MaxPings, mp)
	}
	// 变体名(-vl/@think)也应命中快淘汰节奏
	if !isFastEvictModel("glm-5.3-flash-vl") || !isFastEvictModel("glm-5.3-flash@think") {
		t.Fatal("变体名应命中快淘汰节奏")
	}
}
