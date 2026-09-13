// lottery.go 抽奖子页编排：开学季转盘 + 成长中心连登抽奖，互不自动串联。
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

const lotteryWriteGap = 1500 * time.Millisecond

type lotteryAPI interface {
	LotteryChances(a *auth.Auth) (int, error)
	LotteryDraw(a *auth.Auth) (json.RawMessage, error)
	SchoolConfigFetch(a *auth.Auth) (*upstream.SchoolConfig, error)
	SchoolWheelDraw(a *auth.Auth, drawUUID string) (*upstream.SchoolDrawResult, error)
}

func (a *App) workbuddyLotteryAPI() (lotteryAPI, *Runtime) {
	rt := a.runtime(provider.WorkBuddy)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, nil
	}
	api, ok := rt.Upstream.(lotteryAPI)
	if !ok {
		return nil, nil
	}
	return api, rt
}

func (a *App) lotteryAcct(uid string) (lotteryAPI, *auth.Auth, error) {
	api, rt := a.workbuddyLotteryAPI()
	if api == nil {
		return nil, nil, fmt.Errorf("workbuddy 平台未启用")
	}
	acct := rt.Pool.AuthByUID(uid)
	if acct == nil {
		return nil, nil, fmt.Errorf("unknown account %s", uid)
	}
	return api, acct, nil
}

// LotteryStatus 单账号两类抽奖余额。
func (a *App) LotteryStatus(uid string) (map[string]any, error) {
	api, acct, err := a.lotteryAcct(uid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"uid":      uid,
		"nickname": acct.Nickname,
		"streak":   map[string]any{"chances": 0, "error": ""},
		"school":   map[string]any{"in_period": false, "chance": upstream.SchoolChance{}, "prizes": []any{}, "error": ""},
	}
	if n, err := api.LotteryChances(acct); err != nil {
		out["streak"] = map[string]any{"chances": 0, "error": err.Error()}
	} else {
		out["streak"] = map[string]any{"chances": n, "error": ""}
	}
	if cfg, err := api.SchoolConfigFetch(acct); err != nil {
		out["school"] = map[string]any{"in_period": false, "chance": upstream.SchoolChance{}, "error": err.Error()}
	} else {
		out["school"] = map[string]any{
			"in_period": cfg.InPeriod,
			"start_at":  cfg.StartAt,
			"end_at":    cfg.EndAt,
			"chance":    cfg.Chance,
			"prizes":    cfg.Prizes,
			"error":     "",
		}
	}
	return out, nil
}

// LotteryStatusAll 全部可用账号抽奖余额（串行，防风控）。
func (a *App) LotteryStatusAll() (map[string]any, error) {
	api, rt := a.workbuddyLotteryAPI()
	if api == nil {
		return nil, fmt.Errorf("workbuddy 平台未启用")
	}
	var accounts []map[string]any
	for _, st := range rt.Pool.List() {
		if st.Disabled {
			continue
		}
		row, err := a.LotteryStatus(st.UID)
		if err != nil {
			accounts = append(accounts, map[string]any{"uid": st.UID, "nickname": st.Nickname, "error": err.Error()})
			continue
		}
		accounts = append(accounts, row)
	}
	return map[string]any{"accounts": accounts}, nil
}

var lotteryBatchMu sync.Mutex
var lotteryBatchBusy bool

func tryLotteryBatch() bool {
	lotteryBatchMu.Lock()
	defer lotteryBatchMu.Unlock()
	if lotteryBatchBusy {
		return false
	}
	lotteryBatchBusy = true
	return true
}

func endLotteryBatch() {
	lotteryBatchMu.Lock()
	lotteryBatchBusy = false
	lotteryBatchMu.Unlock()
}

// RunLotteryAccount 抽空指定账号的一类抽奖。kind=streak|school|all
func (a *App) RunLotteryAccount(uid, kind string) (map[string]any, error) {
	api, acct, err := a.lotteryAcct(uid)
	if err != nil {
		return nil, err
	}
	if !a.tryLockTask(uid) {
		return nil, fmt.Errorf("该账号有任务动作正在执行中")
	}
	defer a.unlockTask(uid)
	return a.runLotteryLocked(api, acct, kind)
}

func (a *App) runLotteryLocked(api lotteryAPI, acct *auth.Auth, kind string) (map[string]any, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = "all"
	}
	uid := acct.UID
	emit := func(msg string) { a.NotifyTaskEvent("lottery", uid, msg) }
	out := map[string]any{"ok": true, "uid": uid, "kind": kind}
	var parts []string
	if kind == "streak" || kind == "all" {
		r := a.drawStreak(api, acct, emit)
		out["streak"] = r
		if msg, _ := r["message"].(string); msg != "" {
			parts = append(parts, "连登："+msg)
		}
	}
	if kind == "school" || kind == "all" {
		r := a.drawSchoolWheel(api, acct, emit)
		out["school"] = r
		if msg, _ := r["message"].(string); msg != "" {
			parts = append(parts, "开学季："+msg)
		}
	}
	out["message"] = strings.Join(parts, "；")
	return out, nil
}

func (a *App) drawStreak(api lotteryAPI, acct *auth.Auth, emit func(string)) map[string]any {
	n, err := api.LotteryChances(acct)
	if err != nil {
		msg := "查询失败：" + err.Error()
		emit("连登抽奖 " + msg)
		return map[string]any{"ok": false, "message": msg, "drawn": 0}
	}
	if n <= 0 {
		emit("连登抽奖次数=0")
		return map[string]any{"ok": true, "message": "次数为 0", "drawn": 0, "chances": 0}
	}
	emit(fmt.Sprintf("连登抽奖开始 %d 次", n))
	var prizes []any
	drawn := 0
	for i := 0; i < n; i++ {
		raw, err := api.LotteryDraw(acct)
		if err != nil {
			msg := fmt.Sprintf("第 %d 抽失败：%v", i+1, err)
			emit("连登 " + msg)
			return map[string]any{"ok": false, "message": msg, "drawn": drawn, "prizes": prizes}
		}
		prizes = append(prizes, json.RawMessage(raw))
		drawn++
		emit(fmt.Sprintf("连登第 %d 抽 %s", i+1, compactPrize(raw)))
		if i < n-1 {
			time.Sleep(lotteryWriteGap)
		}
	}
	msg := fmt.Sprintf("抽完 %d 次", drawn)
	emit("连登抽奖 " + msg)
	return map[string]any{"ok": true, "message": msg, "drawn": drawn, "prizes": prizes}
}

func (a *App) drawSchoolWheel(api lotteryAPI, acct *auth.Auth, emit func(string)) map[string]any {
	cfg, err := api.SchoolConfigFetch(acct)
	if err != nil {
		msg := "查询失败：" + err.Error()
		emit("开学季转盘 " + msg)
		return map[string]any{"ok": false, "message": msg, "drawn": 0}
	}
	if !cfg.InPeriod {
		emit("开学季转盘非进行期")
		return map[string]any{"ok": true, "skipped": true, "message": "活动非进行期", "drawn": 0}
	}
	bal := cfg.Chance.Balance
	if bal <= 0 {
		emit("开学季转盘余额=0")
		return map[string]any{"ok": true, "message": "余额为 0", "drawn": 0, "balance": 0}
	}
	emit(fmt.Sprintf("开学季转盘开始 %d 次", bal))
	var prizes []any
	drawn := 0
	totalCredit := 0
	prev := bal
	stall := 0
	for bal > 0 {
		res, err := api.SchoolWheelDraw(acct, clientUUID())
		if err != nil {
			if upstream.IsSchoolNoChance(err) {
				emit("开学季转盘次数耗尽")
				break
			}
			msg := "抽奖失败：" + err.Error()
			emit("开学季转盘 " + msg)
			return map[string]any{"ok": false, "message": msg, "drawn": drawn, "prizes": prizes, "credit": totalCredit}
		}
		prizes = append(prizes, res)
		drawn++
		totalCredit += res.CreditAmount
		bal = res.ChanceBalance
		if bal >= prev {
			stall++
			if stall >= 3 {
				emit("开学季转盘余额未递减，提前停")
				break
			}
		} else {
			stall = 0
		}
		prev = bal
		emit(fmt.Sprintf("转盘 → %s（余额 %d）", res.Label, bal))
		if bal > 0 {
			time.Sleep(lotteryWriteGap)
		}
	}
	msg := fmt.Sprintf("抽出 %d 次，积分 +%d", drawn, totalCredit)
	emit("开学季转盘 " + msg)
	return map[string]any{"ok": true, "message": msg, "drawn": drawn, "credit": totalCredit, "prizes": prizes, "balance": bal}
}

func compactPrize(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func clientUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// RunLotteryAll 异步串行抽空全部可用账号。kind=streak|school|all
func (a *App) RunLotteryAll(kind string) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = "all"
	}
	if !tryLotteryBatch() {
		return fmt.Errorf("抽奖批量正在执行")
	}
	api, rt := a.workbuddyLotteryAPI()
	if api == nil {
		endLotteryBatch()
		return fmt.Errorf("workbuddy 平台未启用")
	}
	var uids []string
	for _, st := range rt.Pool.List() {
		if st.Disabled {
			continue
		}
		uids = append(uids, st.UID)
	}
	if len(uids) == 0 {
		endLotteryBatch()
		return fmt.Errorf("没有可用账号")
	}
	go func() {
		defer endLotteryBatch()
		a.NotifyTaskEvent("lottery", "", fmt.Sprintf("抽奖批量开始（%d 个账号 · %s）", len(uids), kind))
		for i, uid := range uids {
			acct := rt.Pool.AuthByUID(uid)
			if acct == nil {
				continue
			}
			a.NotifyTaskEvent("lottery", uid, fmt.Sprintf("[%d/%d] 开始", i+1, len(uids)))
			if !a.tryLockTask(uid) {
				a.NotifyTaskEvent("lottery", uid, "跳过：账号忙碌")
				continue
			}
			_, err := a.runLotteryLocked(api, acct, kind)
			a.unlockTask(uid)
			if err != nil {
				a.NotifyTaskEvent("lottery", uid, "失败："+err.Error())
				log.Printf("lottery %s: %v", uid, err)
			}
			if i < len(uids)-1 {
				time.Sleep(lotteryWriteGap)
			}
		}
		a.NotifyTaskEvent("lottery", "", fmt.Sprintf("抽奖批量结束（%d 个账号）", len(uids)))
	}()
	return nil
}
