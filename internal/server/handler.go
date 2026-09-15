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
	HardCooldown         time.Duration
	SoftCooldown         time.Duration
	ErrThreshold         int
	ErrCooldown          time.Duration
	RefreshSkew          time.Duration
}

// stickyEntry 粘性路由记录：记录上次路由账号及连续使用次数。
// 不使用 credits（pool 中余额仅在签到/手动刷新时更新，对话后是 stale 数据），
// 改用请求计数：连续请求 maxReqs 次后自动降级换账号。
type stickyEntry struct {
	uid      string
	reqCount int
	maxReqs  int
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	apiMu    sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
	stickyMu sync.RWMutex
	sticky   map[string]*stickyEntry // runtimeKind → stickyEntry
	reqLogs  reqLogStore             // 请求级日志（环形）
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
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry)}
	h.reqLogs.path = cfg.RequestLogPath
	h.reqLogs.legacy = cfg.RequestLogLegacyPath
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

// ReadBodyArchive 读回请求体存档。
func (h *Handler) ReadBodyArchive(name string) ([]byte, bool) {
	return h.reqLogs.ReadBodyArchive(name)
}

// SetBodyArchiveDir 设置请求体存档目录（main 启动时调用）。
func (h *Handler) SetBodyArchiveDir(dir string) { h.reqLogs.SetBodiesDir(dir) }

// Close 释放持有的资源（请求日志 journal 句柄）；进程退出时调用。
func (h *Handler) Close() { h.reqLogs.close() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// stickyKey 粘性路由 key（按渠道独立）
func (h *Handler) stickyKey(kind provider.Kind) string { return kind.String() }

// pickWithSticky 粘性路由选择账号。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用状态
//   - 该模型处于 6004 限额冷却
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级为按模型选号并重置粘性记录。
func (h *Handler) pickWithSticky(rt *Runtime) *auth.Auth {
	return h.pickWithStickyForModel(rt, "")
}

func (h *Handler) pickWithStickyForModel(rt *Runtime, model string) *auth.Auth {
	const defaultMaxReqs = 50

	h.stickyMu.RLock()
	sticky := h.sticky[h.stickyKey(rt.Kind)]
	h.stickyMu.RUnlock()

	if sticky != nil && sticky.uid != "" && sticky.reqCount < sticky.maxReqs {
		acct := rt.Pool.AuthByUID(sticky.uid)
		if acct != nil {
			status, ok := rt.Pool.Status(sticky.uid)
			if ok && !status.Cooling && !status.Disabled && !rt.Pool.CooledForModel(sticky.uid, model) {
				log.Printf("sticky route platform=%s uid=%s model=%s count=%d/%d",
					rt.Kind, sticky.uid, model, sticky.reqCount, sticky.maxReqs)
				return acct
			}
		}
	}

	acct := rt.Pool.PickExcludingForModel(nil, model)
	if acct == nil {
		return nil
	}

	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s model=%s maxReqs=%d", rt.Kind, acct.UID, model, defaultMaxReqs)
	return acct
}

// stickySuccess 粘性路由成功：递增请求计数。
func (h *Handler) stickySuccess(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	key := h.stickyKey(rt.Kind)
	if e := h.sticky[key]; e != nil {
		e.reqCount++
	}
}

// stickyClear 粘性路由失败（错误/冷却）：清除粘性记录，下次请求强制重新选号。
func (h *Handler) stickyClear(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	delete(h.sticky, h.stickyKey(rt.Kind))
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
			entry := map[string]any{"id": id, "object": "model", "created": 1753600000, "owned_by": k.String()}
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

	rc, uid, ok := h.dispatchChat(rt, t0, requestedModel, body, w)
	if !ok {
		return
	}
	defer rc.Close()
	bodyFile := h.reqLogs.SaveBodyArchive(body)
	body = nil
	fbw := newFirstByteWriter(w, t0)
	if peek.Stream {
		tee := &usageTee{}
		var out http.ResponseWriter = fbw
		var ttw *thinkTagWriter
		if thinkTag {
			ttw = newThinkTagWriter(fbw)
			out = ttw
		}
		err := rt.Upstream.Stream(out, &teeReadCloser{rc: rc, w: tee})
		if ttw != nil {
			ttw.Finish()
		}
		h.finishReqLogFile(t0, requestedModel, rt.Kind.String(), uid, http.StatusOK, true, fbw.ttfb(), tee.snapshot(), bodyFile)
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
	writeJSON(fbw, http.StatusOK, resp)
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
func (h *Handler) dispatchChat(rt *Runtime, t0 time.Time, model string, body []byte, w http.ResponseWriter) (io.ReadCloser, string, bool) {
	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	var lastBody []byte                   // 最后一次上游错误（轮转耗尽时按原状态透传）
	routeModel := extractModel(body)      // 出站裸模型名（6004 模型级冷却按它画界）
	convID := extractConversationID(body) // 会话标识：prompt_cache_key 注入源
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickWithStickyForModel(rt, routeModel)
		if acct == nil {
			break
		}
		if tried[acct.UID] || rt.Pool.CooledForModel(acct.UID, routeModel) {
			h.stickyClear(rt)
			acct = rt.Pool.PickExcludingForModel(tried, routeModel)
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
				h.stickyClear(rt)
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
			h.stickyClear(rt)
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			h.stickyClear(rt)
			kind := rt.Upstream.Classify(status, string(respBody))
			// 账号侧错误（限流/欠费/会话死/上游 5xx）：罚号并换号重试。
			// 客户端侧错误（400 参数/404 模型名）：换号无意义，且可能来自
			// 中转站模型探活——若罚号，单个客户端即可把整池打入冷却雪崩。
			switch kind {
			case provider.ErrHardCredit:
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
					lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
					continue
				}
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			case provider.ErrSessionDead:
				if rt.Pool.NoteSessionDead(acct.UID) {
					log.Printf("session dead disable platform=%s uid=%s consecutive=%d", rt.Kind, acct.UID, pool.SessionDeadThreshold())
				}
			case provider.ErrServer:
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			default: // ErrClient / ErrNotFound：请求本身被上游拒绝
				// 11140 内容审核熔断：短窗多次说明持续发违规内容，停号止损防整号拉黑
				if status == http.StatusForbidden && strings.Contains(string(respBody), "11140") {
					if rt.Pool.NoteContentBlock(acct.UID) {
						log.Printf("content-block circuit breaker: disable platform=%s uid=%s (11140 x3/h)", rt.Kind, acct.UID)
					}
				}
				out := respBody
				outStatus := status
				if upstream.IsContentPolicyBlock(string(respBody)) {
					// 内容审核：回网关防火墙文案，不透传上游 body（账号/业务 code）。
					kw := upstream.ContentBlockKeyword(string(respBody))
					out = upstream.FirewallHitResponse(kw)
					outStatus = http.StatusForbidden
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
				h.stickyClear(rt)
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				_ = rc.Close()
				continue
			}
		case <-time.After(firstContentTimeout):
			log.Printf("chat stall detected platform=%s uid=%s model? first content > %v, rotate", rt.Kind, acct.UID, firstContentTimeout)
			lastErr = fmt.Errorf("first content chunk timeout > %v", firstContentTimeout)
			h.stickyClear(rt)
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			_ = rc.Close()
			continue
		}
		brc.prefix = sink.Bytes()
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt)
		return brc, acct.UID, true
	}
	if lastBody != nil {
		out := lastBody
		outStatus := lastStatus
		if upstream.IsContentPolicyBlock(string(lastBody)) {
			out = upstream.FirewallHitResponse(upstream.ContentBlockKeyword(string(lastBody)))
			outStatus = http.StatusForbidden
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
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	h.finishReqLog(t0, model, rt.Kind.String(), "", http.StatusServiceUnavailable, false, 0, nil, body)
	return nil, "", false
}

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
		return rt, parts[1], nil
	}

	// 裸模型名（无渠道前缀）：在已接入账号的渠道里自动解析。
	// 优先按模型名精确命中（动态缓存 → 静态兜底）；无命中且仅有一个活跃渠道时按该渠道处理。
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
	return nil, "", fmt.Errorf("model %q not found; use explicit prefix: workbuddy/<model> / traework/<model> / qoder/<model>", model)
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
