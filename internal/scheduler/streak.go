// streak.go 连登管家：签到排程后自动检查连登兑换档位 → 可兑换即兑换 → 按抽奖次数抽奖。
//
// 背景（2026-09-12）：成长中心连登档位（7d/14d/28d）按连续登录天数解锁，兑换发
// credit/energy/补签卡/抽奖次数；抽奖次数只能从兑换获得。兑换按钮在 UI 上恒可点，
// 但未解锁时服务端 403「连续登录天数不足」——所以放在每日签到后跑一遍（幂等），
// 到天数那天自动完成「兑换 → 抽奖」闭环，无需人工盯。
package scheduler

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/upstream"
)

// streakAPI 连登管家所需的 growth 能力（仅 workbuddy 上游实现）。
type streakAPI interface {
	BuddyQuota(a *auth.Auth) (affordable, maxOpen int, err error)
	BuddyOpen(a *auth.Auth, count int) (string, error)
	ClaimGift(a *auth.Auth) (int64, error)
	ClaimCompensation(a *auth.Auth) (int64, error)
	GrowthStreakFull(a *auth.Auth) (*upstream.StreakFull, error)
	GrowthRedeemTier(a *auth.Auth, tier string) error
	LotteryChances(a *auth.Auth) (int, error)
	LotteryDraw(a *auth.Auth) (json.RawMessage, error)
	HeatmapYesterdayMissed(a *auth.Auth) (bool, error)
	UseMakeupCard(a *auth.Auth, date string) error
}

// RunStreakBonusNow 对所有可用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow() {
	api, ok := s.cfg.Upstream.(streakAPI)
	if !ok || s.cfg.Pool == nil {
		return
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		s.streakBonusAccount(api, a)
	}
}

// streakBonusAccount 单账号：补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
func (s *Scheduler) streakBonusAccount(api streakAPI, a *auth.Auth) {
	// 0. 补签保连登：昨日漏签且有补签卡则补上（连续天数一断就要重攒 7 天）。
	s.makeupYesterday(api, a)
	// 0.5 礼包/补偿（每号一次，无则业务错误静默跳过）。
	if credit, err := api.ClaimGift(a); err == nil {
		log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc", a.UID, credit)
	}
	if credit, err := api.ClaimCompensation(a); err == nil {
		log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc", a.UID, credit)
	}

	full, err := api.GrowthStreakFull(a)
	if err != nil {
		log.Printf("streak-bonus %s: %v", a.UID, err)
		return
	}
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := api.GrowthRedeemTier(a, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("streak-bonus %s: redeem %s: %v", a.UID, tier.Tier, err)
			continue
		}
		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			a.UID, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := api.LotteryChances(a)
	if err != nil {
		log.Printf("streak-bonus %s: lottery summary: %v", a.UID, err)
		return
	}
	for i := 0; i < chances; i++ {
		raw, err := api.LotteryDraw(a)
		if err != nil {
			log.Printf("streak-bonus %s: draw: %v", a.UID, err)
			return
		}
		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", a.UID, i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", a.UID, chances)
	}
	// Buddy 盲盒：能量没有其它消耗出口，攒够就抽（自动signin 同款口径）。
	if affordable, maxOpen, err := api.BuddyQuota(a); err == nil && affordable > 0 {
		count := affordable
		if count > maxOpen {
			count = maxOpen
		}
		if name, oerr := api.BuddyOpen(a, count); oerr == nil {
			log.Printf("streak-bonus %s: 🎁 Buddy 盲盒 ×%d（%s）", a.UID, count, name)
			s.emitTask("activity", a.UID, fmt.Sprintf("Buddy 盲盒 ×%d（%s）", count, name))
		}
	}
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 无卡 / 无漏签 / 查询失败均静默（不影响主流程）。
func (s *Scheduler) makeupYesterday(api streakAPI, a *auth.Auth) {
	missed, err := api.HeatmapYesterdayMissed(a)
	if err != nil || !missed {
		return
	}
	full, err := api.GrowthStreakFull(a)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := api.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("streak-bonus %s: 补签 %s 失败: %v", a.UID, yesterday, err)
		return
	}
	log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", a.UID, yesterday)
}
