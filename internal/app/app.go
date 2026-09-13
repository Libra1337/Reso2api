// Package app 业务编排层：HTTP 管理 API + 托盘动作 + 登录编排 + 日志。
// 由 daemon 入口（cmd/wild-work）装配，替代旧版 wails 绑定层。
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/login"
	loginqoder "wild-work/internal/login_qoder"
	logintrae "wild-work/internal/login_trae"
	"wild-work/internal/platform"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/qoder"
	"wild-work/internal/scheduler"
	"wild-work/internal/server"
	"wild-work/internal/upstream"
)

// Version 版本号。
const Version = "2.0.1"

const (
	loginTimeout   = 5 * time.Minute
	loginPollEvery = 2 * time.Second
)

// Runtime 一个渠道的运行时资源。
type Runtime struct {
	Kind      provider.Kind
	Pool      *pool.Pool
	Upstream  provider.Upstream
	Scheduler *scheduler.Scheduler
}

// Options 构建 App 的依赖。
type Options struct {
	ConfigPath string
	Config     *config.Config
	Runtimes   map[provider.Kind]*Runtime
	Handler    *server.Handler
}

// App 应用编排。
type App struct {
	cfgPath  string
	cfg      *config.Config
	runtimes map[provider.Kind]*Runtime
	handler  *server.Handler

	mu      sync.Mutex // 保护 httpSrv / cfg 修改
	httpSrv *http.Server

	muLogin      sync.Mutex
	loginBusy    bool
	loginCtx     context.Context
	loginCancel  context.CancelFunc
	loginClient  *http.Client
	loginStateFP string
	loginKind    provider.Kind
	pricingFP    string

	sessMu   sync.Mutex // 保护面板会话表
	sessions map[string]*adminSession
	sessFP   string // 面板会话持久化文件（重启/部署不丢登录）

	loginGuard *loginGuard // 面板登录暴破防护（按 IP 失败计数 + 锁定）

	travelMu       sync.Mutex // 猫猫状态缓存锁（含 in-flight 去重）
	travelCache    []TravelStatusEntry
	travelFetched  time.Time
	travelFetching bool

	usageMu      sync.Mutex // 用量统计缓存锁
	usageCache   *UsageStatsResult
	usageFetched time.Time

	newAccMu  sync.Mutex      // 新号检测锁
	knownUIDs map[string]bool // 已知 workbuddy 账号基线（导入 diff 用）

	taskMu       sync.Mutex      // 任务动态流锁
	taskEvents   []TaskEventView // 环形（最新在后）
	travelRuns   int             // travel start 未配对 end 数（>0 = 运行中）
	activityRuns int

	logFile *os.File

	refreshMu  sync.Mutex // 防并发刷新积分
	refreshing bool

	pricingMu      sync.Mutex
	pricingCache   []provider.ModelPricing // 本地缓存
	pricingFetched time.Time
	pricingErr     string // 最近一次拉取错误
}

// New 构建 App 并接管全局日志（写文件 + 环形缓冲）。
func New(opts Options) (*App, error) {
	a := &App{
		cfgPath:    opts.ConfigPath,
		cfg:        opts.Config,
		runtimes:   opts.Runtimes,
		handler:    opts.Handler,
		loginGuard: &loginGuard{},
	}
	a.loginStateFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "login-state.json")
	a.pricingFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "pricing-cache.json")
	// 面板会话持久化：部署重启后保持登录
	a.sessFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "panel_sessions.json")
	a.loadSessions()

	// 日志文件 data/app.log
	logFP := filepath.Join(filepath.Dir(opts.Config.StateFile), "app.log")
	if err := os.MkdirAll(filepath.Dir(logFP), 0o755); err == nil {
		f, err := os.OpenFile(logFP, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			a.logFile = f
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(&logWriter{app: a})
	a.loadPricingCache()
	return a, nil
}

// Close 关闭日志文件。
func (a *App) Close() {
	if a.logFile != nil {
		_ = a.logFile.Close()
		a.logFile = nil
	}
}

func (a *App) runtime(kind provider.Kind) *Runtime {
	if a.runtimes == nil {
		return nil
	}
	return a.runtimes[kind]
}

func (a *App) firstRuntime() *Runtime {
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.TraeWork, provider.Qoder} {
		if rt := a.runtime(k); rt != nil {
			return rt
		}
	}
	return nil
}

func (a *App) totalAccounts() int {
	n := 0
	for _, rt := range a.runtimes {
		if rt != nil && rt.Pool != nil {
			n += len(rt.Pool.List())
		}
	}
	return n
}

func (a *App) allStatuses() []pool.Status {
	out := []pool.Status{}
	for _, rt := range a.runtimes {
		if rt != nil && rt.Pool != nil {
			out = append(out, rt.Pool.List()...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (a *App) findRuntimeAuth(uid string) (*Runtime, *auth.Auth) {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil {
			continue
		}
		if au := rt.Pool.AuthByUID(uid); au != nil {
			return rt, au
		}
	}
	return nil, nil
}

func (a *App) checkinTimes() []string {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinTimes()
	}
	return nil
}

func (a *App) keepaliveHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.KeepaliveHours()
	}
	return nil
}

func (a *App) nextFire() time.Time {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.NextFire()
	}
	return time.Time{}
}

// SetHandler 注入 HTTP handler（在 HandleAPI 注册后调用）。
func (a *App) SetHandler(h *server.Handler) { a.handler = h }

// ---------------------------------------------------------------------------
// HTTP 服务
// ---------------------------------------------------------------------------

// StartServer 按当前配置启动 HTTP 服务。
func (a *App) StartServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serveLocked(a.cfg.Listen.Addr())
}

// serveLocked 在 newAddr 上启动新服务并切换；调用方需持有 a.mu。
func (a *App) serveLocked(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	old := a.httpSrv
	a.httpSrv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve %s: %v", addr, err)
		}
	}()
	if old != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = old.Shutdown(shutdownCtx)
	}
	return nil
}

// Stop 停止 HTTP 服务（退出时调用）。
func (a *App) Stop() {
	a.mu.Lock()
	srv := a.httpSrv
	a.httpSrv = nil
	a.mu.Unlock()
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	a.Close()
}

// Quit 退出整个程序（Web UI“退出”按钮调用）。
func (a *App) Quit() {
	a.Stop()
	os.Exit(0)
}

// ---------------------------------------------------------------------------
// 账号操作
// ---------------------------------------------------------------------------

// StartLoginFor 发起指定渠道登录：workbuddy / traework / qoder。
func (a *App) StartLoginFor(kind string) (string, error) {
	k := provider.Kind(strings.TrimSpace(kind))
	if k == "" {
		k = provider.WorkBuddy
	}
	if k != provider.WorkBuddy && k != provider.TraeWork && k != provider.Qoder {
		return "", fmt.Errorf("unknown login provider %s", kind)
	}
	a.muLogin.Lock()
	if a.loginBusy {
		a.muLogin.Unlock()
		return "", errors.New("已有登录流程进行中，请先完成或取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.loginCtx, a.loginCancel = ctx, cancel
	a.loginKind = k
	switch k {
	case provider.TraeWork:
		a.loginClient = logintrae.NewClient()
	case provider.Qoder:
		a.loginClient = loginqoder.NewClient()
	default:
		a.loginClient = login.NewClient()
	}
	a.loginBusy = true
	a.muLogin.Unlock()

	var authURL string
	var err error
	switch k {
	case provider.TraeWork:
		authURL, err = logintrae.Start(a.loginClient, a.loginStateFP)
	case provider.Qoder:
		authURL, err = loginqoder.Start(a.loginClient, a.loginStateFP)
	default:
		authURL, err = login.Start(a.loginClient, a.loginStateFP)
		if err == nil {
			if resolved, rerr := login.ResolveAuthURL(a.loginClient, authURL); rerr == nil && resolved != "" {
				authURL = resolved
			}
		}
	}
	if err != nil {
		a.finishLogin()
		return "", err
	}
	// 浏览器打开由前端 Web UI 通过 window.open() 完成
	go a.pollLogin(ctx)
	log.Printf("%s 登录流程已发起", k)
	return authURL, nil
}

// CancelLogin 取消当前登录流程。
func (a *App) CancelLogin() error {
	a.muLogin.Lock()
	cancel := a.loginCancel
	a.muLogin.Unlock()
	if cancel == nil {
		return errors.New("没有进行中的登录")
	}
	cancel()
	_ = os.Remove(a.loginStateFP)
	log.Printf("登录已取消")
	return nil
}

// pollLogin 后台轮询登录结果，写日志。
func (a *App) pollLogin(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("login poll panic: %v", r)
		}
		a.finishLogin()
	}()
	deadline := time.Now().Add(loginTimeout)
	log.Printf("登录流程已发起，等待浏览器完成…")
	t := time.NewTicker(loginPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("登录已取消")
			return
		case <-t.C:
		}
		if time.Now().After(deadline) {
			log.Printf("登录超时，请重新发起")
			return
		}
		if a.loginKind == provider.TraeWork {
			r, err := logintrae.Poll(a.loginClient, a.loginStateFP)
			if err == nil {
				a.completeTraeLogin(r)
				return
			}
			if !errors.Is(err, logintrae.ErrPending) {
				log.Printf("trae login poll failed: %v", err)
			}
			continue
		}
		if a.loginKind == provider.Qoder {
			r, err := loginqoder.Poll(a.loginClient, a.loginStateFP)
			if err == nil {
				a.completeQoderLogin(r)
				return
			}
			if !errors.Is(err, loginqoder.ErrPending) {
				log.Printf("qoder login poll failed: %v", err)
			}
			continue
		}
		r, err := login.Poll(a.loginClient, a.loginStateFP)
		if err == nil {
			a.completeLogin(r)
			return
		}
		if !errors.Is(err, login.ErrPending) {
			log.Printf("workbuddy login poll failed: %v", err)
		}
	}
}

// completeLogin 登录成功：写 auth 文件、重载账号池，异步签到。
func (a *App) completeLogin(r login.Result) {
	log.Printf("workbuddy 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := login.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("workbuddy 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("workbuddy 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.safeGo(func() {
		rt := a.runtime(provider.WorkBuddy)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		if res, err := rt.Scheduler.CheckinAccount(r.UID); err != nil {
			log.Printf("新账号签到失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("新账号签到完成 %s：%s", r.Nickname, res.Msg)
		}
	})
}

func (a *App) completeTraeLogin(r logintrae.Result) {
	log.Printf("traework 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logintrae.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("traework 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("traework 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.safeGo(func() {
		rt := a.runtime(provider.TraeWork)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		if res, err := rt.Scheduler.CheckinAccount(r.UID); err != nil {
			log.Printf("TraeWork 新账号签到失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("TraeWork 新账号签到完成 %s：%s", r.Nickname, res.Msg)
		}
	})
}

// completeQoderLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池。
// Qoder 当前无签到活动，不做首次签到。
func (a *App) completeQoderLogin(r loginqoder.Result) {
	log.Printf("qoder 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qoder", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qoder.EnsureFingerprint(au)
	fp, err := loginqoder.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qoder 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("qoder 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
}

func (a *App) finishLogin() {
	a.muLogin.Lock()
	a.loginBusy = false
	a.loginCtx, a.loginCancel, a.loginClient = nil, nil, nil
	a.loginKind = ""
	a.muLogin.Unlock()
}

// onNewWorkBuddyAccounts 检测新导入的 workbuddy 账号并自动触发
// 「上报（仅新号）→ 领养 → 巡检派出」闭环。
// 首次调用（启动对齐）只建立基线不触发；批量导入由 30s 防抖窗口合并成一轮。
func (a *App) onNewWorkBuddyAccounts(rt *Runtime, auths []*auth.Auth) {
	if rt.Scheduler == nil {
		return
	}
	a.newAccMu.Lock()
	if a.knownUIDs == nil {
		a.knownUIDs = map[string]bool{}
		for _, au := range auths {
			a.knownUIDs[au.UID] = true
		}
		a.newAccMu.Unlock()
		return // 启动基线：现有号不算"新导入"
	}
	var fresh []string
	for _, au := range auths {
		if !a.knownUIDs[au.UID] {
			a.knownUIDs[au.UID] = true
			fresh = append(fresh, au.UID)
		}
	}
	a.newAccMu.Unlock()
	if len(fresh) == 0 {
		return
	}
	log.Printf("detected %d new account(s), auto onboarding in 30s: %v", len(fresh), fresh)
	sch := rt.Scheduler
	uids := fresh
	a.safeGo(func() {
		time.Sleep(30 * time.Second) // 防抖：多文件批量导入合并成一轮
		sch.RunActivityFor(uids)     // 新号补对话门槛 + 领养
		sch.RunTravelNow()           // 巡检：新号领养兜底/派出，老号幂等 skip
	})
}

// ReloadAccounts 导出启动/外部触发用的账号池对齐入口。
func (a *App) ReloadAccounts() { a.reloadAccounts() }

// reloadAccounts 用 auths 目录最新文件对齐账号池。
func (a *App) reloadAccounts() {
	if rt := a.runtime(provider.WorkBuddy); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadWorkBuddyDir(a.cfg.AuthDir, a.cfg.Region)
		if err != nil {
			log.Printf("reload workbuddy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
			a.onNewWorkBuddyAccounts(rt, auths)
		}
	}
	if rt := a.runtime(provider.TraeWork); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadTraeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload traework accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Qoder); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qoder accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
}

// CheckinAccount 单个账号立即签到。
func (a *App) CheckinAccount(uid string) (scheduler.CheckinResult, error) {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Scheduler == nil {
		return scheduler.CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Qoder {
		return scheduler.CheckinResult{}, fmt.Errorf("Qoder 渠道无签到活动")
	}
	res, err := rt.Scheduler.CheckinAccount(uid)
	if err != nil {
		log.Printf("checkin failed uid=%s err=%v", uid, err)
		return res, err
	}
	log.Printf("checkin uid=%s ok=%t msg=%s remain=%d has_remain=%t", uid, res.OK, res.Msg, res.Remain, res.HasRemain)
	return res, nil
}

// CheckinAll 全部账号立即签到。
// TaskEventView 任务动态流条目（面板展示）。
type TaskEventView struct {
	Kind string `json:"kind"` // travel | activity
	UID  string `json:"uid"`
	Msg  string `json:"msg"`
	At   int64  `json:"at"`
}

const taskFeedCap = 200

// NotifyTaskEvent 接收调度器任务事件（main 里 wiring 到 scheduler observer）。
// start/end 事件维护运行态计数；UID 为空即起止事件。
func (a *App) NotifyTaskEvent(kind, uid, msg string) {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	if uid == "" {
		switch {
		case strings.Contains(msg, "开始"):
			if kind == "travel" {
				a.travelRuns++
			} else {
				a.activityRuns++
			}
		case strings.Contains(msg, "完成"):
			if kind == "travel" {
				a.travelRuns = 0
			} else {
				a.activityRuns = 0
			}
		}
	}
	a.taskEvents = append(a.taskEvents, TaskEventView{Kind: kind, UID: uid, Msg: msg, At: time.Now().Unix()})
	if len(a.taskEvents) > taskFeedCap {
		a.taskEvents = a.taskEvents[len(a.taskEvents)-taskFeedCap:]
	}
}

// TaskFeed 返回任务动态（新→旧）与运行态。
func (a *App) TaskFeed() map[string]any {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	out := make([]TaskEventView, 0, len(a.taskEvents))
	for i := len(a.taskEvents) - 1; i >= 0; i-- {
		out = append(out, a.taskEvents[i])
	}
	return map[string]any{
		"events":           out,
		"travel_running":   a.travelRuns > 0,
		"activity_running": a.activityRuns > 0,
	}
}

// UsageStatsResult 用量统计聚合。
type UsageStatsResult struct {
	Days      int              `json:"days"`
	FetchedAt int64            `json:"fetched_at"`
	Daily     []map[string]any `json:"daily"`    // [{date, credit, requests}]
	Models    []map[string]any `json:"models"`   // [{model, credit, requests}]
	Accounts  []map[string]any `json:"accounts"` // [{nickname, credit, requests}]
	Tokens    []map[string]any `json:"tokens"`   // 请求日志 token 聚合 [{model, tokens, requests}]
	Expiring  []map[string]any `json:"expiring"` // ≤7 天到期的积分包 [{nickname, name, remain, expire_at}]
	Errors    []string         `json:"errors,omitempty"`
}

const usageStatsTTL = 10 * time.Minute

// UsageStats 聚合官方请求用量（积分消耗）+ 本地请求日志 token 统计 + 积分到期提醒。
func (a *App) UsageStats(days int, force bool) *UsageStatsResult {
	a.usageMu.Lock()
	if !force && a.usageCache != nil && time.Since(a.usageFetched) < usageStatsTTL && a.usageCache.Days == days {
		out := a.usageCache
		a.usageMu.Unlock()
		return out
	}
	a.usageMu.Unlock()

	rt := a.runtime(provider.WorkBuddy)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return &UsageStatsResult{Days: days}
	}
	api, ok := rt.Upstream.(interface {
		FetchRequestUsage(acct *auth.Auth, d int) ([]upstream.UsageRow, error)
	})
	if !ok {
		return &UsageStatsResult{Days: days}
	}
	up, _ := rt.Upstream.(taskAPI)

	type agg struct {
		credit   float64
		requests int64
	}
	dayAgg := map[string]*agg{}
	modelAgg := map[string]*agg{}
	acctAgg := map[string]*agg{}
	var errs []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var expiring []map[string]any

	for _, st := range rt.Pool.List() {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(uid, nickname string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			acct := rt.Pool.AuthByUID(uid)
			if acct == nil {
				return
			}
			rows, err := api.FetchRequestUsage(acct, days)
			if err != nil {
				mu.Lock()
				errs = append(errs, nickname+": "+err.Error())
				mu.Unlock()
				return
			}
			mu.Lock()
			aa := acctAgg[nickname]
			if aa == nil {
				aa = &agg{}
				acctAgg[nickname] = aa
			}
			for _, r := range rows {
				d := r.Time.Format("2006-01-02")
				if dayAgg[d] == nil {
					dayAgg[d] = &agg{}
				}
				dayAgg[d].credit += r.Credit
				dayAgg[d].requests++
				if modelAgg[r.Model] == nil {
					modelAgg[r.Model] = &agg{}
				}
				modelAgg[r.Model].credit += r.Credit
				modelAgg[r.Model].requests++
				aa.credit += r.Credit
				aa.requests++
			}
			mu.Unlock()
			// 积分到期提醒（≤7 天）
			if up != nil {
				if _, items, err := rt.Upstream.UserResourceDetail(acct); err == nil {
					deadline := time.Now().AddDate(0, 0, 7)
					for _, it := range items {
						if it.ExpireAt == "" {
							continue
						}
						if t, err := time.ParseInLocation("2006-01-02 15:04:05", it.ExpireAt, time.Local); err == nil && t.After(time.Now()) && t.Before(deadline) && it.Remain > 0 {
							mu.Lock()
							expiring = append(expiring, map[string]any{
								"nickname": nickname, "name": it.Name,
								"remain": it.Remain, "expire_at": it.ExpireAt,
							})
							mu.Unlock()
						}
					}
				}
			}
		}(st.UID, st.Nickname)
	}
	wg.Wait()

	sortAgg := func(m map[string]*agg) []map[string]any {
		out := make([]map[string]any, 0, len(m))
		for k, v := range m {
			out = append(out, map[string]any{"key": k, "credit": v.credit, "requests": v.requests})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["credit"].(float64) > out[j]["credit"].(float64) })
		return out
	}
	daily := sortAgg(dayAgg)
	sort.Slice(daily, func(i, j int) bool {
		return daily[i]["key"].(string) < daily[j]["key"].(string)
	})

	// token 统计：请求日志内存环（模型 → tokens/requests）
	tokAgg := map[string]*agg{}
	for _, l := range a.handler.RequestLogs() {
		total := l.InTokens + l.OutTokens
		if total > 0 {
			if tokAgg[l.Model] == nil {
				tokAgg[l.Model] = &agg{}
			}
			tokAgg[l.Model].credit += float64(total)
			tokAgg[l.Model].requests++
		}
	}
	tokens := sortAgg(tokAgg)

	res := &UsageStatsResult{
		Days: days, FetchedAt: time.Now().Unix(),
		Daily: daily, Models: sortAgg(modelAgg), Accounts: sortAgg(acctAgg),
		Tokens: tokens, Expiring: expiring, Errors: errs,
	}
	a.usageMu.Lock()
	a.usageCache = res
	a.usageFetched = time.Now()
	a.usageMu.Unlock()
	return res
}

// LimitsOverview 模型限流总览（面板「模型限流」页）。
func (a *App) LimitsOverview() map[string]any {
	type acctCool struct {
		UID      string    `json:"uid"`
		Nickname string    `json:"nickname"`
		Reason   string    `json:"reason"`
		Until    time.Time `json:"until"`
	}
	models := []map[string]any{}
	cooling := []acctCool{}
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil {
			continue
		}
		prefix := ""
		if rt.Kind != provider.WorkBuddy {
			prefix = rt.Kind.String() + "/"
		}
		for _, m := range rt.Pool.ModelCooldowns() {
			name := m.Model
			if prefix != "" {
				name = prefix + m.Model
			}
			models = append(models, map[string]any{
				"model":     name,
				"cooled":    m.Cooled,
				"total":     m.Total,
				"available": m.Available,
			})
		}
		for _, st := range rt.Pool.List() {
			if st.Cooling && !st.Disabled {
				cooling = append(cooling, acctCool{UID: st.UID, Nickname: st.Nickname, Reason: st.Reason, Until: st.Until})
			}
		}
	}
	return map[string]any{"models": models, "account_cooling": cooling}
}

// TravelStatusEntry 猫猫乐园单账号状态。
type TravelStatusEntry struct {
	UID      string                 `json:"uid"`
	Nickname string                 `json:"nickname"`
	Disabled bool                   `json:"disabled"`
	Buddy    *upstream.Buddy        `json:"buddy"` // nil = 无猫
	Travel   *upstream.TravelState  `json:"travel"`
	Streak   *upstream.StreakDetail `json:"streak"`
	Error    string                 `json:"error,omitempty"`
}

type travelStatusResp struct {
	FetchedAt int64               `json:"fetched_at"`
	Accounts  []TravelStatusEntry `json:"accounts"`
}

// travelStatusAPI 面板聚合所需的 growth 能力（仅 workbuddy 上游实现）。
type travelStatusAPI interface {
	BuddyInfo(*auth.Auth) (*upstream.Buddy, error)
	TravelStatus(*auth.Auth) (*upstream.TravelState, error)
	GrowthStreakDetail(*auth.Auth) (*upstream.StreakDetail, error)
}

const travelStatusTTL = 60 * time.Second

// TravelStatus 聚合全部 workbuddy 账号的猫/旅行/连登状态（60s 缓存）。
func (a *App) TravelStatus(force bool) travelStatusResp {
	a.travelMu.Lock()
	if !force && time.Since(a.travelFetched) < travelStatusTTL && a.travelCache != nil {
		resp := travelStatusResp{FetchedAt: a.travelFetched.Unix(), Accounts: a.travelCache}
		a.travelMu.Unlock()
		return resp
	}
	for a.travelFetching { // 并发刷新去重：等前一 个完成后再看缓存
		a.travelMu.Unlock()
		time.Sleep(100 * time.Millisecond)
		a.travelMu.Lock()
		if time.Since(a.travelFetched) < time.Second && a.travelCache != nil {
			resp := travelStatusResp{FetchedAt: a.travelFetched.Unix(), Accounts: a.travelCache}
			a.travelMu.Unlock()
			return resp
		}
	}
	a.travelFetching = true
	a.travelMu.Unlock()
	defer func() {
		a.travelMu.Lock()
		a.travelFetching = false
		a.travelMu.Unlock()
	}()

	entries := make([]TravelStatusEntry, 0)
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil {
			continue
		}
		api, ok := rt.Upstream.(travelStatusAPI)
		if !ok {
			continue // 无 growth 能力的平台（traework/qoder）
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4) // 并发上限，防面板刷新打爆上游
		for _, st := range rt.Pool.List() {
			wg.Add(1)
			go func(uid, nickname string, disabled bool) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				e := TravelStatusEntry{UID: uid, Nickname: nickname, Disabled: disabled}
				if disabled {
					mu.Lock()
					entries = append(entries, e)
					mu.Unlock()
					return
				}
				acct := rt.Pool.AuthByUID(uid)
				if acct == nil {
					e.Error = "no credentials"
					mu.Lock()
					entries = append(entries, e)
					mu.Unlock()
					return
				}
				e.Buddy, _ = api.BuddyInfo(acct)
				e.Travel, _ = api.TravelStatus(acct)
				e.Streak, _ = api.GrowthStreakDetail(acct)
				mu.Lock()
				entries = append(entries, e)
				mu.Unlock()
			}(st.UID, st.Nickname, st.Disabled)
		}
		wg.Wait()
	}
	// 排序：可领奖 > 旅行中 > 空闲 > 无猫 > 禁用
	rank := func(e TravelStatusEntry) int {
		if e.Disabled {
			return 5
		}
		if e.Buddy == nil {
			return 4
		}
		if e.Travel != nil {
			switch e.Travel.State {
			case "arrived":
				return 1
			case "traveling":
				return 2
			}
		}
		return 3
	}
	sort.Slice(entries, func(i, j int) bool {
		if rank(entries[i]) != rank(entries[j]) {
			return rank(entries[i]) < rank(entries[j])
		}
		return entries[i].Nickname < entries[j].Nickname
	})

	a.travelMu.Lock()
	a.travelCache = entries
	a.travelFetched = time.Now()
	a.travelMu.Unlock()
	return travelStatusResp{FetchedAt: a.travelFetched.Unix(), Accounts: entries}
}

// RunTravelAll 异步触发全部平台的旅行巡检（无旅行能力的平台自动跳过）。
func (a *App) RunTravelAll() {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Scheduler == nil {
			continue
		}
		sch := rt.Scheduler
		a.safeGo(sch.RunTravelNow)
	}
}

// RunClaimsAll 异步触发全部平台的到站领奖（无能力平台自动跳过）。
func (a *App) RunClaimsAll() {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Scheduler == nil {
			continue
		}
		sch := rt.Scheduler
		a.safeGo(sch.RunClaimsNow)
	}
}

// RunActivityAll 异步触发全部平台的活跃上报。
func (a *App) RunActivityAll() {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Scheduler == nil {
			continue
		}
		sch := rt.Scheduler
		a.safeGo(sch.RunActivityNow)
	}
}

func (a *App) CheckinAll() []scheduler.CheckinResult {
	results := make([]scheduler.CheckinResult, 0)
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Scheduler == nil {
			continue
		}
		if rt.Kind == provider.Qoder { // Qoder 无签到活动，跳过
			continue
		}
		for _, st := range rt.Pool.List() {
			if st.Disabled {
				results = append(results, scheduler.CheckinResult{UID: st.UID, Msg: "账号已禁用"})
				continue
			}
			res, err := rt.Scheduler.CheckinAccount(st.UID)
			if err != nil {
				res = scheduler.CheckinResult{UID: st.UID, Msg: err.Error()}
			}
			results = append(results, res)
		}
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	log.Printf("批量签到完成：total=%d ok=%d failed=%d", len(results), ok, len(results)-ok)
	return results
}

// RefreshCredits 刷新单个账号积分。
func (a *App) RefreshCredits(uid string) (int64, error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return 0, fmt.Errorf("unknown account %s", uid)
	}
	log.Printf("credits refresh start platform=%s uid=%s", rt.Kind, uid)
	remain, err := rt.Upstream.UserResource(au)
	if err != nil {
		log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, uid, err)
		return 0, err
	}
	rt.Pool.SetCredits(uid, remain)
	log.Printf("credits refresh success platform=%s uid=%s remain=%d", rt.Kind, uid, remain)
	return remain, nil
}

// RefreshAll 刷新全部账号积分，返回汇总（供托盘消息框 / Web UI）。
func (a *App) RefreshAll() RefreshSummary {
	a.refreshMu.Lock()
	if a.refreshing {
		a.refreshMu.Unlock()
		return RefreshSummary{Busy: true}
	}
	a.refreshing = true
	a.refreshMu.Unlock()
	defer func() {
		a.refreshMu.Lock()
		a.refreshing = false
		a.refreshMu.Unlock()
	}()
	sum := RefreshSummary{Platforms: map[string]PlatformSummary{}}
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil {
			continue
		}
		ps := PlatformSummary{Accounts: []AccountRefresh{}}
		for _, st := range rt.Pool.List() {
			au := rt.Pool.AuthByUID(st.UID)
			if au == nil || au.AccessToken == "" {
				ps.Failed++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: false, Msg: "no token"})
				continue
			}
			if remain, err := rt.Upstream.UserResource(au); err == nil {
				rt.Pool.SetCredits(st.UID, remain)
				ps.OK++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: true, Remain: remain})
			} else {
				ps.Failed++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: false, Msg: shortErr(err)})
				log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, st.UID, err)
			}
		}
		sum.Total += ps.OK + ps.Failed
		sum.OK += ps.OK
		sum.Failed += ps.Failed
		sum.Platforms[rt.Kind.String()] = ps
	}
	return sum
}

// RefreshSummary 积分刷新汇总（托盘消息框内容）。
type RefreshSummary struct {
	Busy      bool                       `json:"busy"`
	Total     int                        `json:"total"`
	OK        int                        `json:"ok"`
	Failed    int                        `json:"failed"`
	Platforms map[string]PlatformSummary `json:"platforms"`
}

// PlatformSummary 单个渠道汇总。
type PlatformSummary struct {
	OK       int              `json:"ok"`
	Failed   int              `json:"failed"`
	Accounts []AccountRefresh `json:"accounts"`
}

// AccountRefresh 单账号刷新结果。
type AccountRefresh struct {
	UID    string `json:"uid"`
	OK     bool   `json:"ok"`
	Remain int64  `json:"remain"`
	Msg    string `json:"msg,omitempty"`
}

// RemoveAccount 删除账号（auth 文件 + 内存池）。
func (a *App) RemoveAccount(uid string) error {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if au.FilePath != "" {
		_ = os.Remove(au.FilePath)
	}
	rt.Pool.Remove(uid)
	log.Printf("已删除账号 %s", shortUID(uid))
	return nil
}

// DisableAccount 停用/启用账号。
func (a *App) DisableAccount(uid string, disabled bool) error {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Pool == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	rt.Pool.SetDisabled(uid, disabled)
	if disabled {
		log.Printf("已停用账号 %s", shortUID(uid))
	} else {
		log.Printf("已启用账号 %s", shortUID(uid))
	}
	return nil
}

// ResourceDetail 查询单个账号积分明细。
func (a *App) ResourceDetail(uid string) (int64, []provider.ResourceItem, error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil || rt.Upstream == nil {
		return 0, nil, fmt.Errorf("unknown account %s", uid)
	}
	return rt.Upstream.UserResourceDetail(au)
}

// ---------------------------------------------------------------------------
// 配置操作
// ---------------------------------------------------------------------------

// SetCheckinTimes 更新自动签到时间（HH:MM）并立即唤醒两个调度器。
func (a *App) SetCheckinTimes(times []string) error {
	minutes, err := config.ParseClockTimes(times)
	if err != nil {
		return err
	}
	clean := normalizeMinutes(minutes)
	if len(clean) == 0 {
		return errors.New("请至少保留一个签到时间")
	}
	formatted := make([]string, 0, len(clean))
	for _, m := range clean {
		formatted = append(formatted, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	a.mu.Lock()
	a.cfg.Schedule.CheckinTimes = formatted
	a.cfg.Schedule.CheckinHours = nil
	err = config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	for _, rt := range a.runtimes {
		if rt == nil || rt.Scheduler == nil {
			continue
		}
		if rt.Kind == provider.Qoder {
			continue // Qoder 无签到活动，避免面板更新把注定失败的签到重新排上
		}
		rt.Scheduler.SetCheckinMinutes(clean)
	}
	log.Printf("自动签到时间已更新：%s", strings.Join(formatted, "、"))
	return nil
}

// SetListen 修改 API 监听主机 + 端口：保存配置并热切换监听。
func (a *App) SetListen(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("端口无效：%d", port)
	}
	addr := config.Listen{Host: host, Port: port}.Addr()

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.serveLocked(addr); err != nil {
		return fmt.Errorf("监听 %s 失败（可能被占用）：%v", addr, err)
	}
	a.cfg.Listen = config.Listen{Host: host, Port: port}
	if err := config.Save(a.cfg, a.cfgPath); err != nil {
		log.Printf("save config after listen change: %v", err)
	}
	log.Printf("API 监听已切换至 %s", addr)
	return nil
}

// SetAPIKey 修改 API 密钥。
func (a *App) SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	a.mu.Lock()
	a.cfg.APIKey = key
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.handler.SetAPIKey(key)
	log.Printf("API-Key 已更新")
	return nil
}

// SetAutostart 设置/取消开机自启。
func (a *App) SetAutostart(on bool) error {
	if err := platform.SetAutostart(on); err != nil {
		return err
	}
	if on {
		log.Printf("已开启开机自启")
	} else {
		log.Printf("已关闭开机自启")
	}
	return nil
}

// AutostartEnabled 当前是否已开机自启。
func (a *App) AutostartEnabled() bool { return platform.AutostartEnabled() }

// ServerRunning 是否正在监听。
func (a *App) ServerRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.httpSrv != nil
}

// LoginBusy 是否登录中。
func (a *App) LoginBusy() bool {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginBusy
}

// ---------------------------------------------------------------------------
// 面板数据（Web UI）
// ---------------------------------------------------------------------------

// AccountView 面板展示的账号（脱敏）。
type AccountView struct {
	UID            string `json:"uid"`
	Group          string `json:"group"` // workbuddy | traework
	Nickname       string `json:"nickname"`
	Credits        int64  `json:"credits"`
	Cooling        bool   `json:"cooling"`
	Until          string `json:"until"`
	Reason         string `json:"reason"`
	Disabled       bool   `json:"disabled"`
	ErrCount       int    `json:"err_count"`
	LastCheckinOK  bool   `json:"last_checkin_ok"`
	LastCheckinAt  string `json:"last_checkin_at"`
	LastCheckinMsg string `json:"last_checkin_msg"`
}

// State Web UI 初始数据。
type State struct {
	Accounts       []AccountView `json:"accounts"`
	CheckinTimes   []string      `json:"checkin_times"`
	KeepaliveHours []int         `json:"keepalive_hours"`
	ListenHost     string        `json:"listen_host"`
	ListenPort     int           `json:"listen_port"`
	APIKey         string        `json:"api_key"`
	LoginBusy      bool          `json:"login_busy"`
	NextCheckin    string        `json:"next_checkin"`
	Version        string        `json:"version"`
	Autostart      bool          `json:"autostart"`
	Running        bool          `json:"running"`
}

// GetState 返回面板初始数据。
func (a *App) GetState() State {
	st := State{
		CheckinTimes:   a.checkinTimes(),
		KeepaliveHours: a.keepaliveHours(),
		ListenHost:     a.cfg.Listen.Host,
		ListenPort:     a.cfg.Listen.Port,
		APIKey:         a.cfg.APIKey,
		LoginBusy:      a.LoginBusy(),
		NextCheckin:    fmtTime(a.nextFire()),
		Version:        Version,
		Autostart:      a.AutostartEnabled(),
		Running:        a.ServerRunning(),
	}
	st.Accounts = a.accountViews()
	return st
}

func (a *App) accountViews() []AccountView {
	statuses := a.allStatuses()
	out := make([]AccountView, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, AccountView{
			UID:            s.UID,
			Group:          a.accountGroup(s.UID),
			Nickname:       s.Nickname,
			Credits:        s.Credits,
			Cooling:        s.Cooling,
			Until:          fmtTime(s.Until),
			Reason:         s.Reason,
			Disabled:       s.Disabled,
			ErrCount:       s.ErrCount,
			LastCheckinOK:  s.LastCheckinOK,
			LastCheckinAt:  fmtTime(s.LastCheckinAt),
			LastCheckinMsg: s.LastCheckinMsg,
		})
	}
	return out
}

// accountGroup 返回账号所属分组（workbuddy/traework）。
func (a *App) accountGroup(uid string) string {
	_, au := a.findRuntimeAuth(uid)
	if au != nil {
		if au.Kind != "" {
			return au.Kind
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "trae-") {
			return "traework"
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "qoder") {
			return "qoder"
		}
	}
	return "workbuddy"
}

// OpenLogFile 用系统默认编辑器打开日志文件。
func (a *App) OpenLogFile() error {
	fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
	if _, err := os.Stat(fp); err != nil {
		_ = os.WriteFile(fp, []byte("(empty)\n"), 0o644)
	}
	abs, err := filepath.Abs(fp)
	if err != nil {
		return err
	}
	return platform.OpenWithTextEditor(abs)
}

// NotifyCheckin 记录自动签到结果（调度器观察器）。
func (a *App) NotifyCheckin(platformName string, r scheduler.CheckinResult) {
	log.Printf("checkin platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t",
		platformName, r.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
}

// NotifyRefresh 记录 token 刷新结果（调度器观察器）。
func (a *App) NotifyRefresh(platformName, uid string, ok bool, msg string) {
	log.Printf("refresh platform=%s uid=%s ok=%t msg=%s", platformName, uid, ok, msg)
}

// ---------------------------------------------------------------------------
// 管理 API（HTTP）
// ---------------------------------------------------------------------------

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// apiError 统一错误响应。
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// HandleAPI 注册管理 API 路由（挂到 server handler 的 /api/* 上）。
// admin_password 非空时，除 session/login/logout 外全部要求会话 Cookie + CSRF。
func (a *App) HandleAPI(mux *http.ServeMux) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.GetState())
	})
	inner.HandleFunc("POST /api/login/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channel string `json:"channel"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		url, err := a.StartLoginFor(req.Channel)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"auth_url": url})
	})
	inner.HandleFunc("POST /api/login/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.CancelLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	inner.HandleFunc("POST /api/account/checkin", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, err := a.CheckinAccount(req.UID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	inner.HandleFunc("POST /api/account/checkin_all", func(w http.ResponseWriter, r *http.Request) {
		results := a.CheckinAll()
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	})
	// 一键旅行巡检 / 活跃上报：异步触发（全量账号含限速间隔需 1-3 分钟，
	// 同步等完会撞 HTTP 超时）；结果写运行日志，面板即时返回"已启动"。
	inner.HandleFunc("POST /api/travel/run_all", func(w http.ResponseWriter, r *http.Request) {
		a.RunTravelAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	})
	inner.HandleFunc("POST /api/activity/run_all", func(w http.ResponseWriter, r *http.Request) {
		a.RunActivityAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	})
	// 一键领奖：只对到站账号领奖（幂等，比全量巡检轻）。
	inner.HandleFunc("POST /api/travel/claim_all", func(w http.ResponseWriter, r *http.Request) {
		a.RunClaimsAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	})
	// 成长任务：列表 / 单任务执行 / 单账号一键全做。
	inner.HandleFunc("GET /api/tasks/list", func(w http.ResponseWriter, r *http.Request) {
		uid := r.URL.Query().Get("uid")
		api, rt := a.workbuddyTaskAPI()
		if api == nil {
			apiError(w, http.StatusBadRequest, "workbuddy 平台未启用")
			return
		}
		if uid == "" {
			writeJSON(w, http.StatusOK, map[string]any{"actions": AutoActionsMeta()})
			return
		}
		acct := rt.Pool.AuthByUID(uid)
		if acct == nil {
			apiError(w, http.StatusBadRequest, "unknown account")
			return
		}
		tasks, err := api.ListTasks(acct)
		if err != nil {
			apiError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "actions": AutoActionsMeta()})
	})
	inner.HandleFunc("POST /api/tasks/auto", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID      string `json:"uid"`
			TaskCode string `json:"task_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.UID == "" || req.TaskCode == "" {
			apiError(w, http.StatusBadRequest, "uid 和 task_code 必填")
			return
		}
		resp, err := a.RunTaskAuto(req.UID, req.TaskCode)
		if err != nil {
			apiError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	inner.HandleFunc("POST /api/tasks/auto_all", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID  string   `json:"uid"`
			UIDs []string `json:"uids"`
			All  bool     `json:"all"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.All || len(req.UIDs) > 0 {
			// 批量：全部可用账号或指定账号列表，串行执行（防风控）
			if err := a.RunTaskAutoAllBatch(req.UIDs, req.All); err != nil {
				apiError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "batch": true})
			return
		}
		if req.UID == "" {
			apiError(w, http.StatusBadRequest, "uid 必填（或传 all/uids 批量）")
			return
		}
		if err := a.RunTaskAutoAll(req.UID); err != nil {
			apiError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	})
	// 猫猫乐园聚合状态：全部账号的猫档案 + 旅行进度 + 连登（60s 缓存，
	// ?refresh=1 强制刷新）。并发拉取，面板单用户场景足够。
	inner.HandleFunc("GET /api/travel/status", func(w http.ResponseWriter, r *http.Request) {
		force := r.URL.Query().Get("refresh") == "1"
		data := a.TravelStatus(force)
		writeJSON(w, http.StatusOK, data)
	})
	// 任务动态流：旅行巡检/活跃上报的实时逐账号反馈（面板轮询）。
	inner.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.TaskFeed())
	})
	// 用量统计：官方请求用量（积分消耗）× 账号/模型/日 聚合 + 请求日志 token 统计。
	inner.HandleFunc("GET /api/usage/stats", func(w http.ResponseWriter, r *http.Request) {
		days := 31
		if v := r.URL.Query().Get("days"); v == "7" {
			days = 7
		}
		force := r.URL.Query().Get("refresh") == "1"
		writeJSON(w, http.StatusOK, a.UsageStats(days, force))
	})
	// 模型限流总览：6004 模型级限额（哪些模型在哪些号上被限、何时重置）
	// + 整号冷却明细（429/欠费/连续错误）。
	inner.HandleFunc("GET /api/limits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.LimitsOverview())
	})
	inner.HandleFunc("POST /api/account/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		remain, err := a.RefreshCredits(req.UID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"remain": remain})
	})
	inner.HandleFunc("POST /api/account/refresh_all", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.RefreshAll())
	})
	inner.HandleFunc("POST /api/account/remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.RemoveAccount(req.UID); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// 导入 WorkBuddy 桌面端导出的凭证 JSON（auth.Parse 兼容嵌套/扁平）。
	inner.HandleFunc("POST /api/account/import", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Raw string `json:"raw"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Raw) == "" {
			apiError(w, http.StatusBadRequest, "请求体缺少 raw 字段")
			return
		}
		au, err := auth.Parse([]byte(req.Raw))
		if err != nil {
			apiError(w, http.StatusBadRequest, "解析凭证失败："+err.Error())
			return
		}
		if au.UID == "" || au.AccessToken == "" {
			apiError(w, http.StatusBadRequest, "凭证缺少 uid 或 accessToken")
			return
		}
		if !validUID(au.UID) {
			apiError(w, http.StatusBadRequest, "凭证 uid 含非法字符（仅允许字母/数字与 -_._@）")
			return
		}
		// 桌面端导出的 expiresAt 为毫秒时间戳，网关按秒处理
		if au.ExpiresAt > 1_000_000_000_000 {
			au.ExpiresAt /= 1000
		}
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken":  au.AccessToken,
				"refreshToken": au.RefreshToken,
				"expiresAt":    au.ExpiresAt,
				"domain":       au.Domain,
			},
			"account": map[string]any{
				"uid":          au.UID,
				"enterpriseId": au.EnterpriseID,
				"nickname":     au.Nickname,
			},
		}
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = os.MkdirAll(a.cfg.AuthDir, 0o755)
		fp := filepath.Join(a.cfg.AuthDir, "workbuddy-"+au.UID+".json")
		tmp := fp + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.Rename(tmp, fp); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.reloadAccounts()
		log.Printf("imported account uid=%s nickname=%s file=%s", au.UID, au.Nickname, filepath.Base(fp))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": au.UID, "nickname": au.Nickname})
	})
	inner.HandleFunc("POST /api/account/disable", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID      string `json:"uid"`
			Disabled bool   `json:"disabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.DisableAccount(req.UID, req.Disabled); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	inner.HandleFunc("POST /api/account/resource_detail", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		remain, items, err := a.ResourceDetail(req.UID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"remain": remain, "items": items})
	})
	inner.HandleFunc("POST /api/config/checkin_times", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Times []string `json:"times"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetCheckinTimes(req.Times); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	inner.HandleFunc("POST /api/config/listen", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetListen(req.Host, req.Port); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	inner.HandleFunc("POST /api/config/api_key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetAPIKey(req.Key); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	inner.HandleFunc("POST /api/config/autostart", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetAutostart(req.On); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	inner.HandleFunc("GET /api/fees", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.FeesInfo())
	})
	inner.HandleFunc("POST /api/fees/refresh", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.FeesInfo())
		go a.safeGo(func() { a.RefreshPricing() })
	})
	inner.HandleFunc("GET /api/request_logs", func(w http.ResponseWriter, r *http.Request) {
		if a.handler != nil {
			writeJSON(w, http.StatusOK, map[string]any{"logs": a.handler.RequestLogs()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"logs": []any{}})
	})
	inner.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
		raw, _ := os.ReadFile(fp)
		lines := strings.Split(string(raw), "\n")
		if len(lines) > 300 {
			lines = lines[len(lines)-300:]
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	})
	inner.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		go a.safeGo(func() { a.Quit() })
	})

	// 受保护路由挂载：/api/ 前缀整体经会话鉴权（更具体的 session/login/logout 优先匹配）
	mux.Handle("/api/", a.requireSession(inner.ServeHTTP))

	// 开放路由：会话查询 / 登录 / 登出
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		if !a.authEnabled() {
			writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": "panel-auth-disabled"})
			return
		}
		if s := a.lookupSession(r); s != nil {
			writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": s.CSRF})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !a.authEnabled() {
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
			return
		}
		ip := remoteIP(r)
		if a.loginGuard.locked(ip) {
			apiError(w, http.StatusTooManyRequests, "尝试次数过多，请 5 分钟后再试")
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.cfg.AdminPassword)) != 1 {
			a.loginGuard.fail(ip)
			log.Printf("panel login FAILED remote=%s", ip) // 失败必须留痕，暴破才可见
			apiError(w, http.StatusUnauthorized, "密码错误")
			return
		}
		a.loginGuard.reset(ip)
		token, err := randToken(32)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		csrf, err := randToken(32)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.sessMu.Lock()
		if a.sessions == nil {
			a.sessions = make(map[string]*adminSession)
		}
		// 顺带清理过期会话
		now := time.Now()
		for k, v := range a.sessions {
			if now.After(v.Expires) {
				delete(a.sessions, k)
			}
		}
		a.sessions[token] = &adminSession{CSRF: csrf, Expires: now.Add(sessionTTL)}
		a.saveSessionsLocked()
		a.sessMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
			Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		})
		log.Printf("panel login ok remote=%s", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "csrf_token": csrf})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err == nil {
			a.sessMu.Lock()
			delete(a.sessions, c.Value)
			a.saveSessionsLocked()
			a.sessMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
			MaxAge:   -1,
		})
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
}

// ---------------------------------------------------------------------------
// 面板会话鉴权
// ---------------------------------------------------------------------------

const (
	sessionCookieName = "ww2a_session"
	sessionTTL        = 7 * 24 * time.Hour
)

// adminSession 一次面板登录会话。
type adminSession struct {
	CSRF    string    `json:"csrf"`
	Expires time.Time `json:"expires"`
}

// sessionsFile 会话持久化格式。v2 增加 pw_fp（签发时的管理密码指纹）：
// 密码变更（改 config 重启）后指纹不符，全部旧会话作废。
// 兼容读取 v1（裸 map，无指纹，不校验）。
type sessionsFile struct {
	PwFP     string                   `json:"pw_fp,omitempty"`
	Sessions map[string]*adminSession `json:"sessions"`
}

// loadSessions 启动时从磁盘恢复未过期会话（部署重启不丢登录）。
func (a *App) loadSessions() {
	if a.sessFP == "" {
		return
	}
	raw, err := os.ReadFile(a.sessFP)
	if err != nil {
		return
	}
	var file sessionsFile
	var sessions map[string]*adminSession
	if json.Unmarshal(raw, &file) == nil && file.Sessions != nil {
		if file.PwFP != "" && file.PwFP != pwFingerprint(a.cfg.AdminPassword) {
			log.Printf("panel sessions dropped: admin password changed")
			return
		}
		sessions = file.Sessions
	} else if json.Unmarshal(raw, &sessions) != nil {
		return
	}
	now := time.Now()
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sessions == nil {
		a.sessions = make(map[string]*adminSession)
	}
	for k, v := range sessions {
		if v != nil && now.Before(v.Expires) {
			a.sessions[k] = v
		}
	}
}

// saveSessionsLocked 落盘当前会话表（调用方持 sessMu）；v2 格式含密码指纹。
func (a *App) saveSessionsLocked() {
	if a.sessFP == "" || a.sessions == nil {
		return
	}
	raw, err := json.MarshalIndent(sessionsFile{
		PwFP:     pwFingerprint(a.cfg.AdminPassword),
		Sessions: a.sessions,
	}, "", "  ")
	if err != nil {
		return
	}
	_ = auth.WriteFileSync(a.sessFP, raw, 0o600)
}

// readLogTail 只读日志文件末尾 max 字节，再裁到 keep 行。
// 整文件 ReadFile 在日志膨胀后会造成每请求的内存尖峰。
func readLogTail(fp string, max int64, keep int) []string {
	f, err := os.Open(fp)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > max {
		if _, err := f.Seek(-max, io.SeekEnd); err != nil {
			return []string{}
		}
	}
	buf, _ := io.ReadAll(io.LimitReader(f, max))
	lines := strings.Split(strings.TrimLeft(string(buf), "\n"), "\n")
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return lines
}

// authEnabled 面板鉴权是否启用（admin_password 非空）。
func (a *App) authEnabled() bool {
	return a.cfg.AdminPassword != ""
}

// lookupSession 从 Cookie 恢复有效会话，无/过期返回 nil。
func (a *App) lookupSession(r *http.Request) *adminSession {
	if !a.authEnabled() {
		return &adminSession{CSRF: ""} // 未启用鉴权：视为已登录（无 CSRF 要求）
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.Expires) {
		return nil
	}
	return s
}

// requireSession 管理 API 中间件：会话校验 + 非 GET 请求 CSRF 校验。
func (a *App) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := a.lookupSession(r)
		if s == nil {
			apiError(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		if a.authEnabled() && r.Method != http.MethodGet {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
				apiError(w, http.StatusForbidden, "CSRF 校验失败")
				return
			}
		}
		next(w, r)
	}
}

// validUID 账号 UID 白名单：路径分隔符/目录跳段一律拒绝。
// /api/account/import 把 UID 拼进落盘文件名（workbuddy-<uid>.json），
// 无白名单则 uid="/../config" 可穿越出 auths 目录任意改写 .json 文件。
// 真实 UID 为 UUID 形态（hex + 连字符）。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 128 || strings.Contains(uid, "..") {
		return false
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '@':
		default:
			return false
		}
	}
	return true
}

// loginGuard 面板登录失败防护：单 IP 连续失败超阈值后锁定一段时间。
// 无限试密码 + 失败不留痕 = 暴破不可见；此组件让两者都有边界与审计。
type loginGuard struct {
	mu    sync.Mutex
	fails map[string]*loginFail
}

type loginFail struct {
	count int
	until time.Time // 非零 = 锁定截止
}

const (
	loginFailLimit = 10              // 连续失败阈值
	loginLockout   = 5 * time.Minute // 锁定时长
)

// locked 返回该 IP 是否处于锁定期。
func (g *loginGuard) locked(ip string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.fails[ip]
	return ok && time.Now().Before(f.until)
}

// fail 记一次失败；达到阈值进入锁定。
func (g *loginGuard) fail(ip string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fails == nil {
		g.fails = map[string]*loginFail{}
	}
	f, ok := g.fails[ip]
	if !ok {
		f = &loginFail{}
		g.fails[ip] = f
	}
	f.count++
	if f.count >= loginFailLimit {
		f.until = time.Now().Add(loginLockout)
		f.count = 0
	}
}

// reset 成功登录清零。
func (g *loginGuard) reset(ip string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

// remoteIP 提取对端 IP（去端口）。
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// pwFingerprint 管理密码指纹：会话文件记录签发时的指纹，
// 密码变更（改 config 重启）后旧会话全部失效，避免"改了密码旧登录还活着"。
func pwFingerprint(pw string) string {
	sum := sha256.Sum256([]byte("ww2a:" + pw))
	return hex.EncodeToString(sum[:8])
}

// randToken 生成 n 字节随机 hex 串。
func randToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------------------
// 渠道费率信息（本地缓存 + 按需刷新）
// ---------------------------------------------------------------------------

// FeesInfo 返回渠道费率说明（本地缓存优先，有账号时尝试拉取上游）。
func (a *App) FeesInfo() map[string]any {
	a.pricingMu.Lock()
	cached := a.pricingCache
	errMsg := a.pricingErr
	a.pricingMu.Unlock()

	if len(cached) == 0 {
		// 首次加载：异步拉取，先返回静态兜底
		go a.safeGo(func() { a.RefreshPricing() })
		return staticFeesInfo()
	}

	// 缓存超过 1 小时，后台静默刷新
	if time.Since(a.pricingFetched) > time.Hour {
		go a.safeGo(func() { a.RefreshPricing() })
	}

	// 按渠道分组
	groups := map[string][]map[string]any{}
	for _, p := range cached {
		groups[p.Channel] = append(groups[p.Channel], map[string]any{
			"model": p.Model,
			"rate":  p.Rate,
			"note":  p.Note,
		})
	}

	channels := make([]map[string]any, 0)
	for _, ch := range []string{"workbuddy", "traework", "qoder"} {
		if models, ok := groups[ch]; ok {
			channels = append(channels, map[string]any{
				"channel": ch,
				"models":  models,
			})
		}
	}

	result := map[string]any{
		"note":       "费率随上游平台政策动态变化，请以官方为准；点击「刷新费率」重新拉取。",
		"channels":   channels,
		"disclaimer": "本工具仅聚合转发，不参与定价；渠道费率以各上游官方页面为准。",
		"cached_at":  a.pricingFetched.Format("01-02 15:04"),
	}
	if errMsg != "" {
		result["error"] = errMsg
	}
	return result
}

// RefreshPricing 从所有已接入渠道拉取最新定价并更新缓存。
func (a *App) RefreshPricing() {
	allPricing := make([]provider.ModelPricing, 0)
	var errs []string

	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil || len(rt.Pool.List()) == 0 {
			continue
		}
		acct := rt.Pool.Pick()
		if acct == nil {
			continue
		}
		// 先确保 token 有效
		if acct.NeedsRefresh(10 * time.Minute) {
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				errs = append(errs, fmt.Sprintf("%s: token refresh failed", rt.Kind))
				continue
			}
		}
		pricing, err := rt.Upstream.FetchModelPricing(acct)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", rt.Kind, shortErr(err)))
			log.Printf("pricing fetch failed platform=%s err=%v", rt.Kind, err)
			continue
		}
		allPricing = append(allPricing, pricing...)
		log.Printf("pricing fetched platform=%s count=%d", rt.Kind, len(pricing))
	}

	// 排序：按渠道 + 倍率
	sort.Slice(allPricing, func(i, j int) bool {
		if allPricing[i].Channel != allPricing[j].Channel {
			return allPricing[i].Channel < allPricing[j].Channel
		}
		return allPricing[i].Rate < allPricing[j].Rate
	})

	a.pricingMu.Lock()
	a.pricingCache = allPricing
	a.pricingFetched = time.Now()
	if len(errs) > 0 {
		a.pricingErr = strings.Join(errs, "; ")
	} else {
		a.pricingErr = ""
	}
	a.pricingMu.Unlock()
	a.savePricingCache()
}

// loadPricingCache 从文件加载定价缓存。
func (a *App) loadPricingCache() {
	raw, err := os.ReadFile(a.pricingFP)
	if err != nil {
		return
	}
	var cache struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}
	if err := json.Unmarshal(raw, &cache); err != nil {
		return
	}
	a.pricingMu.Lock()
	a.pricingCache = cache.Models
	if cache.Fetched != "" {
		if t, err := time.Parse(time.RFC3339, cache.Fetched); err == nil {
			a.pricingFetched = t
		}
	}
	a.pricingMu.Unlock()
}

// savePricingCache 持久化定价缓存到文件。
func (a *App) savePricingCache() {
	a.pricingMu.Lock()
	cache := struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}{
		Models:  a.pricingCache,
		Fetched: a.pricingFetched.Format(time.RFC3339),
	}
	a.pricingMu.Unlock()
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(a.pricingFP, raw, 0o644)
}

// staticFeesInfo 静态兜底费率说明（无账号/首次加载时）。
func staticFeesInfo() map[string]any {
	return map[string]any{
		"note": "费率随上游平台政策动态变化，请以官方为准；添加账号后点击「刷新费率」拉取实时数据。",
		"channels": []map[string]any{
			{
				"channel": "workbuddy",
				"models": []map[string]any{
					{"model": "auto", "rate": 0, "note": "自动路由最优模型"},
					{"model": "deepseek-v4-*", "rate": 0, "note": "按对话计费"},
					{"model": "glm-*", "rate": 0, "note": "按对话计费"},
					{"model": "kimi-* / minimax-* / hy3", "rate": 0, "note": "按对话计费"},
				},
			},
			{
				"channel": "traework",
				"models": []map[string]any{
					{"model": "DeepSeek-*", "rate": 0, "note": "按 token 计费"},
					{"model": "glm-5*", "rate": 0, "note": "按 token 计费"},
					{"model": "Doubao-* / qwen-*", "rate": 0, "note": "按 token 计费"},
				},
			},
			{
				"channel": "qoder",
				"models": []map[string]any{
					{"model": "deepseek-v4-*", "rate": 0, "note": "按积分计费"},
					{"model": "qwen3.* / glm-5.*", "rate": 0, "note": "按积分计费"},
					{"model": "kimi-* / minimax-* / auto", "rate": 0, "note": "按积分计费"},
				},
			},
		},
		"disclaimer": "本工具仅聚合转发，不参与定价；渠道费率以各上游官方页面为准。",
	}
}

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

// logWriter 写日志文件。
type logWriter struct{ app *App }

func (w *logWriter) Write(p []byte) (int, error) {
	if w.app.logFile != nil {
		_, _ = w.app.logFile.Write(p)
	}
	return len(p), nil
}

// safeGo 带 panic 兜底的 goroutine 启动器。
func (a *App) safeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC: %v", r)
			}
		}()
		f()
	}()
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func normalizeMinutes(minutes []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("01-02 15:04")
}

func shortUID(uid string) string {
	if len(uid) <= 10 {
		return uid
	}
	return uid[:10] + "…"
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
