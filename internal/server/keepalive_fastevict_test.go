package server

import (
	"testing"
	"time"

	"wild-work/internal/config"
)

// 快淘汰模型(glm-5.3-flash)走秒级加密保活节奏:闲置 90s 即到期,而常规模型
// 按默认 6 分钟;ping 间隔 60s、上限 20 轮。
func TestKeepaliveFastEvictCadence(t *testing.T) {
	cfg := config.KeepaliveConfig{Enabled: true, MinTokens: 100}.Normalized()
	s := newKeepaliveStore(t.TempDir(), cfg)
	now := time.Now()
	s.entries["sess-flash"] = &keepaliveEntry{
		kind: "workbuddy", uid: "u1", model: "glm-5.3-flash", bytes: 300_000,
		lastSeen: now.Add(-2 * time.Minute), // 闲置 2 分钟
	}
	s.entries["sess-normal"] = &keepaliveEntry{
		kind: "workbuddy", uid: "u2", model: "glm-5.3", bytes: 300_000,
		lastSeen: now.Add(-2 * time.Minute),
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
		t.Fatal("glm-5.3-flash 闲置 2 分钟应已到期(fastEvict 90s)")
	}
	if normalDue {
		t.Fatal("常规模型闲置 2 分钟不应到期(默认 6min)")
	}
}

// 60s ping 间隔:上一发 ping 后不足 60s 不发,超过 60s 再发。
func TestKeepaliveFastEvictPingEvery(t *testing.T) {
	cfg := config.KeepaliveConfig{Enabled: true, MinTokens: 100}.Normalized()
	s := newKeepaliveStore(t.TempDir(), cfg)
	base := time.Now().Add(-10 * time.Minute)
	e := &keepaliveEntry{
		kind: "workbuddy", uid: "u1", model: "glm-5.3-flash", bytes: 300_000,
		lastSeen: base, lastPing: base.Add(9 * time.Minute), pings: 1,
	}
	s.entries["sess"] = e
	if due := s.sweepDue(base.Add(9*time.Minute + 50*time.Second)); len(due) != 0 {
		t.Fatal("ping 后 50s(<60s 间隔)不应再发")
	}
	if due := s.sweepDue(base.Add(10*time.Minute + 1*time.Second)); len(due) != 1 {
		t.Fatal("ping 后 61s(≥60s 间隔)应再发")
	}
}

// 体量护栏:超过 fastEvictMaxBytes 的巨型前缀不快 ping(上游亚分钟丢,
// 快 ping 白烧配额),回落默认慢节奏。
func TestKeepaliveMegaPrefixUsesDefaultCadence(t *testing.T) {
	cfg := config.KeepaliveConfig{Enabled: true, MinTokens: 100}.Normalized()
	mega := &keepaliveEntry{kind: "workbuddy", uid: "u1", model: "glm-5.3-flash", bytes: fastEvictMaxBytes + 1}
	idle, every, mp := cadenceFor(cfg, mega)
	if idle != time.Duration(cfg.IdleAfterMin)*time.Minute ||
		every != time.Duration(cfg.PingEveryMin)*time.Minute || mp != cfg.MaxPings {
		t.Fatalf("超大前缀应走默认节奏 %dm/%dm/%d, got %v/%v/%d", cfg.IdleAfterMin, cfg.PingEveryMin, cfg.MaxPings, idle, every, mp)
	}
	normal := &keepaliveEntry{kind: "workbuddy", uid: "u2", model: "glm-5.3-flash", bytes: fastEvictMaxBytes}
	if idle, _, _ := cadenceFor(cfg, normal); idle != fastEvictIdleAfter {
		t.Fatalf("护栏内的 flash 应走快节奏 %v, got %v", fastEvictIdleAfter, idle)
	}
	// 实测样本:2.47MB 的 59 万 token 前缀必须落在护栏外
	mega2 := &keepaliveEntry{model: "glm-5.3-flash", bytes: 2_469_575}
	if mega2.bytes <= fastEvictMaxBytes {
		t.Fatal("2.47MB 前缀应超出护栏")
	}
}

// 快淘汰模型允许的轮数上限更高(20),窗口内持续续命。
func TestKeepaliveFastEvictMaxPings(t *testing.T) {
	cfg := config.KeepaliveConfig{Enabled: true, MinTokens: 100}.Normalized()
	if _, _, mp := cadenceFor(cfg, &keepaliveEntry{model: "glm-5.3-flash", bytes: 300_000}); mp != fastEvictMaxPings {
		t.Fatalf("flash maxPings=%d want %d", mp, fastEvictMaxPings)
	}
	if _, _, mp := cadenceFor(cfg, &keepaliveEntry{model: "glm-5.3", bytes: 300_000}); mp != cfg.MaxPings {
		t.Fatalf("常规模型应走默认 maxPings=%d, got %d", cfg.MaxPings, mp)
	}
	// 变体名(-vl/@think)也应命中快淘汰节奏
	if !isFastEvictModel("glm-5.3-flash-vl") || !isFastEvictModel("glm-5.3-flash@think") {
		t.Fatal("变体名应命中快淘汰节奏")
	}
}
