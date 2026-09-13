// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者。
package pool

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"wild-work/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard       CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                       // 429 → 短冷却
	CoolErr                        // 连续错误 → 中冷却
	CoolLowBalance                 // 余额低于平均 60% → 15min 冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	case CoolLowBalance:
		return "low_balance"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID            string    `json:"uid"`
	Nickname       string    `json:"nickname,omitempty"`
	Credits        int64     `json:"credits"`
	Cooling        bool      `json:"cooling"`
	Until          time.Time `json:"until,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Disabled       bool      `json:"disabled"`
	ErrCount       int       `json:"err_count,omitempty"`
	LastCheckinOK  bool      `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string    `json:"last_checkin_msg,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int

	// sessionDeadFails 连续 12153 计数（内存态，重启清零）：
	// 单次 session-dead 判定大多是接口抖动误报，连续 N 次才真正禁用。
	sessionDeadFails int

	// modelCool 模型级冷却截止（429 code 6004，上游明说重置时刻）：
	// 只冷却触发模型，账号对其他模型保持可用。内存态，重启清零。
	modelCool map[string]time.Time

	lastCheckinOK  bool
	lastCheckinAt  time.Time
	lastCheckinMsg string
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type accountState struct {
	Credits  int64     `json:"credits"`
	Disabled bool      `json:"disabled"`
	Reason   string    `json:"reason,omitempty"`
	Until    time.Time `json:"until,omitempty"`
	// ModelCool 模型级冷却（6004）：model → 重置墙钟。持久化避免重启后
	// 已知限额信息丢失、反复重撞 429 才重建。
	ModelCool      map[string]time.Time `json:"model_cool,omitempty"`
	LastCheckinOK  bool                 `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time            `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string               `json:"last_checkin_msg,omitempty"`
}

type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 账号（top-K 随机）；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// pickTopK healthy 候选按积分降序取前 K 个，随机命中其一。
// 纯"最高积分"会让全部流量压在同一号上直到 429/欠费——尤其粘性过期后
// 重新 Pick 又选回同一号，"50 次轮换"形同虚设。top-3 随机兼顾余额
// 均衡与负载分散。
const pickTopK = 3

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var cand []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		cand = append(cand, e)
	}
	if len(cand) == 0 {
		return nil
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].credits > cand[j].credits })
	if len(cand) > pickTopK {
		cand = cand[:pickTopK]
	}
	return cand[rand.Intn(len(cand))].a
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// SetDisabled 设置账号禁用/启用状态。
func (p *Pool) SetDisabled(uid string, d bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = d
		if d {
			e.reason = "手动停用"
		} else {
			e.reason = ""
		}
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// RecordCheckin 记录一次签到结果（含错误信息），随 state.json 持久化。
func (p *Pool) RecordCheckin(uid string, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCheckinOK = ok
		e.lastCheckinAt = time.Now()
		e.lastCheckinMsg = msg
	}
	p.saveLocked()
}

// Remove 从池中移除账号（内存 + 状态文件）；auth 文件删除由调用方负责。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
		e.sessionDeadFails = 0
	}
}

// CooldownSoftForModel 模型级软冷却：账号只对触发模型冷却到 resetAt
// （封顶 2h，防上游文案时钟异常导致超长冷却），其他模型不受影响。
func (p *Pool) CooldownSoftForModel(uid string, resetAt time.Time, model, reason string) {
	if model == "" || resetAt.IsZero() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	// 上游 6004 为按天重置的模型用量限额（实测文案"将在 <次日> 重置"）。
	// cap 24h：既尊重上游重置墙钟（避免限期内反复重试打爆），又不至于
	// 因上游文案时钟异常锁死超过一天。
	if cap := time.Now().Add(24 * time.Hour); resetAt.After(cap) {
		resetAt = cap
	}
	if e.modelCool == nil {
		e.modelCool = map[string]time.Time{}
	}
	e.modelCool[model] = resetAt
	e.reason = reason
	p.saveLocked()
}

// CooledForModel 报告账号当前是否处于指定模型的冷却期（6004 语义）。
func (p *Pool) CooledForModel(uid, model string) bool {
	if model == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	until, ok := e.modelCool[model]
	return ok && time.Now().Before(until)
}

// ModelCoolEntry 单账号在某模型上的冷却明细。
type ModelCoolEntry struct {
	UID   string    `json:"uid"`
	Until time.Time `json:"until"`
}

// ModelCoolSummary 模型维度的限流聚合（面板「模型限流」页数据源）。
type ModelCoolSummary struct {
	Model     string           `json:"model"`
	Cooled    []ModelCoolEntry `json:"cooled"`    // 被限账号（按重置时间升序）
	Total     int              `json:"total"`     // 未禁用账号总数
	Available int              `json:"available"` // 当前可用的账号数
}

// ModelCooldowns 聚合全部模型级冷却（6004），顺手清理过期项。
// 只返回有活跃冷却的模型。
func (p *Pool) ModelCooldowns() []ModelCoolSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	agg := map[string][]ModelCoolEntry{}
	total := 0
	for uid, e := range p.byUID {
		if e.disabled {
			continue
		}
		total++
		for model, until := range e.modelCool {
			if !now.Before(until) {
				delete(e.modelCool, model) // 过期清理
				continue
			}
			agg[model] = append(agg[model], ModelCoolEntry{UID: uid, Until: until})
		}
	}
	if len(agg) == 0 {
		return nil
	}
	out := make([]ModelCoolSummary, 0, len(agg))
	for model, entries := range agg {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Until.Before(entries[j].Until) })
		out = append(out, ModelCoolSummary{
			Model:     model,
			Cooled:    entries,
			Total:     total,
			Available: total - len(entries),
		})
	}
	// 被限账号最多的模型排前面（最受关注的先看）
	sort.Slice(out, func(i, j int) bool { return len(out[i].Cooled) > len(out[j].Cooled) })
	return out
}

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（调度日志/运维引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

const (
	// sessionDeadThreshold 连续 12153 次数达到阈值才禁用。上游实测：一次性禁用
	// 的号事后全部 refresh 成功——单次 12153 多为网络抖动/上游闪断误报。
	sessionDeadThreshold = 3
	sessionDeadReason    = "12153 session dead"
)

// NoteSessionDead 记录一次 ErrSessionDead（12153）——不立即禁用。
// 计数 +1，达到阈值 → Disable 并清计数；成功（NoteSuccess）清计数。
// 返回 true 表示本次已达阈值并完成禁用。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.disabled = true
	e.reason = sessionDeadReason
	e.sessionDeadFails = 0
	p.saveLocked()
	return true
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:            uid,
		Nickname:       e.a.Nickname,
		Credits:        e.credits,
		Cooling:        !e.until.IsZero() && now.Before(e.until),
		Until:          e.until,
		Reason:         e.reason,
		Disabled:       e.disabled,
		ErrCount:       e.errCount,
		LastCheckinOK:  e.lastCheckinOK,
		LastCheckinAt:  e.lastCheckinAt,
		LastCheckinMsg: e.lastCheckinMsg,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	now := time.Now()
	for uid, s := range sf.Accounts {
		modelCool := map[string]time.Time{}
		for m, until := range s.ModelCool {
			if now.Before(until) {
				modelCool[m] = until // 过期项不恢复
			}
		}
		p.byUID[uid] = &entry{
			a:              &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:        s.Credits,
			disabled:       s.Disabled,
			reason:         s.Reason,
			until:          s.Until,
			modelCool:      modelCool,
			lastCheckinOK:  s.LastCheckinOK,
			lastCheckinAt:  s.LastCheckinAt,
			lastCheckinMsg: s.LastCheckinMsg,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]accountState{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = accountState{
			Credits:        e.credits,
			Disabled:       e.disabled,
			Reason:         e.reason,
			Until:          e.until,
			LastCheckinOK:  e.lastCheckinOK,
			LastCheckinAt:  e.lastCheckinAt,
			LastCheckinMsg: e.lastCheckinMsg,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	_ = auth.WriteFileSync(p.stateFp, raw, 0o600)
}
