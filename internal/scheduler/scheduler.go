// Package scheduler 定时任务：每日签到 + token keepalive。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
// 签到时间支持分钟精度运行中更新（SetCheckinMinutes），由 GUI 面板写入 config.json 后调用。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       provider.Upstream
	Name           string // 日志中的平台名
	CheckinHours   []int  // 旧配置兼容，整点小时
	CheckinMinutes []int  // 当天分钟数，优先于 CheckinHours
	KeepaliveHours []int  // 默认 [22]
	// SkipCheckin 平台无签到活动（如 Qoder）时置真：不注入默认签到时间，
	// 调度器只做 keepalive；否则 New 的默认值会让每账号每天两次注定失败的签到。
	SkipCheckin bool

	// TravelInterval 旅行巡检滚动间隔（如 2h）：>0 时忽略 TravelHours 时点，
	// 每隔该间隔巡检一轮——巡检是幂等状态机（到站领奖/空闲派出/其余 skip），
	// 高频安全，猫一到站即领奖再派出，不必等时点。0 = 用 TravelHours 每日时点。
	TravelInterval      time.Duration
	TravelHours         []int // 猫猫旅行时点（默认 [9,21]：一趟派出 + 一趟领奖闭环），仅 workbuddy
	ActivityHours       []int // 活跃上报时点（默认 [10]），仅 workbuddy
	BlackcatHours       []int // 夜猫子补足时点（默认 [23]，须在 23:00–08:00 窗口内）
	ActivityReportCount int   // 每号每次上报条数（默认 5：领猫对话量门槛）
	// TravelDisabled/ActivityDisabled 显式关闭对应排程（traework/qoder 平台置真）。
	TravelDisabled   bool
	ActivityDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	mu         sync.Mutex // 保护 cfg 中的小时配置
	cfg        Config
	wake       chan struct{}              // 配置变更唤醒 Run 循环重算下次触发
	onCheckin  func(CheckinResult)        // 结果观察者，供 GUI 接收自动签到结果
	onRefresh  func(string, bool, string) // token 刷新结果观察者
	adoptTried map[string]string          // uid → 自然日（CST）：领养门槛未达的当日防抖
	travelMu   sync.Mutex                 // 旅行巡检重入锁（手动+定时撞车防护）
	activityMu sync.Mutex                 // 活跃上报重入锁

	onTaskEvent func(TaskEvent) // 任务事件观察者（面板动态流）
	tcMu        sync.Mutex      // 旅行动作计数锁
	tc          travelCountersSnapshot
}

// New 构建。
func New(cfg Config) *Scheduler {
	if !cfg.SkipCheckin && len(cfg.CheckinMinutes) == 0 {
		if len(cfg.CheckinHours) > 0 {
			cfg.CheckinMinutes = make([]int, 0, len(cfg.CheckinHours))
			for _, h := range cfg.CheckinHours {
				if h >= 0 && h <= 23 {
					cfg.CheckinMinutes = append(cfg.CheckinMinutes, h*60)
				}
			}
		} else {
			cfg.CheckinMinutes = []int{9 * 60, 21 * 60}
		}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if !cfg.TravelDisabled && len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if !cfg.ActivityDisabled && len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 5
	}
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1)}
}

// schedule 返回当前签到分钟/保活小时配置的副本。
func (s *Scheduler) schedule() (checkinMinutes, keepaliveHours []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.cfg.CheckinMinutes...), append([]int{}, s.cfg.KeepaliveHours...)
}

// CheckinHours 保留旧 API，返回整点签到小时。
func (s *Scheduler) CheckinHours() []int {
	minutes, _ := s.schedule()
	out := make([]int, 0, len(minutes))
	for _, m := range minutes {
		if m%60 == 0 {
			out = append(out, m/60)
		}
	}
	return out
}

// CheckinMinutes 当前签到时间，单位为当天分钟数（0..1439）。
func (s *Scheduler) CheckinMinutes() []int {
	minutes, _ := s.schedule()
	return minutes
}

// CheckinTimes 当前签到时间，格式为 HH:MM。
func (s *Scheduler) CheckinTimes() []string {
	minutes := s.CheckinMinutes()
	out := make([]string, 0, len(minutes))
	for _, m := range minutes {
		out = append(out, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return out
}

// KeepaliveHours 当前保活时间（副本）。
func (s *Scheduler) KeepaliveHours() []int {
	_, kh := s.schedule()
	return kh
}

// SetCheckinMinutes 运行中更新签到分钟并唤醒调度循环。
func (s *Scheduler) SetCheckinMinutes(minutes []int) {
	clean := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Ints(clean)
	s.mu.Lock()
	s.cfg.CheckinMinutes = clean
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SetCheckinHours 保留旧 API，将整点小时转换为当天分钟数。
func (s *Scheduler) SetCheckinHours(hours []int) {
	minutes := make([]int, 0, len(hours))
	for _, h := range hours {
		minutes = append(minutes, h*60)
	}
	s.SetCheckinMinutes(minutes)
}

// SetCheckinObserver 设置签到结果观察器；用于把定时任务结果推送到 GUI。
func (s *Scheduler) SetCheckinObserver(fn func(CheckinResult)) {
	s.mu.Lock()
	s.onCheckin = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyCheckin(r CheckinResult) {
	s.mu.Lock()
	fn := s.onCheckin
	s.mu.Unlock()
	if fn != nil {
		fn(r)
	}
}

// SetRefreshObserver 设置 token 刷新结果观察器。
func (s *Scheduler) SetRefreshObserver(fn func(uid string, ok bool, msg string)) {
	s.mu.Lock()
	s.onRefresh = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyRefresh(uid string, ok bool, msg string) {
	s.mu.Lock()
	fn := s.onRefresh
	s.mu.Unlock()
	if fn != nil {
		fn(uid, ok, msg)
	}
}

// NextFire 返回最近的一次触发时间（签到 + 保活合并）。
func (s *Scheduler) NextFire() time.Time {
	ch, kh := s.schedule()
	all := append(append([]int{}, ch...), hoursToMinutes(kh)...)
	return nextFireMinutes(time.Now(), all)
}

// nextFire 保留旧测试/API语义：hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	return nextFireMinutes(now, hoursToMinutes(hours))
}

func hoursToMinutes(hours []int) []int {
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		out = append(out, h*60)
	}
	return out
}

// nextFireMinutes 返回 now 之后最近的触发时间；输入为当天分钟数。
func nextFireMinutes(now time.Time, minutes []int) time.Time {
	var earliest time.Time
	for _, m := range minutes {
		if m < 0 || m >= 24*60 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), m/60, m%60, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// travelInterval 读取旅行滚动间隔（0 = 时点模式）。
func (s *Scheduler) travelInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.TravelInterval
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskKeepalive
	taskTravel
	taskActivity
	taskBlackcat
)

// scheduleAll 返回全部任务时点（分钟），travel/activity 关闭时为空。
func (s *Scheduler) scheduleAll() (checkin, keepalive, travel, activity, blackcat []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	checkin = append([]int{}, s.cfg.CheckinMinutes...)
	keepalive = hoursToMinutes(s.cfg.KeepaliveHours)
	if !s.cfg.TravelDisabled {
		travel = append([]int{}, s.cfg.TravelHours...)
	}
	if !s.cfg.ActivityDisabled {
		activity = append([]int{}, s.cfg.ActivityHours...)
	}
	blackcat = append([]int{}, s.cfg.BlackcatHours...)
	return
}

// Run 主循环，阻塞直到 ctx 取消。
// 按最近触发时点调度四类任务；触发时点按任务槽精确执行（不再按当前分钟匹配，
// 避免进程繁忙跨分钟后当日触发被静默跳过）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		now := time.Now()
		ch, kh, th, ah, bh := s.scheduleAll()
		type slot struct {
			at   time.Time
			kind taskKind
			min  int
		}
		var slots []slot
		for _, m := range ch {
			slots = append(slots, slot{nextFireMinutes(now, []int{m}), taskCheckin, m})
		}
		for _, m := range kh {
			slots = append(slots, slot{nextFireMinutes(now, []int{m}), taskKeepalive, m})
		}
		if s.travelInterval() > 0 {
			// 滚动间隔模式：本轮结束后隔 interval 再巡检（忽略时点表）
			slots = append(slots, slot{now.Add(s.travelInterval()), taskTravel, -1})
		} else {
			for _, m := range th {
				slots = append(slots, slot{nextFireMinutes(now, []int{m}), taskTravel, m})
			}
		}
		for _, m := range ah {
			slots = append(slots, slot{nextFireMinutes(now, []int{m}), taskActivity, m})
		}
		for _, m := range bh {
			slots = append(slots, slot{nextFireMinutes(now, []int{m}), taskBlackcat, m})
		}
		var next *slot
		for i := range slots {
			if next == nil || slots[i].at.Before(next.at) {
				next = &slots[i]
			}
		}
		if next == nil {
			// 无任何任务（全部显式关闭）：空转等唤醒
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		timer := time.NewTimer(time.Until(next.at))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop() // 配置变更，重算
		case <-timer.C:
			switch next.kind {
			case taskCheckin:
				s.RunCheckinNow()
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskTravel:
				s.RunTravelNow()
			case taskActivity:
				s.RunActivityNow()
			case taskBlackcat:
				s.RunBlackcatNow()
			}
		}
	}
}

func containsMinute(minutes []int, minute int) bool {
	for _, v := range minutes {
		if v == minute {
			return true
		}
	}
	return false
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// CheckinResult 单账号签到结果（GUI 面板展示）。
type CheckinResult struct {
	UID       string `json:"uid"`
	OK        bool   `json:"ok"`
	Msg       string `json:"msg"`
	Remain    int64  `json:"remain"`
	HasRemain bool   `json:"has_remain"`
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	defer s.RunStreakBonusNow() // 签到后跑连登管家：补签/礼包/档位兑换/抽奖（幂等）
	name := s.name()
	log.Printf("checkin batch start platform=%s accounts=%d", name, len(s.cfg.Pool.List()))
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			log.Printf("checkin skip platform=%s uid=%s reason=disabled", name, st.UID)
			continue
		}
		r := s.checkinOne(st.UID)
		log.Printf("checkin result platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t", name, st.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
	}
	log.Printf("checkin batch done platform=%s", name)
}

// CheckinAccount 单个账号立即签到（禁用跳过），返回该账号结果。
func (s *Scheduler) CheckinAccount(uid string) (CheckinResult, error) {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if st.Disabled {
		return CheckinResult{}, fmt.Errorf("account %s disabled", uid)
	}
	return s.checkinOne(uid), nil
}

// checkinOne 单账号签到 + 余额刷新 + 解冻 + 记录签到状态。
func (s *Scheduler) checkinOne(uid string) CheckinResult {
	name := s.name()
	log.Printf("checkin start platform=%s uid=%s", name, uid)
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		r := CheckinResult{UID: uid, Msg: "no refresh token"}
		s.cfg.Pool.RecordCheckin(uid, false, r.Msg)
		s.notifyCheckin(r)
		return r
	}
	// 签到前保证 access token 有效；否则仅依赖晚间 keepalive 时，早上的签到可能拿过期 token。
	if a.NeedsRefresh(2 * time.Hour) {
		if err := s.refreshForCheckin(a, uid); err != nil {
			r := CheckinResult{UID: uid, Msg: "refresh: " + shortErr(err)}
			s.cfg.Pool.RecordCheckin(uid, false, r.Msg)
			s.notifyCheckin(r)
			return r
		}
	}
	r := CheckinResult{UID: uid}
	checkinErr := s.cfg.Upstream.DailyCheckin(a)
	// status 接口本身就是令牌有效性验证；若返回 session dead，刷新一次后重试整套签到。
	if checkinErr != nil && isSessionDead(checkinErr) {
		log.Printf("checkin token invalid platform=%s uid=%s, refreshing and retrying", name, uid)
		if err := s.refreshForCheckin(a, uid); err != nil {
			checkinErr = fmt.Errorf("token invalid; refresh failed: %w", err)
		} else {
			checkinErr = s.cfg.Upstream.DailyCheckin(a)
		}
	}
	if checkinErr != nil {
		log.Printf("checkin failed platform=%s uid=%s err=%v", name, uid, checkinErr)
		r.Msg = shortErr(checkinErr)
		if isAlready(checkinErr) {
			r.OK = true // 已签到时视为成功状态
			r.Msg = "已签到"
		}
	} else {
		r.OK = true
		r.Msg = "ok"
	}
	// 无论签到成败都查余额（已签到等业务错误下余额刷新仍有效）
	remain, rerr := s.cfg.Upstream.UserResource(a)
	if rerr != nil {
		log.Printf("checkin credits failed platform=%s uid=%s err=%v", name, uid, rerr)
		r.OK = false // 签到后的积分确认失败，整次操作向 GUI 报告失败
		if r.Msg == "" {
			r.Msg = "余额查询失败"
		} else {
			r.Msg += "；余额查询失败"
		}
	} else {
		r.Remain, r.HasRemain = remain, true
		log.Printf("checkin credits platform=%s uid=%s remain=%d", name, uid, remain)
		s.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	s.cfg.Pool.RecordCheckin(uid, r.OK, r.Msg)
	s.notifyCheckin(r)
	return r
}

// isAlready 只匹配明确的“今日已签到”，不能因错误文本包含 checkin 就判成功。
func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked") ||
		strings.Contains(s, "code=9095")
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func (s *Scheduler) name() string {
	if s.cfg.Name != "" {
		return s.cfg.Name
	}
	return "unknown"
}

func (s *Scheduler) refreshForCheckin(a *auth.Auth, uid string) error {
	name := s.name()
	log.Printf("refresh start platform=%s uid=%s reason=checkin", name, uid)
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("refresh failed platform=%s uid=%s err=%v", name, uid, err)
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("refresh save failed platform=%s uid=%s err=%v", name, uid, err)
		return fmt.Errorf("refresh save: %w", err)
	}
	log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, uid, a.ExpiresAt)
	return nil
}

func isSessionDead(err error) bool {
	var ue *provider.Error
	return errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	name := s.name()
	log.Printf("refresh batch start platform=%s", name)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			log.Printf("refresh skip platform=%s uid=%s reason=disabled", name, st.UID)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			msg := "no refresh token"
			log.Printf("refresh skip platform=%s uid=%s reason=%s", name, st.UID, msg)
			s.notifyRefresh(st.UID, false, msg)
			continue
		}
		log.Printf("refresh start platform=%s uid=%s reason=keepalive", name, st.UID)
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh failed platform=%s uid=%s err=%v", name, st.UID, err)
			var ue *provider.Error
			if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
				// 连续 N 次才禁用：单次 12153 多为抖动误报，一次禁号误杀健康账号
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("refresh disabled platform=%s uid=%s reason=session_dead consecutive=%d", name, st.UID, pool.SessionDeadThreshold())
				}
			}
			s.notifyRefresh(st.UID, false, err.Error())
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh save failed platform=%s uid=%s err=%v", name, st.UID, err)
			s.notifyRefresh(st.UID, false, "refresh save: "+err.Error())
			continue
		}
		log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, st.UID, a.ExpiresAt)
		s.notifyRefresh(st.UID, true, "ok")
	}
	log.Printf("refresh batch done platform=%s", name)
}
