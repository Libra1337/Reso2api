// travel.go 猫猫旅行巡检 + 活跃上报（仅 WorkBuddy 上游支持，经能力断言挂载）。
// 移植自 workbuddy2api 上游：每账号单趟状态机推进，不轮询不等待。
package scheduler

import (
	"fmt"
	"log"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间相同。
	travelLocationID = 4

	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// 账号间限速与账号内上报间隔：避免触发上游风控。测试可置 0。
var (
	travelAccountDelay   = 800 * time.Millisecond
	activityAccountDelay = 800 * time.Millisecond
	activityReportGap    = 1500 * time.Millisecond
)

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。
var cstZone = time.FixedZone("CST", 8*60*60)

func travelDay(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// travelAPI 猫猫旅行/活跃上报能力接口（仅 workbuddy 上游实现；
// 调度器经类型断言获取，traework/qoder 无此能力自然跳过）。
type travelAPI interface {
	TravelStatus(a *auth.Auth) (*upstream.TravelState, error)
	TravelDepart(a *auth.Auth, locationID int) error
	TravelClaim(a *auth.Auth, recordID int64) (int64, error)
	BuddyInfo(a *auth.Auth) (*upstream.Buddy, error)
	BuddyFirst(a *auth.Auth) error
	BuddyAgreement(a *auth.Auth) error
	GrowthStreak(a *auth.Auth) (int, error)
	ReportChatActivity(a *auth.Auth, conversationID, requestID string) error
}

// TaskEvent 任务执行事件（面板实时动态流）：逐账号动作 + 开始/结束汇总。
type TaskEvent struct {
	Kind string // travel | activity
	UID  string
	Msg  string
	At   time.Time
}

// SetTaskObserver 注册任务事件观察者（面板任务动态流的数据源）。
func (s *Scheduler) SetTaskObserver(fn func(TaskEvent)) {
	s.mu.Lock()
	s.onTaskEvent = fn
	s.mu.Unlock()
}

// emitTask 发一条任务事件（无观察者时零开销）。
func (s *Scheduler) emitTask(kind, uid, msg string) {
	s.mu.Lock()
	fn := s.onTaskEvent
	s.mu.Unlock()
	if fn != nil {
		fn(TaskEvent{Kind: kind, UID: uid, Msg: msg, At: time.Now()})
	}
}

// travelCap 返回上游的旅行能力视图；无该能力（非 workbuddy 平台）返回 nil。
func (s *Scheduler) travelCap() travelAPI {
	if s.cfg.Upstream == nil {
		return nil
	}
	if cap, ok := s.cfg.Upstream.(travelAPI); ok {
		return cap
	}
	return nil
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；查询失败只跳过该账号本轮；账号间限速。
// TryLock 防重入：手动触发与定时触发撞车时直接跳过，不双打上游。
func (s *Scheduler) RunTravelNow() {
	api := s.travelCap()
	if api == nil {
		return
	}
	if !s.travelMu.TryLock() {
		log.Printf("travel platform=%s: busy, skip (already running)", s.cfg.Name)
		return
	}
	defer s.travelMu.Unlock()
	total := 0
	for _, st := range s.cfg.Pool.List() {
		if !st.Disabled {
			total++
		}
	}
	s.emitTask("travel", "", fmt.Sprintf("旅行巡检开始（%d 个账号）", total))
	var adopted, claimed, departed int
	var claimCredits int64
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		before := s.travelCounters()
		s.travelOne(api, a)
		after := s.travelCounters()
		switch {
		case after.adopts > before.adopts:
			adopted++
			s.emitTask("travel", a.UID, "领养成功 +300 积分")
		case after.claims > before.claims:
			claimed++
			claimCredits += after.claimCredits - before.claimCredits
			s.emitTask("travel", a.UID, fmt.Sprintf("领奖 +%d 积分", after.claimCredits-before.claimCredits))
		case after.departs > before.departs:
			departed++
			s.emitTask("travel", a.UID, "已派出旅行")
		default:
			s.emitTask("travel", a.UID, travelSkipReason(api, a))
		}
	}
	s.emitTask("travel", "", fmt.Sprintf("旅行巡检完成：领奖 %d 次(+%d 积分) · 派出 %d · 领养 %d",
		claimed, claimCredits, departed, adopted))
}

// refreshCredits 收益入账（领奖/领养）后立即回读余额写入池——
// 否则账号卡片要等下次签到才反映猫猫收益，面板上"收益加了但积分没动"。
func (s *Scheduler) refreshCredits(a *auth.Auth, why string) {
	if s.cfg.Upstream == nil || s.cfg.Pool == nil {
		return
	}
	remain, err := s.cfg.Upstream.UserResource(a)
	if err != nil {
		log.Printf("travel platform=%s uid=%s: refresh credits after %s: %v", s.cfg.Name, a.UID, why, err)
		return
	}
	s.cfg.Pool.SetCredits(a.UID, remain)
}

// bumpTravel 原子更新旅行动作计数。
func (s *Scheduler) bumpTravel(fn func(*travelCountersSnapshot)) {
	s.tcMu.Lock()
	fn(&s.tc)
	s.tcMu.Unlock()
}

// travelCounters 读取旅行动作计数（供逐账号增量判定）。
func (s *Scheduler) travelCounters() travelCountersSnapshot {
	s.tcMu.Lock()
	defer s.tcMu.Unlock()
	return s.tc
}

type travelCountersSnapshot struct {
	adopts       int
	claims       int
	departs      int
	claimCredits int64
}

// travelSkipReason 无动作账号的一句话原因（面板展示用）。
func travelSkipReason(api travelAPI, a *auth.Auth) string {
	if ts, err := api.TravelStatus(a); err == nil && ts != nil {
		switch ts.State {
		case travelStateTraveling:
			return fmt.Sprintf("旅行中（约 %s 后到站，奖励 %d）", fmtDur(time.Until(time.Unix(ts.ArriveAt, 0))), ts.RewardCredit)
		case travelStateIdle:
			if ts.DailyLimitReached {
				return "今日已派出，明天再来"
			}
		}
	}
	return "本轮无需动作"
}

// fmtDur 中文时长（小时/分钟）。
func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if h := int(d.Hours()); h > 0 {
		return fmt.Sprintf("%d小时%d分", h, int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d分钟", int(d.Minutes()))
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作。
func (s *Scheduler) travelOne(api travelAPI, a *auth.Auth) {
	buddy, err := api.BuddyInfo(a)
	if err != nil {
		log.Printf("travel platform=%s uid=%s: buddy-info: %v", s.cfg.Name, a.UID, err)
		return
	}
	if buddy == nil {
		s.adoptBuddy(api, a, false)
		return
	}
	ts, err := api.TravelStatus(a)
	if err != nil {
		log.Printf("travel platform=%s uid=%s: status: %v", s.cfg.Name, a.UID, err)
		return
	}
	switch ts.State {
	case travelStateArrived:
		s.travelClaim(api, a, ts)
	case travelStateIdle:
		s.travelDepart(api, a, ts)
	case travelStateTraveling:
		log.Printf("travel platform=%s uid=%s: skip (traveling record=%d)", s.cfg.Name, a.UID, ts.RecordID)
	default:
		log.Printf("travel platform=%s uid=%s: skip (unknown state %q)", s.cfg.Name, a.UID, ts.State)
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(api travelAPI, a *auth.Auth, ts *upstream.TravelState) {
	if ts.DailyLimitReached {
		log.Printf("travel platform=%s uid=%s: skip (daily limit reached)", s.cfg.Name, a.UID)
		return
	}
	if err := api.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel platform=%s uid=%s: depart: %v", s.cfg.Name, a.UID, err)
		return
	}
	log.Printf("travel platform=%s uid=%s: depart ok location=%d", s.cfg.Name, a.UID, travelLocationID)
	s.bumpTravel(func(t *travelCountersSnapshot) { t.departs++ })
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(api travelAPI, a *auth.Auth, ts *upstream.TravelState) {
	if ts.RecordID == 0 {
		log.Printf("travel platform=%s uid=%s: claim skipped (no record_id)", s.cfg.Name, a.UID)
		return
	}
	reward, err := api.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel platform=%s uid=%s: claim record=%d: %v", s.cfg.Name, a.UID, ts.RecordID, err)
		return
	}
	log.Printf("travel platform=%s uid=%s: claim ok record=%d reward=%d", s.cfg.Name, a.UID, ts.RecordID, reward)
	s.bumpTravel(func(t *travelCountersSnapshot) { t.claims++; t.claimCredits += reward })
	s.refreshCredits(a, "claim")
}

// adoptBuddy 无猫时领养：先同意协议（幂等）再 buddy/first。
// 门槛未达标（HTTP 400 first_buddy task not completed）属预期，
// 记当日已试后静默跳过。force=true 豁免当日防抖（活跃上报补满对话量后重试）。
func (s *Scheduler) adoptBuddy(api travelAPI, a *auth.Auth, force bool) {
	if !force && s.adoptTriedToday(a.UID) {
		return
	}
	if err := api.BuddyAgreement(a); err != nil {
		log.Printf("travel platform=%s uid=%s: agreement: %v", s.cfg.Name, a.UID, err)
		return
	}
	err := api.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel platform=%s uid=%s: adopt ok (+300 credits)", s.cfg.Name, a.UID)
		s.bumpTravel(func(t *travelCountersSnapshot) { t.adopts++ })
		s.refreshCredits(a, "adopt")
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel platform=%s uid=%s: adopt skipped (conversation threshold not reached, retry tomorrow)", s.cfg.Name, a.UID)
	default:
		log.Printf("travel platform=%s uid=%s: adopt: %v", s.cfg.Name, a.UID, err)
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adoptTried == nil {
		s.adoptTried = map[string]string{}
	}
	s.adoptTried[uid] = travelDay(time.Now())
}

// RunActivityFor 对指定账号执行活跃上报（新号导入场景：只补新号门槛，
// 不重复打扰老号——上游风控口径每号每天 1 次足够）。
// 完成后由调用方决定是否接巡检（app 层新号闭环）。
func (s *Scheduler) RunActivityFor(uids []string) {
	api := s.travelCap()
	if api == nil || len(uids) == 0 {
		return
	}
	if !s.activityMu.TryLock() {
		log.Printf("activity platform=%s: busy, skip targeted run", s.cfg.Name)
		return
	}
	defer s.activityMu.Unlock()
	count := s.activityCount()
	s.emitTask("activity", "", fmt.Sprintf("新号上报开始（%d 个账号 × %d 条）", len(uids), count))
	var reported int
	for i, uid := range uids {
		a := s.cfg.Pool.AuthByUID(uid)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if i > 0 {
			time.Sleep(activityAccountDelay)
		}
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for j := 1; j <= count; j++ {
			if err := api.ReportChatActivity(a, cid, fmt.Sprintf("%s-r%d", cid, j)); err != nil {
				log.Printf("activity platform=%s uid=%s: report %d/%d: %v", s.cfg.Name, uid, j, count, err)
				break
			}
			ok++
			if j < count {
				time.Sleep(activityReportGap)
			}
		}
		if ok < count {
			s.emitTask("activity", uid, fmt.Sprintf("上报中断（%d/%d 条）", ok, count))
			continue
		}
		reported++
		if days, err := api.GrowthStreak(a); err == nil && days > 0 {
			s.emitTask("activity", uid, fmt.Sprintf("上报完成，连登 %d 天", days))
		} else {
			s.emitTask("activity", uid, "上报完成")
		}
		if buddy, err := api.BuddyInfo(a); err == nil && buddy == nil {
			before := s.travelCounters()
			s.adoptBuddy(api, a, true)
			if s.travelCounters().adopts > before.adopts {
				s.emitTask("activity", uid, "领养成功 +300 积分")
			}
		}
	}
	s.emitTask("activity", "", fmt.Sprintf("新号上报完成：%d/%d 个号发满", reported, len(uids)))
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 每号 N 条（ActivityReportCount，默认 5）共用同一 conversationId、requestId 各自独立
// ——领养猫的对话量门槛实测需 5 次。上报全发满后：streak 自检 + 无猫账号立即重试领养。
func (s *Scheduler) RunActivityNow() {
	api := s.travelCap()
	if api == nil {
		return
	}
	if !s.activityMu.TryLock() {
		log.Printf("activity platform=%s: busy, skip (already running)", s.cfg.Name)
		return
	}
	defer s.activityMu.Unlock()
	count := s.activityCount()
	total := 0
	for _, st := range s.cfg.Pool.List() {
		if !st.Disabled {
			total++
		}
	}
	s.emitTask("activity", "", fmt.Sprintf("活跃上报开始（%d 个账号 × %d 条）", total, count))
	var reported, streaks int
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := api.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity platform=%s uid=%s: report %d/%d: %v", s.cfg.Name, a.UID, i, count, err)
				break
			}
			ok++
			if i < count {
				time.Sleep(activityReportGap)
			}
		}
		if ok < count {
			s.emitTask("activity", a.UID, fmt.Sprintf("上报中断（%d/%d 条）", ok, count))
			continue
		}
		reported++
		days, err := api.GrowthStreak(a)
		switch {
		case err != nil:
			log.Printf("WARN: activity platform=%s uid=%s: streak check failed: %v", s.cfg.Name, a.UID, err)
			s.emitTask("activity", a.UID, "上报完成，连登回读失败")
		case days == 0:
			log.Printf("WARN: activity platform=%s uid=%s: report OK but streak.days=0 (silent drop?)", s.cfg.Name, a.UID)
			s.emitTask("activity", a.UID, "上报完成，但连登未点亮（疑似静默丢弃）")
		default:
			log.Printf("activity platform=%s uid=%s: streak days=%d", s.cfg.Name, a.UID, days)
			streaks++
			s.emitTask("activity", a.UID, fmt.Sprintf("上报完成，连登 %d 天", days))
		}
		// 无猫账号对话量刚补满 → 立即重试领养（豁免当日防抖）
		if buddy, err := api.BuddyInfo(a); err == nil && buddy == nil {
			before := s.travelCounters()
			s.adoptBuddy(api, a, true)
			if s.travelCounters().adopts > before.adopts {
				s.emitTask("activity", a.UID, "领养成功 +300 积分")
			}
		}
	}
	s.emitTask("activity", "", fmt.Sprintf("活跃上报完成：%d/%d 个号发满，连登点亮 %d", reported, total, streaks))
}

// activityCount 读取每号上报条数（默认 5，配置可改）。
func (s *Scheduler) activityCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.ActivityReportCount > 0 {
		return s.cfg.ActivityReportCount
	}
	return 5
}

// RunClaimsNow 立即领取全部到站奖励（幂等：只对 arrived 状态动作，其余跳过）。
// 到站猫当日不能再派出（旅行每天 1 趟），领奖即当日终点——不必等下一轮巡检。
func (s *Scheduler) RunClaimsNow() {
	api := s.travelCap()
	if api == nil {
		return
	}
	if !s.travelMu.TryLock() {
		log.Printf("claim platform=%s: busy, skip (travel running)", s.cfg.Name)
		return
	}
	defer s.travelMu.Unlock()
	s.emitTask("travel", "", "领奖开始（仅到站账号）")
	var claimed, credits int64
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		buddy, err := api.BuddyInfo(a)
		if err != nil || buddy == nil {
			continue
		}
		ts, err := api.TravelStatus(a)
		if err != nil {
			continue
		}
		if ts.State != travelStateArrived || ts.RecordID == 0 {
			continue
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		reward, err := api.TravelClaim(a, ts.RecordID)
		if err != nil {
			log.Printf("claim platform=%s uid=%s: claim record=%d: %v", s.cfg.Name, a.UID, ts.RecordID, err)
			s.emitTask("travel", a.UID, fmt.Sprintf("领奖失败：%v", err))
			continue
		}
		claimed++
		credits += reward
		s.bumpTravel(func(t *travelCountersSnapshot) { t.claims++; t.claimCredits += reward })
		s.refreshCredits(a, "claim")
		s.emitTask("travel", a.UID, fmt.Sprintf("领奖 +%d 积分", reward))
	}
	s.emitTask("travel", "", fmt.Sprintf("领奖完成：%d 次 +%d 积分", claimed, credits))
}
