// keepalive.go — glm-5.3-flash 等模型的上游前缀缓存保活。
//
// 背景（2026-10-07 PG 逐请求实锤）：flash 类模型的上游前缀缓存淘汰快——
// 同会话 24 万 token 闲置 10-20 分钟后整单零命中（间隔几分钟则 96-99%）。
// 上游淘汰策略不可调；网关侧解法：会话闲置期间，定时用**同账号重放同一
// 请求前缀**（命中缓存，费用约冷启的 1/21，输出压到 max_tokens=16），
// 持续刷新缓存条目的活跃度，用户回来时直接命中。
//
// 存储：大上下文会话的出站 body 落盘 data/keepalive/<sha>.json（超过
// min_tokens≈2.2B/token 的字节量才跟踪），内存索引会话→元信息。重启丢
// 索引，孤儿文件启动清扫。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/provider"
)

// keepaliveEntry 单会话保活状态。
type keepaliveEntry struct {
	kind     string // runtime kind
	uid      string // 粘住的账号（同账号同缓存）
	file     string // body 存档路径
	lastIn   int64  // 最近一轮真实 in_tokens
	lastSeen time.Time
	lastPing time.Time
	pings    int
	inFlight bool
}

// keepaliveStore 保活索引 + 磁盘存档。
type keepaliveStore struct {
	mu      sync.Mutex
	dir     string
	cfg     config.KeepaliveConfig
	entries map[string]*keepaliveEntry // session → entry
}

func newKeepaliveStore(dir string, cfg config.KeepaliveConfig) *keepaliveStore {
	if dir == "" {
		dir = filepath.Join("data", "keepalive")
	}
	s := &keepaliveStore{dir: dir, cfg: cfg.Normalized(), entries: map[string]*keepaliveEntry{}}
	_ = os.MkdirAll(s.dir, 0o700)
	s.cleanOrphans()
	return s
}

// track 记录/刷新一个会话的保活材料（大 body 落盘覆盖旧档）。
// 调用点：chatCompletions dispatch 成功后（body 仍有效时）。
func (s *keepaliveStore) track(session, kind, uid string, body []byte) {
	if session == "" || uid == "" || !s.cfg.Enabled {
		return
	}
	// 只跟踪大上下文会话：字节估算 ~2.2B/token（保守偏早跟踪）。
	if int64(float64(len(body))/2.2) < int64(s.cfg.MinTokens) {
		return
	}
	sum := sha256.Sum256([]byte(session))
	name := hex.EncodeToString(sum[:12]) + ".json"
	fp := filepath.Join(s.dir, name)
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, fp); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[session]; ok {
		e.uid, e.kind, e.file = uid, kind, fp
		e.lastSeen, e.pings, e.inFlight = time.Now(), 0, false
		return
	}
	// 容量上限：清最旧
	if len(s.entries) >= 64 {
		var oldestK string
		var oldestT time.Time
		for k, e := range s.entries {
			if oldestK == "" || e.lastSeen.Before(oldestT) {
				oldestK, oldestT = k, e.lastSeen
			}
		}
		if oldestK != "" {
			if e := s.entries[oldestK]; e != nil {
				_ = os.Remove(e.file)
			}
			delete(s.entries, oldestK)
		}
	}
	s.entries[session] = &keepaliveEntry{
		kind: kind, uid: uid, file: fp, lastSeen: time.Now(),
	}
}

// noteIn 更新会话最近真实 in_tokens（保活资格与日志展示用）。
func (s *keepaliveStore) noteIn(session string, inTokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[session]; ok {
		e.lastIn = inTokens
	}
}

// cleanOrphans 删除没有索引的存档文件（重启后索引为空，全清）。
func (s *keepaliveStore) cleanOrphans() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	live := map[string]bool{}
	s.mu.Lock()
	for _, e := range s.entries {
		live[filepath.Base(e.file)] = true
	}
	s.mu.Unlock()
	for _, ent := range entries {
		n := ent.Name()
		if !live[n] && strings.HasSuffix(n, ".json") {
			_ = os.Remove(filepath.Join(s.dir, n))
		}
	}
}

// sweepDue 返回本轮应保活的条目（并标记 inFlight 由调用方清理）。
func (s *keepaliveStore) sweepDue(now time.Time) []*keepaliveEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []*keepaliveEntry
	for _, e := range s.entries {
		if e.inFlight {
			continue
		}
		idle := now.Sub(e.lastSeen)
		if idle < time.Duration(s.cfg.IdleAfterMin)*time.Minute {
			continue
		}
		if idle > time.Duration(s.cfg.WindowMin)*time.Minute {
			_ = os.Remove(e.file)
			delete(s.entries, sessionOf(s.entries, e))
			continue
		}
		if e.pings >= s.cfg.MaxPings {
			continue
		}
		if !e.lastPing.IsZero() && now.Sub(e.lastPing) < time.Duration(s.cfg.PingEveryMin)*time.Minute {
			continue
		}
		e.lastPing, e.inFlight = now, true
		due = append(due, e)
	}
	return due
}

func sessionOf(m map[string]*keepaliveEntry, target *keepaliveEntry) string {
	for k, v := range m {
		if v == target {
			return k
		}
	}
	return ""
}

func (s *keepaliveStore) done(e *keepaliveEntry) {
	s.mu.Lock()
	e.inFlight = false
	if e.pings < 1<<30 {
		e.pings++
	}
	s.mu.Unlock()
}

// rewriteMaxTokensSmall 把 body 的 max_tokens 压到 16（省保活输出费用；
// max_tokens 不参与前缀缓存键，messages 不动）。
func rewriteMaxTokensSmall(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["max_tokens"] = 16
	delete(obj, "max_completion_tokens")
	if out, err := json.Marshal(obj); err == nil {
		return out
	}
	return body
}

// runKeepaliveLoop 后台保活循环（Handler 启动；enabled=false 时为空转）。
func (h *Handler) runKeepaliveLoop(store *keepaliveStore) {
	if store == nil || !store.cfg.Enabled {
		return
	}
	go func() {
		for {
			time.Sleep(2 * time.Minute)
			for _, e := range store.sweepDue(time.Now()) {
				go h.keepalivePing(store, e)
			}
		}
	}()
}

// runtimeKindOf 渠道名字符串 → provider.Kind。
func runtimeKindOf(kind string) provider.Kind { return provider.Kind(kind) }

func providerChatStream(rt *Runtime, acct *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return provider.ChatStreamConv(rt.Upstream, acct, body, extractConversationID(body))
}

// keepalivePing 同账号重放前缀：命中缓存续命 + 输出压到 16 token。
// 失败静默（下一轮再试）；不进 reqlog、不触发质量熔断。
func (h *Handler) keepalivePing(store *keepaliveStore, e *keepaliveEntry) {
	defer store.done(e)
	body, err := os.ReadFile(e.file)
	if err != nil || len(body) == 0 {
		return
	}
	rt := h.cfg.Runtimes[runtimeKindOf(e.kind)]
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return
	}
	acct := rt.Pool.AuthByUID(e.uid)
	if acct == nil {
		return
	}
	if status, ok := rt.Pool.Status(e.uid); ok && (status.Cooling || status.Disabled) {
		return
	}
	ping := rewriteMaxTokensSmall(body)
	rc, status, _, err := providerChatStream(rt, acct, ping)
	if err == nil && status < 400 && rc != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 8192))
		_ = rc.Close()
	}
	log.Printf("cache keepalive ping kind=%s uid=%.8s in≈%dk status=%d err=%v",
		e.kind, e.uid, e.lastIn/1000, status, err != nil)
}
