// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pgstore"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Runtime 是一个平台的一组运行时资源：pool + upstream + 静态模型兜底。
type Runtime struct {
	Kind         provider.Kind
	Pool         *pool.Pool
	Upstream     provider.Upstream
	StaticModels []provider.ModelInfo

	mu       sync.RWMutex
	models   []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// Config handler 依赖。
type Config struct {
	Runtimes map[provider.Kind]*Runtime
	APIKey   string // 空 = 不鉴权

	// WebUI 为内嵌的静态 Web UI 文件系统（go:embed 产物）；非 nil 时挂载到 /
	WebUI fs.FS
	// AttachAPI 由调用方注册管理 API 路由（internal/app 的 HandleAPI）
	AttachAPI func(mux *http.ServeMux)

	// 兼容旧调用方：只传 Pool/Upstream 时等价于只启用 workbuddy。
	Pool     *pool.Pool
	Upstream provider.Upstream

	MaxRotate int
	// RequestLogPath 请求日志持久化文件（jsonl 追加，永不删除）；非空时重启恢复
	RequestLogPath string
	// RequestLogLegacyPath 旧版单 JSON 日志路径；存在且新日志为空时一次性导入
	RequestLogLegacyPath string
	// PGStore PostgreSQL 存储模式（storage.mode=postgres）；非 nil 时请求日志
	// 走 PG（jsonl 停写，历史由 cmd/reqlog-import 一次性导入）
	PGStore *pgstore.Store
	// MaskUpstream 上游特征掩码（默认开）：models 去渠道前缀 / 错误报文中性化
	MaskUpstream *bool
	// Routing 溢流路由（超大上下文改发缓存稳定模型，见 config.Routing）
	Routing      config.Routing
	Keepalive    config.KeepaliveConfig
	KeepaliveDir string
	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration
}

// stickyEntry 粘性路由记录：记录上次路由账号及连续使用次数。
// 不使用 credits（pool 中余额仅在签到/手动刷新时更新，对话后是 stale 数据），
// 改用请求计数：连续请求 maxReqs 次后自动降级换账号。
type stickyEntry struct {
	uid      string
	reqCount int
	maxReqs  int
	lastUsed time.Time
}

// 会话级粘性的淘汰参数：空闲超时 + 条目上限（插入新条目时顺带清扫）。
const (
	stickyIdleTTL    = 2 * time.Hour
	stickyMaxEntries = 4096
)

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	apiMu        sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
	maskUpstream bool

	// sessInMu/sessLastIn 会话 → 上一轮真实 in_tokens（溢流路由判定用；
	// 首轮无记录时按 body 字节估算）。与 sticky 同规格淘汰。
	sessInMu   sync.Mutex
	sessLastIn map[string]int64
	ka         *keepaliveStore // 前缀缓存保活（cache_keepalive.enabled 时启用）
	stickyMu   sync.RWMutex
	sticky     map[string]*stickyEntry // stickyKey(kind, 会话指纹) → stickyEntry
	reqLogs    reqLogStore             // 请求级日志（环形）
}

func NewHandler(cfg Config) *Handler {
	if cfg.Runtimes == nil && cfg.Pool != nil && cfg.Upstream != nil {
		cfg.Runtimes = map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: cfg.Pool, Upstream: cfg.Upstream, StaticModels: WorkBuddyStaticModels()},
		}
	}
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 5
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	maskUpstream := true
	if cfg.MaskUpstream != nil {
		maskUpstream = *cfg.MaskUpstream
	}
	maskUpstreamDefault = maskUpstream
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry), maskUpstream: maskUpstream, sessLastIn: make(map[string]int64)}
	if cfg.Keepalive.Enabled {
		h.ka = newKeepaliveStore(cfg.KeepaliveDir, cfg.Keepalive)
		h.runKeepaliveLoop(h.ka)
	}
	h.reqLogs.path = cfg.RequestLogPath
	h.reqLogs.legacy = cfg.RequestLogLegacyPath
	if cfg.PGStore != nil {
		h.reqLogs.pg = cfg.PGStore
	}
	h.reqLogs.load()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.anthropicMessages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuth(h.anthropicCountTokens))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 无 /v1 前缀的别名：兼容把 Base URL 填成裸域名的客户端
	// （它们会拼出 /chat/completions 而不是 /v1/chat/completions）
	h.mux.HandleFunc("POST /chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /messages", h.withAuth(h.anthropicMessages))
	h.mux.HandleFunc("POST /messages/count_tokens", h.withAuth(h.anthropicCountTokens))
	h.mux.HandleFunc("GET /models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.WebUI != nil {
		h.mux.Handle("/", webCache(http.FileServer(http.FS(cfg.WebUI))))
	}
	if cfg.AttachAPI != nil {
		cfg.AttachAPI(h.mux)
	}
	return h
}

// RequestLogsPage 请求日志永久历史分页（newest-first）。
func (h *Handler) RequestLogsPage(page, size int) ([]ReqLog, int) {
	return h.reqLogs.Page(page, size)
}

// RequestLogsPageFilter 按页 + 过滤读取请求日志（PG 模式服务端过滤；file 模式忽略过滤）。
func (h *Handler) RequestLogsPageFilter(page, size int, f pgstore.ReqLogFilter) ([]ReqLog, int) {
	return h.reqLogs.PageFilter(page, size, f)
}

// PGStorage 是否处于 PostgreSQL 存储模式。
func (h *Handler) PGStorage() bool { return h.reqLogs.pg != nil }

// ReadBodyArchive 读回请求体存档。
func (h *Handler) ReadBodyArchive(name string) ([]byte, bool) {
	return h.reqLogs.ReadBodyArchive(name)
}

// SetBodyArchiveDir 设置请求体存档目录（main 启动时调用）。
func (h *Handler) SetBodyArchiveDir(dir string) { h.reqLogs.SetBodiesDir(dir) }

// Close 释放持有的资源（请求日志 journal 句柄）；进程退出时调用。
func (h *Handler) Close() { h.reqLogs.close() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// stickyKey 粘性路由 key：按渠道 + 会话指纹分组。
// session 为空（请求无任何会话标识）时退化为渠道级全局键——旧行为。
func (h *Handler) stickyKey(kind provider.Kind, session string) string {
	if session == "" {
		return kind.String()
	}
	return kind.String() + "|" + session
}

// pickWithSticky 粘性路由选择账号（无会话标识的兼容入口）。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用状态
//   - 该模型处于 6004 限额冷却
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级为按模型选号并重置粘性记录。
//
// 按【会话】而非渠道全局粘（2026-09-29）：prompt_cache_key 内含账号 UID，
// 换号即换键，上游前缀缓存全冷。此前渠道全局计数每 50 个请求换一次号
// （高峰期中位 188s 一次），一次换号把所有活跃会话的缓存一起清零——
// 48h 数据：639 次换号，698 个零缓存大请求中 647 个由换号导致。
// 会话级粘性把换号的影响面收窄到单个会话，且每个会话保底 50 次热缓存请求。
func (h *Handler) pickWithSticky(rt *Runtime) *auth.Auth {
	return h.pickWithStickyForModel(rt, "", "", nil)
}

func (h *Handler) pickWithStickyForModel(rt *Runtime, model, session string, body []byte) *auth.Auth {
	const defaultMaxReqs = 50

	key := h.stickyKey(rt.Kind, session)
	h.stickyMu.RLock()
	sticky := h.sticky[key]
	h.stickyMu.RUnlock()

	if sticky != nil && sticky.uid != "" && sticky.reqCount < sticky.maxReqs {
		acct := rt.Pool.AuthByUID(sticky.uid)
		if acct != nil {
			status, ok := rt.Pool.Status(sticky.uid)
			if ok && !status.Cooling && !status.Disabled && !rt.Pool.CooledForModel(sticky.uid, model) {
				h.stickyMu.Lock()
				sticky.lastUsed = time.Now()
				h.stickyMu.Unlock()
				log.Printf("sticky route platform=%s uid=%s model=%s count=%d/%d",
					rt.Kind, sticky.uid, model, sticky.reqCount, sticky.maxReqs)
				return acct
			}
		}
	}

	acct := h.pickFresh(rt, nil, model, body)
	if acct == nil {
		return nil
	}

	h.stickyMu.Lock()
	h.evictStickyLocked()
	h.sticky[key] = &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs, lastUsed: time.Now()}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s model=%s maxReqs=%d", rt.Kind, acct.UID, model, defaultMaxReqs)
	return acct
}

// evictStickyLocked 清扫粘性记录（调用方持锁）：先删空闲超时条目，
// 仍超上限则按 lastUsed 淘汰最旧。插入新条目时调用，摊销 O(n)。
func (h *Handler) evictStickyLocked() {
	if len(h.sticky) < stickyMaxEntries/2 {
		return
	}
	now := time.Now()
	for k, e := range h.sticky {
		if now.Sub(e.lastUsed) > stickyIdleTTL {
			delete(h.sticky, k)
		}
	}
	if len(h.sticky) < stickyMaxEntries {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(h.sticky))
	for k, e := range h.sticky {
		all = append(all, kv{k, e.lastUsed})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, e := range all[:len(all)-stickyMaxEntries+1] {
		delete(h.sticky, e.k)
	}
}

// stickySuccess 粘性路由成功：递增请求计数。
func (h *Handler) stickySuccess(rt *Runtime, session string) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	if e := h.sticky[h.stickyKey(rt.Kind, session)]; e != nil {
		e.reqCount++
	}
}

// stickyClear 粘性路由失败（错误/冷却）：清除该会话的粘性记录，下次请求强制重新选号。
// 只影响传入会话；其他会话的粘性与缓存不受牵连。
func (h *Handler) stickyClear(rt *Runtime, session string) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	delete(h.sticky, h.stickyKey(rt.Kind, session))
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := h.currentAPIKey(); key != "" {
			authz := r.Header.Get("Authorization")
			// OpenAI 惯例 Authorization: Bearer <key>；Anthropic 惯例 x-api-key: <key>
			apiKey := r.Header.Get("x-api-key")
			if !strings.HasPrefix(authz, "Bearer ") {
				// 无 Bearer 时回退 x-api-key（Anthropic SDK 用法）
				if apiKey == "" {
					writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
					return
				}
				authz = "Bearer " + apiKey
			}
			if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authz, "Bearer ")), []byte(key)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) SetAPIKey(key string) { h.apiMu.Lock(); defer h.apiMu.Unlock(); h.cfg.APIKey = key }
func (h *Handler) currentAPIKey() string {
	h.apiMu.RLock()
	defer h.apiMu.RUnlock()
	return h.cfg.APIKey
}
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	accounts := map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		accounts[k.String()] = rt.Pool.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

var workbuddyStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2", ContextWindow: 131072}, {ID: "glm-5.1", ContextWindow: 131072}, {ID: "glm-5v-turbo", ContextWindow: 131072},
	{ID: "kimi-k2.7", ContextWindow: 131072}, {ID: "minimax-m3", ContextWindow: 131072}, {ID: "hy3", ContextWindow: 131072},
	{ID: "hy3-preview", ContextWindow: 131072}, {ID: "hy3-preview-agent", ContextWindow: 131072},
	{ID: "deepseek-v4-pro", ContextWindow: 131072}, {ID: "deepseek-v4-flash", ContextWindow: 131072},
}

var traeworkStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2"}, {ID: "glm-5-turbo"}, {ID: "glm-5"}, {ID: "DeepSeek-V4-Pro"}, {ID: "DeepSeek-V4-Flash"},
	{ID: "kimi-k2.6"}, {ID: "kimi-k2.7-code"}, {ID: "minimax-m3"}, {ID: "qwen3-coder"}, {ID: "Doubao-Seed-2.1-Pro"},
}

// dynamicModelsCache 保留给旧测试/旧单平台语义；实际多平台缓存放在 Runtime 内。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

func (h *Handler) modelList() []map[string]any {
	out := []map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		if rt.Pool == nil || len(rt.Pool.List()) == 0 { // 只暴露已接入账号的平台
			continue
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		for _, mi := range infos {
			id := k.String() + "/" + mi.ID
			ownedBy := k.String()
			// 上游特征掩码：对外去渠道前缀、owned_by 中性化（渠道身份不外露）。
			// 路由层不变：带前缀的旧模型名仍可调用（runtimeForModel 兼容两种）。
			if h.maskUpstream {
				if _, bare, ok := strings.Cut(id, "/"); ok {
					id = bare
				}
				// 家族化 owned_by（官方 /models 口径：deepseek/moonshot/zhipu）
				ownedBy = map[string]string{"deepseek": "deepseek", "kimi": "moonshot", "glm": "z-ai"}[maskFamily(id)]
				if ownedBy == "" {
					ownedBy = "system"
				}
				// deepseek 家族：id 用官方名（官方对别名也回显官方名）
				id = officialDisplayName(id)
			}
			entry := map[string]any{"id": id, "object": "model", "created": 1753600000, "owned_by": ownedBy}
			if h.maskUpstream && ownedBy == "deepseek" {
				// 官方 /models 条目形态：name + context_window/max_output_tokens 键名
				entry["name"] = strings.ToUpper(strings.ReplaceAll(id, "-", " "))
				delete(entry, "created")
			}
			if h.maskUpstream && (ownedBy == "z-ai" || ownedBy == "moonshot") {
				// 官方极简条目（bigmodel 实测 / moonshot 文档）：四键
				delete(entry, "context_length")
				delete(entry, "max_output_tokens")
			}
			if h.maskUpstream && ownedBy == "z-ai" {
				// 官方 glm /models 条目极简：{id, object, created, owned_by}
				delete(entry, "context_length")
				delete(entry, "max_output_tokens")
			}
			if mi.ContextWindow > 0 {
				entry["context_length"] = mi.ContextWindow
			}
			if mi.MaxTokens > 0 {
				entry["max_output_tokens"] = mi.MaxTokens
			}
			out = append(out, entry)
			if v, ok := thinkVariant(entry); ok {
				out = append(out, v)
			}
			if v, ok := visionVariant(entry); ok {
				out = append(out, v)
			}
			// 官方名别名（modelAliases 的反向暴露）：客户端按官方名判定
			// 视觉能力（如 Kelivo 认 deepseek-flash），列表里没有则无法选用。
			for alias, target := range modelAliases {
				if strings.HasSuffix(id, "/"+target) || id == target {
					av := make(map[string]any, len(entry))
					for k, val := range entry {
						av[k] = val
					}
					av["id"] = strings.TrimSuffix(id, target) + alias
					out = append(out, av)
					break
				}
			}
		}
	}
	return out
}

// thinkVariant 克隆模型条目并追加 @think 后缀。
// /v1/models 必须列出 @think 变体，中转站（new-api 等）按模型名路由，
// 列表里没有的名字直接报 "No available upstream provider"，请求到不了网关。
// auto 路由别名本身不对应具体模型，不生成变体。
func thinkVariant(entry map[string]any) (map[string]any, bool) {
	id, _ := entry["id"].(string)
	if id == "" || strings.HasSuffix(id, "@think") || strings.HasSuffix(id, "/auto") {
		return nil, false
	}
	v := make(map[string]any, len(entry))
	for k, val := range entry {
		v[k] = val
	}
	v["id"] = id + "@think"
	return v, true
}

// visionModelSource 判断模型是否视觉可用且名字不含客户端可识别的视觉字样。
// 客户端（Cherry Studio / Open WebUI 等）按名字启发式判定视觉能力，缺
// vl/vision 字样的视觉模型在聊天里直接不发图；对这类模型暴露 -vl 别名。
func visionModelSource(id string) bool {
	lower := strings.ToLower(id)
	if strings.HasSuffix(lower, "-vl") || strings.HasSuffix(lower, "-vision") {
		return false // 已是别名，不再叠加
	}
	vision := strings.Contains(lower, "vl") || strings.Contains(lower, "vision") ||
		strings.Contains(lower, "5v") || strings.Contains(lower, "4o") ||
		strings.Contains(lower, "deepseek-v4.1")
	nameSignalsVision := strings.Contains(lower, "vl") || strings.Contains(lower, "vision") ||
		strings.Contains(lower, "4o")
	return vision && !nameSignalsVision
}

// visionVariant 克隆视觉模型条目并追加 -vl 后缀（客户端视觉放行别名，
// 入口侧由 stripThinkSuffix 剥离后路由到原模型）。
func visionVariant(entry map[string]any) (map[string]any, bool) {
	id, _ := entry["id"].(string)
	if !visionModelSource(id) {
		return nil, false
	}
	v := make(map[string]any, len(entry))
	for k, val := range entry {
		v[k] = val
	}
	v["id"] = id + "-vl"
	return v, true
}

func (h *Handler) fetchRuntimeModels(rt *Runtime) []provider.ModelInfo {
	if rt.Kind == provider.WorkBuddy { // 兼容旧单平台缓存观察点
		dynamicModelsCache.RLock()
		if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
			out := dynamicModelsCache.ids
			dynamicModelsCache.RUnlock()
			return out
		}
		if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
			dynamicModelsCache.RUnlock()
			return nil
		}
		dynamicModelsCache.RUnlock()
	}
	rt.mu.RLock()
	if len(rt.models) > 0 && time.Since(rt.fetched) < dynamicModelsTTL {
		out := rt.models
		rt.mu.RUnlock()
		return out
	}
	if rt.Kind != provider.WorkBuddy && !rt.lastFail.IsZero() && time.Since(rt.lastFail) < modelsFetchFailCooldown {
		rt.mu.RUnlock()
		return nil
	}
	rt.mu.RUnlock()
	acct := rt.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := rt.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		now := time.Now()
		rt.mu.Lock()
		rt.lastFail = now
		rt.mu.Unlock()
		if rt.Kind == provider.WorkBuddy {
			dynamicModelsCache.Lock()
			dynamicModelsCache.lastFail = now
			dynamicModelsCache.Unlock()
		}
		return nil
	}
	now := time.Now()
	rt.mu.Lock()
	rt.models = infos
	rt.fetched = now
	rt.lastFail = time.Time{}
	rt.mu.Unlock()
	if rt.Kind == provider.WorkBuddy { // 兼容旧测试观察点
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = now
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	const chatBodyLimit = 8 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, chatBodyLimit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > chatBodyLimit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body exceeds 8MiB limit")
		return
	}
	var peek struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	if len(peek.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages must be a non-empty array")
		return
	}
	// @think 后缀：推理内容包装为 <think>…</think> 正文标签输出。
	// 面向只从正文标签提取思考的客户端（ZCode OpenAI 兼容模式等），
	// 标签形态可穿透任意中转站。例：kimi-k3-1@think / workbuddy/glm-5.3@think。
	requestedModel := peek.Model
	var thinkTag bool
	peek.Model, thinkTag = stripThinkSuffix(peek.Model)
	// 裸 deepseek 默认 <think> 包装（网关强制开思考 + reasoning_content 非 OpenAI
	// 标准字段，思维链会漏进不认识它的客户端正文）。@nothink 后缀显式关闭。
	if strings.HasPrefix(strings.ToLower(peek.Model), "deepseek") && !strings.HasSuffix(peek.Model, "@nothink") {
		thinkTag = true
	}
	peek.Model = strings.TrimSuffix(peek.Model, "@nothink")
	rt, model, err := h.runtimeForModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 派发期心跳：长上下文冷缓存/上游拥塞时选号+等响应头可达数分钟，
	// 期间零字节会被下游读超时掐断（nginx 499 → 用户侧 502）。
	dw := w
	var hb *sseHeartbeat
	if peek.Stream {
		hb = startSSEHeartbeat(w, openAIErrFrame)
		dw = hb
	}
	session := upstream.ConversationFingerprint(body)
	rc, uid, ok := h.dispatchChat(rt, t0, requestedModel, body, dw, session)
	if hb != nil {
		w = hb.Stop()
	}
	if !ok {
		return
	}
	defer rc.Close()
	if h.ka != nil {
		h.ka.track(session, rt.Kind.String(), uid, body)
	}
	bodyFile := h.reqLogs.SaveBodyArchive(body)
	body = nil
	fbw := newFirstByteWriter(w, t0)
	if peek.Stream {
		// SSE 心跳：立刻下发注释行并冲刷，防下游 ~1s 首字节超时掐流
		//（实测 monoize 类客户端 1.01s cancel，2h 掐 716 个请求；注释行是
		// 合法 SSE，所有解析器忽略）。响应头与 Stream() 输出保持一致。
		hdr := fbw.Header()
		hdr.Set("Content-Type", "text/event-stream")
		hdr.Set("Cache-Control", "no-cache")
		hdr.Set("Connection", "keep-alive")
		hdr.Set("X-Accel-Buffering", "no")
		_, _ = io.WriteString(fbw, ": keepalive\n\n")
		fbw.Flush()
		tee := &usageTee{}
		var out http.ResponseWriter = fbw
		var ttw *thinkTagWriter
		if thinkTag {
			ttw = newThinkTagWriter(fbw)
			out = ttw
		}
		// 响应指纹掩码（最外层）：id→chatcmpl-、model→请求展示名、
		// usage 剥非标字段（credit/prompt_cache_* 等）。面板统计走 tee，先于掩码。
		if h.maskUpstream {
			mw := newSSEMaskWriter(out, displayNameFor(requestedModel))
			out = mw
		}
		err := rt.Upstream.Stream(out, &teeReadCloser{rc: rc, w: tee})
		if ttw != nil {
			ttw.Finish()
		}
		// 上游中途断流（看门狗掐静默/传输故障）时，客户端只见到半截 SSE——
		// 大多显示为"断连"。补发标准错误帧让客户端拿到显式失败并可重试，
		// 而非静默 EOF；仅在上游未发过 finish/[DONE] 时补（正常收尾不重复）。
		if err != nil && !tee.sawDone() {
			log.Printf("stream interrupted mid-flight platform=%s uid=%s model=%s err=%v (sending error frame)", rt.Kind, uid, requestedModel, err)
			errFrame := "data: {\"error\":{\"message\":\"upstream stream interrupted: " + strings.ReplaceAll(err.Error(), "\"", "'") + "\",\"type\":\"api_error\"}}\n\ndata: [DONE]\n\n"
			_, _ = io.WriteString(out, errFrame)
			if fl, ok := out.(http.Flusher); ok {
				fl.Flush()
			}
		}
		h.finishReqLogFile(t0, requestedModel, rt.Kind.String(), uid, http.StatusOK, true, fbw.ttfb(), tee.snapshot(), bodyFile)
		if u := tee.snapshot(); u != nil {
			h.noteSessionIn(session, num(u["prompt_tokens"]))
			if h.ka != nil {
				h.ka.noteIn(session, num(u["prompt_tokens"]))
			}
		}
		h.noteQuality(rt, uid, tee)
		_ = err
		return
	}
	resp, err := rt.Upstream.Aggregate(rc)
	if err != nil {
		writeOpenAIError(fbw, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	if thinkTag {
		wrapThinkTag(resp)
	}
	usage, _ := resp["usage"].(map[string]any)
	h.finishReqLogFile(t0, requestedModel, rt.Kind.String(), uid, http.StatusOK, false, 0, usage, bodyFile)
	h.noteSessionIn(session, num(usage["prompt_tokens"]))
	if h.ka != nil {
		h.ka.noteIn(session, num(usage["prompt_tokens"]))
	}
	h.noteQualityMap(rt, uid, usage, respToolCalled(resp))
	if h.maskUpstream {
		maskAggregateResp(resp, displayNameFor(requestedModel))
	}
	writeJSON(fbw, http.StatusOK, resp)
}

// heavyBody 重任务判定：出站 body 超过 150KB（约 5 万 token）。
func heavyBody(body []byte) bool { return len(body) > 150*1024 }

// pickFresh 换号选号：重任务时优先避开质量降级中的账号（全员降级则退化普通选号）。
func (h *Handler) pickFresh(rt *Runtime, tried map[string]bool, model string, body []byte) *auth.Auth {
	if heavyBody(body) {
		return rt.Pool.PickExcludingForModelHeavy(tried, model)
	}
	return rt.Pool.PickExcludingForModel(tried, model)
}

// noteQuality 流式收尾的质量降级判定：大输入 200 完成，但输出 <1000 tokens、
// 无 tool_calls、finish 不是 tool_calls——上游对该号静默降质（思考半截直出、
// 计划模式断流）。记一次质量降级：该号 30min 内避开重任务，10min 内 3 次则
// 整体冷却 10min 逼粘性会话换号。
func (h *Handler) noteQuality(rt *Runtime, uid string, tee *usageTee) {
	if uid == "" {
		return
	}
	u := tee.snapshot()
	if u == nil {
		return
	}
	inTok, _ := toInt64(u["prompt_tokens"])
	outTok, _ := toInt64(u["completion_tokens"])
	if inTok < 50_000 || outTok >= 1_000 {
		return
	}
	if tee.toolSeen() || tee.finishReason() == "tool_calls" {
		return
	}
	rt.Pool.NoteQualityDegraded(uid)
	log.Printf("quality degraded platform=%s uid=%s in=%d out=%d finish=%s -> heavy-avoid 30m",
		rt.Kind, uid, inTok, outTok, tee.finishReason())
}

// noteQualityMap 非流式版本。
func (h *Handler) noteQualityMap(rt *Runtime, uid string, usage map[string]any, toolCalled bool) {
	if uid == "" || usage == nil {
		return
	}
	inTok, _ := toInt64(usage["prompt_tokens"])
	outTok, _ := toInt64(usage["completion_tokens"])
	if inTok < 50_000 || outTok >= 1_000 {
		return
	}
	if toolCalled {
		return
	}
	rt.Pool.NoteQualityDegraded(uid)
	log.Printf("quality degraded platform=%s uid=%s in=%d out=%d (non-stream) -> heavy-avoid 30m",
		rt.Kind, uid, inTok, outTok)
}

// respToolCalled 非流式响应里是否有 tool_calls。
func respToolCalled(resp map[string]any) bool {
	choices, _ := resp["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		msg, _ := c["message"].(map[string]any)
		if msg == nil {
			continue
		}
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			return true
		}
	}
	return false
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// wrapThinkTag 非流式：把 message 的推理内容并入正文 <think> 标签，删除推理字段。
func wrapThinkTag(resp map[string]any) {
	choices, _ := resp["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		msg, _ := c["message"].(map[string]any)
		if msg == nil {
			continue
		}
		reasoning, _ := msg["reasoning_content"].(string)
		if reasoning == "" {
			reasoning, _ = msg["reasoning"].(string)
		}
		if reasoning == "" {
			continue
		}
		content, _ := msg["content"].(string)
		msg["content"] = "<think>" + reasoning + "</think>\n" + content
		delete(msg, "reasoning_content")
		delete(msg, "reasoning")
	}
}

// dispatchChat 选号（粘性/刷新/换号重试）并打开上游流。
// 失败路径自行把错误响应写入 w 并返回 ok=false；
// 成功返回需调用方 Close 的 rc 与命中账号 uid。
func (h *Handler) dispatchChat(rt *Runtime, t0 time.Time, model string, body []byte, w http.ResponseWriter, session string) (io.ReadCloser, string, bool) {
	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	var lastBody []byte                   // 最后一次上游错误（轮转耗尽时按原状态透传）
	routeModel := extractModel(body)      // 出站裸模型名（6004 模型级冷却按它画界）
	convID := extractConversationID(body) // 会话标识：prompt_cache_key 注入源
	// 超大上下文溢流路由（routing.overflow）：flash 类大上下文缓存淘汰快，
	// 超阈值改发缓存稳定的指定模型（响应仍回显请求名）。
	body, routeModel, model = h.applyOverflow(body, routeModel, model, session)
	// 全池模型冷却快速失败：所有健康账号都被 6004 冷却时，轮转必然空转
	// （每号一次上游 429 往返，实测一圈 600s+）。立即以 429 回绝并带最早
	// 重置时刻，让客户端显式重试而非长时间挂死后断连。
	if routeModel != "" && rt.Pool.CooledForModelAll(routeModel) {
		msg := fmt.Sprintf("all accounts rate-limited for model %s; retry later", routeModel)
		if until := rt.Pool.ModelCoolUntil(routeModel); !until.IsZero() {
			msg += " (reset " + until.Format(time.RFC3339) + ")"
		}
		if h.maskUpstream {
			// 掩码模式：官方措辞（"accounts"=多号池特征不外露），细节进日志
			log.Printf("masked 429 detail: %s", msg)
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("Rate limit exceeded for model %s. Please try again later.", routeModel))
		} else {
			writeOpenAIError(w, http.StatusTooManyRequests, "model_rate_limited", msg)
		}
		h.finishReqLog(t0, model, rt.Kind.String(), "", http.StatusTooManyRequests, false, 0, nil, body)
		return nil, "", false
	}
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickWithStickyForModel(rt, routeModel, session, body)
		if acct == nil {
			break
		}
		if tried[acct.UID] || rt.Pool.CooledForModel(acct.UID, routeModel) {
			h.stickyClear(rt, session)
			acct = h.pickFresh(rt, tried, routeModel, body)
			if acct == nil {
				break
			}
		}
		tried[acct.UID] = true
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			log.Printf("refresh start platform=%s uid=%s reason=request", rt.Kind, acct.UID)
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				log.Printf("refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
				lastErr = err
				h.stickyClear(rt, session)
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					// 连续 N 次才禁用：单次 12153 多为抖动误报（上游实测误杀率 100%）
					if rt.Pool.NoteSessionDead(acct.UID) {
						log.Printf("session dead disable platform=%s uid=%s consecutive=%d", rt.Kind, acct.UID, pool.SessionDeadThreshold())
					} else {
						rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh session dead (transient?)")
					}
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
			}
			log.Printf("refresh success platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
		}
		rc, status, respBody, terr := provider.ChatStreamConv(rt.Upstream, acct, body, convID)
		if terr != nil {
			lastErr = terr
			h.stickyClear(rt, session)
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			kind := rt.Upstream.Classify(status, string(respBody))
			// 11102「该后端无此模型」：确定性答复，非账号故障——按 (账号, 模型)
			// 写指数退避负缓存（6h 起 ×2 封顶 24h），同请求立刻换号。
			// provider.IsModelAbsent 对非 workbuddy 渠道 body 不命中，零影响。
			if routeModel != "" && provider.IsModelAbsent(string(respBody)) {
				rt.Pool.BlockModelBackoff(acct.UID, routeModel, "11102 model absent")
				log.Printf("model absent rotate platform=%s uid=%s model=%s (11102)",
					rt.Kind, acct.UID, routeModel)
				h.stickyClear(rt, session)
				lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
				lastStatus, lastBody = status, respBody
				continue
			}
			// 账号侧错误（限流/欠费/会话死/上游 5xx）：罚号并换号重试。
			// 客户端侧错误（400 参数/404 模型名）：换号无意义，且可能来自
			// 中转站模型探活——若罚号，单个客户端即可把整池打入冷却雪崩。
			switch kind {
			case provider.ErrHardCredit:
				h.stickyClear(rt, session)
				rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额/权益不足")
			case provider.ErrSoftRate:
				// 6004 模型级限流：只对该模型冷却，账号对其他模型立即可用。
				// 同请求立刻换号，不把 429 丢回客户端（避免会话断链）。
				if provider.IsModelRateLimit(string(respBody)) {
					resetAt, ok := provider.ParseSoftRateReset(string(respBody))
					if !ok {
						resetAt = time.Now().Add(h.cfg.SoftCooldown)
					}
					rt.Pool.CooldownSoftForModel(acct.UID, resetAt, routeModel, "6004 model rate limit")
					log.Printf("model rate limit rotate platform=%s uid=%s model=%s reset=%s",
						rt.Kind, acct.UID, routeModel, resetAt.Format(time.RFC3339))
					h.stickyClear(rt, session)
					lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
					continue
				}
				h.stickyClear(rt, session)
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			case provider.ErrSessionDead:
				h.stickyClear(rt, session)
				if rt.Pool.NoteSessionDead(acct.UID) {
					log.Printf("session dead disable platform=%s uid=%s consecutive=%d", rt.Kind, acct.UID, pool.SessionDeadThreshold())
				}
			case provider.ErrServer:
				h.stickyClear(rt, session)
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			default: // ErrClient / ErrNotFound：请求本身被上游拒绝
				// 不清粘性：11140 内容审核 / 11133 参数错是请求内容问题，
				// 账号本身健康，会话下一轮继续用同号可保住上游前缀缓存。
				// 11140 内容审核熔断：短窗多次说明持续发违规内容，停号止损防整号拉黑
				if status == http.StatusForbidden && strings.Contains(string(respBody), "11140") {
					if rt.Pool.NoteContentBlock(acct.UID) {
						log.Printf("content-block circuit breaker: disable platform=%s uid=%s (11140 x3/h)", rt.Kind, acct.UID)
					}
				}
				out := respBody
				outStatus := status
				if h.maskUpstream && !upstream.IsContentPolicyBlock(string(respBody)) {
					// 上游特征掩码：原始报文写日志（排障），对外发中性 OpenAI 错误
					log.Printf("upstream error passthrough masked platform=%s uid=%s status=%d body=%.400s",
						rt.Kind, acct.UID, status, respBody)
					out = maskUpstreamErrorBody(status, model)
				}
				if upstream.IsContentPolicyBlock(string(respBody)) {
					// 内容审核：回网关防火墙文案，不透传上游 body（账号/业务 code）。
					// 状态码用 400 不用 403：monoize 类中转把 403 当渠道永久故障，
					// 一次命中就把整个上游熔断 60s，所有用户随之报 no provider。
					kw := upstream.ContentBlockKeyword(string(respBody))
					out = upstream.FirewallHitResponse(kw)
					outStatus = http.StatusBadRequest
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(outStatus)
				_, _ = w.Write(out)
				h.finishReqLog(t0, model, rt.Kind.String(), acct.UID, outStatus, false, 0, nil, body)
				return nil, "", false
			}
			lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
			lastStatus, lastBody = status, respBody
			continue
		}
		// 卡流检测：流打开后 firstContentTimeout 内未出现首个内容块
		// （content/reasoning_content/tool_calls），视为该账号/模型卡死，
		// 关流换号重试，避免把死流耗到客户端超时。
		// 探测阶段消费的字节（含携带 tool_call id/name 的首片）必须回放。
		rc = newIdleWatchdog(rc, streamIdleTimeout)
		brc := &bufferedStream{br: bufio.NewReaderSize(rc, 64*1024), rc: rc}
		var sink bytes.Buffer
		progress := make(chan error, 1)
		go func() { progress <- waitFirstContent(brc.br, &sink) }()
		select {
		case perr := <-progress:
			if perr != nil && perr != io.EOF {
				lastErr = perr
				h.stickyClear(rt, session)
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				_ = rc.Close()
				continue
			}
		case <-time.After(firstContentTimeout):
			log.Printf("chat stall detected platform=%s uid=%s model? first content > %v, rotate", rt.Kind, acct.UID, firstContentTimeout)
			lastErr = fmt.Errorf("first content chunk timeout > %v", firstContentTimeout)
			h.stickyClear(rt, session)
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			_ = rc.Close()
			continue
		}
		brc.prefix = sink.Bytes()
		rt.Pool.NoteSuccess(acct.UID)
		// 11102 负缓存半开语义：成功即清（该模型实测又通了，避让立即解除）。
		if routeModel != "" {
			rt.Pool.BlockModelClear(acct.UID, routeModel)
		}
		h.stickySuccess(rt, session)
		return brc, acct.UID, true
	}
	if lastBody != nil {
		out := lastBody
		outStatus := lastStatus
		if h.maskUpstream && !upstream.IsContentPolicyBlock(string(lastBody)) {
			log.Printf("upstream error passthrough masked (rotate-exhausted) status=%d body=%.400s", lastStatus, lastBody)
			out = maskUpstreamErrorBody(lastStatus, model)
		}
		if upstream.IsContentPolicyBlock(string(lastBody)) {
			out = upstream.FirewallHitResponse(upstream.ContentBlockKeyword(string(lastBody)))
			outStatus = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(outStatus)
		_, _ = w.Write(out)
		h.finishReqLog(t0, model, rt.Kind.String(), "", outStatus, false, 0, nil, body)
		return nil, "", false
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	if h.maskUpstream {
		log.Printf("masked 503 detail: %s", msg)
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable",
			"The service is temporarily overloaded. Please try again later.")
	} else {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	}
	h.finishReqLog(t0, model, rt.Kind.String(), "", http.StatusServiceUnavailable, false, 0, nil, body)
	return nil, "", false
}

// applyOverflow 超大上下文溢流：命中规则（上一轮真实 in_tokens 或首轮字节
// 估算 ~2.6B/token 超过 over_tokens）时改写 body 的 model 字段并返回目标模型。
// 返回值：(body, 出站模型, 展示模型)。未命中规则原样返回。
func (h *Handler) applyOverflow(body []byte, routeModel, displayModel, session string) ([]byte, string, string) {
	rule, ok := h.cfg.Routing.Overflow[displayModel]
	if !ok || rule.OverTokens <= 0 || rule.To == "" || rule.To == routeModel {
		return body, routeModel, displayModel
	}
	last := int64(0)
	if session != "" {
		h.sessInMu.Lock()
		last = h.sessLastIn[session]
		h.sessInMu.Unlock()
	}
	est := int64(float64(len(body)) / 2.6)
	if last < est {
		last = est
	}
	if last <= int64(rule.OverTokens) {
		return body, routeModel, displayModel
	}
	nb, err := rewriteModel(body, rule.To)
	if err != nil {
		return body, routeModel, displayModel
	}
	log.Printf("overflow route %s -> %s (last_in=%d threshold=%d session=%.16s)", displayModel, rule.To, last, rule.OverTokens, session)
	return nb, rule.To, displayModel
}

// noteSessionIn 记录会话上一轮真实 in_tokens（溢流判定；容量与 sticky 同规格）。
func (h *Handler) noteSessionIn(session string, inTokens int64) {
	if session == "" || inTokens <= 0 {
		return
	}
	h.sessInMu.Lock()
	defer h.sessInMu.Unlock()
	if len(h.sessLastIn) >= 4096 {
		h.sessLastIn = make(map[string]int64)
	}
	h.sessLastIn[session] = inTokens
}

// modelAliases 客户端生态已知官方名 → 网关实际模型名。部分客户端（如
// Kelivo）按模型名硬编码判定视觉能力：DeepSeek V4.1 Flash 的官方名是
// deepseek-flash（Kelivo 源码 _isDeepSeekVisionModel 只认 deepseek-flash /
// deepseek-v4-flash），上游动态列表暴露的却是 deepseek-v4.1-flash——名字
// 不匹配时客户端直接不把图片放进请求（canImageInput=false）。暴露并路由
// 官方名别名让这类客户端放行图片。
var modelAliases = map[string]string{
	"deepseek-flash": "deepseek-v4.1-flash",
}

// resolveModelAlias 模型名别名归一（无别名原样返回）。
func resolveModelAlias(name string) string {
	if real, ok := modelAliases[name]; ok {
		return real
	}
	return name
}

// maskUpstreamDefault 进程级掩码默认（runtimeForModel 为自由函数用；由
// NewHandler 按 config 同步，与 Handler.maskUpstream 一致）。
var maskUpstreamDefault = true

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	model = strings.TrimSpace(model)
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		kind := provider.Kind(parts[0])
		rt := h.cfg.Runtimes[kind]
		if rt == nil || rt.Pool == nil || rt.Upstream == nil {
			return nil, "", fmt.Errorf("provider %q is not configured", kind)
		}
		if len(rt.Pool.List()) == 0 {
			return nil, "", fmt.Errorf("provider %q has no account", kind)
		}
		return rt, resolveModelAlias(parts[1]), nil
	}

	// 裸模型名（无渠道前缀）：在已接入账号的渠道里自动解析。
	// 优先按模型名精确命中（动态缓存 → 静态兜底）；无命中且仅有一个活跃渠道时按该渠道处理。
	model = resolveModelAlias(model)
	var hit *Runtime
	var fallback *Runtime
	active := 0
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		if rt.Pool == nil || rt.Upstream == nil || len(rt.Pool.List()) == 0 {
			continue
		}
		active++
		fallback = rt
		if hit != nil {
			continue
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		for _, mi := range infos {
			if mi.ID == model {
				hit = rt
				break
			}
		}
	}
	if hit != nil {
		return hit, model, nil
	}
	if active == 1 && fallback != nil {
		return fallback, model, nil
	}
	if maskUpstreamDefault {
		// 掩码模式：官方式文案（渠道名列表=多渠道中转特征，不外露）
		return nil, "", fmt.Errorf("The model %q does not exist or you do not have access to it.", model)
	}
	return nil, "", fmt.Errorf("model %q not found; use explicit prefix: workbuddy/<model> / traework/<model> / qoder/<model> / qclaw/<model>", model)
}

// runtimeForModelWithFallback 先按原样解析；解析出的裸模型名未在渠道模型表
// 命中（如 claude-* 等 Anthropic 客户端模型名）时改用 fallback 模型。
func (h *Handler) runtimeForModelWithFallback(model, fallback string) (*Runtime, string, error) {
	rt, m, err := h.runtimeForModel(model)
	if err == nil {
		if m != model || h.modelKnown(rt, m) {
			return rt, m, nil
		}
		// 单渠道兜底放行的未知裸模型名：上游大概率不认，走 fallback
		err = fmt.Errorf("model %q not in channel model list", m)
	}
	if fallback != "" && fallback != model {
		if rt2, m2, err2 := h.runtimeForModel(fallback); err2 == nil {
			log.Printf("anthropic model %q fallback -> %s", model, m2)
			return rt2, m2, nil
		}
	}
	return nil, "", err
}

// modelKnown 判断模型是否在渠道模型表（动态缓存 → 静态兜底）中。
func (h *Handler) modelKnown(rt *Runtime, model string) bool {
	infos := h.fetchRuntimeModels(rt)
	if len(infos) == 0 {
		infos = rt.StaticModels
	}
	for _, mi := range infos {
		if mi.ID == model {
			return true
		}
	}
	return false
}

// extractModel 从请求体提取出站模型名（rewriteModel 之后的裸名）。
func extractModel(body []byte) string {
	var s struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &s)
	return s.Model
}

// extractConversationID 从请求体提取会话标识（metadata.conversation_id /
// 顶层 conversation_id / conversationId 任一），供 prompt_cache_key 注入。
func extractConversationID(body []byte) string {
	var s struct {
		Metadata struct {
			ConversationID string `json:"conversation_id"`
		} `json:"metadata"`
		ConversationID  string `json:"conversation_id"`
		ConversationID2 string `json:"conversationId"`
	}
	_ = json.Unmarshal(body, &s)
	for _, v := range []string{s.Metadata.ConversationID, s.ConversationID, s.ConversationID2} {
		if v != "" {
			return v
		}
	}
	return ""
}

func rewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj["model"] = model
	return json.Marshal(obj)
}

func (h *Handler) runtimeKinds() []provider.Kind {
	ks := make([]provider.Kind, 0, len(h.cfg.Runtimes))
	for k := range h.cfg.Runtimes {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

// webCache SPA 静态资源缓存策略：
//   - index.html → no-cache：每次回源校验，部署后浏览器必须立刻拿到新资源引用
//     （缺省无 Cache-Control 时浏览器走启发式缓存，旧 HTML 一直不刷新——
//     引用已删除的旧 hash JS，表现为"看不到新前端"/白屏）
//   - assets/*（文件名带构建 hash）→ 一年 immutable：内容变文件名必变，可永存
func webCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || p == "index.html" {
			// 历史版本给首页带 ?v=<ts> 做缓存穿透，旧地址在浏览器侧被启发式
			// 缓存后无法失效——301 到无参地址（不同缓存键必取新），一劳永逸。
			// hash 路由（#/...）由浏览器在重定向后自动保留。
			if r.URL.RawQuery != "" {
				http.Redirect(w, r, "./", http.StatusMovedPermanently)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

func WorkBuddyStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, workbuddyStaticModels...)
}
func TraeWorkStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, traeworkStaticModels...)
}

// maskUpstreamErrorBody 生成中性 OpenAI 错误报文（不含上游 code/文案/品牌）。
// 掩码模式下替代上游原始 4xx/5xx body 出站；原文由调用方写日志。
func maskUpstreamErrorBody(status int, model string) []byte {
	typ := "api_error"
	if status == http.StatusTooManyRequests {
		typ = "rate_limit_error"
	} else if status >= 500 {
		typ = "server_error"
	}
	msg := fmt.Sprintf("upstream service error (http %d)", status)
	if model != "" {
		msg += " for model " + model
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": status},
	})
	return body
}
