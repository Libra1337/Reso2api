// school.go 开学季活动编排：列表 / 单账号执行 / 批量执行；抽奖独立（lottery.go）。
package app

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

const schoolWriteGap = 1500 * time.Millisecond

type schoolAPI interface {
	SchoolTasks(a *auth.Auth) (*upstream.SchoolTasksResult, error)
	SchoolViewed(a *auth.Auth, code string) error
	SchoolShareComplete(a *auth.Auth) error
	SchoolClaim(a *auth.Auth, code string) error
	ReportSchoolMiniChat(a *auth.Auth) error
	ReportSchoolDesktopChat(a *auth.Auth) error
	ReportSchoolExpertUse(a *auth.Auth) error
}

func (a *App) workbuddySchoolAPI() (schoolAPI, *Runtime) {
	rt := a.runtime(provider.WorkBuddy)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, nil
	}
	api, ok := rt.Upstream.(schoolAPI)
	if !ok {
		return nil, nil
	}
	return api, rt
}

func (a *App) schoolAcct(uid string) (schoolAPI, *Runtime, *auth.Auth, error) {
	api, rt := a.workbuddySchoolAPI()
	if api == nil {
		return nil, nil, nil, fmt.Errorf("workbuddy 平台未启用")
	}
	acct := rt.Pool.AuthByUID(uid)
	if acct == nil {
		return nil, nil, nil, fmt.Errorf("unknown account %s", uid)
	}
	return api, rt, acct, nil
}

// SchoolList 开学季任务盘点。
func (a *App) SchoolList(uid string) (map[string]any, error) {
	api, _, acct, err := a.schoolAcct(uid)
	if err != nil {
		return nil, err
	}
	res, err := api.SchoolTasks(acct)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"uid":       uid,
		"nickname":  acct.Nickname,
		"in_period": res.InPeriod,
		"tasks":     res.Tasks,
	}, nil
}

var schoolBatchMu sync.Mutex
var schoolBatchBusy bool

func (a *App) trySchoolBatch() bool {
	schoolBatchMu.Lock()
	defer schoolBatchMu.Unlock()
	if schoolBatchBusy {
		return false
	}
	schoolBatchBusy = true
	return true
}

func (a *App) endSchoolBatch() {
	schoolBatchMu.Lock()
	schoolBatchBusy = false
	schoolBatchMu.Unlock()
}

// RunSchoolAccount 同步执行单账号开学季可自动任务（不含抽奖、不含学生认证）。
func (a *App) RunSchoolAccount(uid string) (map[string]any, error) {
	api, _, acct, err := a.schoolAcct(uid)
	if err != nil {
		return nil, err
	}
	if !a.tryLockTask(uid) {
		return nil, fmt.Errorf("该账号有任务动作正在执行中")
	}
	defer a.unlockTask(uid)
	return a.runSchoolAccountLocked(api, acct)
}

func (a *App) runSchoolAccountLocked(api schoolAPI, acct *auth.Auth) (map[string]any, error) {
	uid := acct.UID
	emit := func(msg string) { a.NotifyTaskEvent("school", uid, msg) }
	res, err := api.SchoolTasks(acct)
	if err != nil {
		return nil, err
	}
	if !res.InPeriod {
		emit("活动非进行期，跳过")
		return map[string]any{"ok": true, "skipped": true, "message": "活动非进行期", "in_period": false}, nil
	}
	stats := map[string]int{"ok": 0, "already": 0, "skip": 0, "fail": 0, "pending": 0}
	var lines []string
	for _, t := range res.Tasks {
		line := a.runSchoolTask(api, acct, t, emit)
		lines = append(lines, line)
		switch {
		case strings.Contains(line, "已完成") || strings.Contains(line, "已领"):
			stats["already"]++
		case strings.Contains(line, "人工") || strings.Contains(line, "未知"):
			stats["skip"]++
		case strings.Contains(line, "失败"):
			stats["fail"]++
		case strings.Contains(line, "点亮") || strings.Contains(line, "领奖"):
			stats["ok"]++
		default:
			stats["pending"]++
		}
	}
	msg := fmt.Sprintf("开学季完成：点亮/领奖 %d · 已完成 %d · 跳过 %d · 失败 %d",
		stats["ok"], stats["already"], stats["skip"], stats["fail"])
	emit(msg)
	return map[string]any{
		"ok":        stats["fail"] == 0,
		"message":   msg,
		"stats":     stats,
		"lines":     lines,
		"in_period": true,
	}, nil
}

func (a *App) runSchoolTask(api schoolAPI, acct *auth.Auth, t upstream.SchoolTask, emit func(string)) string {
	uid8 := acct.UID
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	code := t.TaskCode
	status := strings.ToLower(t.Status)
	if code == "" {
		return "空 task_code，跳过"
	}
	if status == "completed" || status == "claimed" {
		msg := code + "：" + status + " 已完成/已领，跳过"
		emit(msg)
		return msg
	}
	switch t.Mode {
	case upstream.SchoolModeManual:
		msg := code + "：人工环节（" + t.Note + "），跳过"
		emit(msg)
		return msg
	case upstream.SchoolModeUnknown:
		msg := code + "：未知任务（" + t.Title + "），保守跳过"
		emit(msg)
		return msg
	}
	if status == "pending" {
		if err := api.SchoolViewed(acct, code); err != nil {
			msg := code + "：viewed 失败：" + err.Error()
			emit(msg)
			return msg
		}
		emit(code + "：viewed pending→in_progress")
		time.Sleep(schoolWriteGap)
	}
	rounds := 1
	if t.Mode == upstream.SchoolModeReport && t.ReportKind != "desktop_seq" {
		need := t.TargetCount - t.Progress
		if need < 1 {
			need = 1
		}
		rounds = int(need)
	}
	progressChanged := false
	cur := t.Progress
	for ri := 0; ri < rounds; ri++ {
		var err error
		if t.Mode == upstream.SchoolModeShare {
			err = api.SchoolShareComplete(acct)
		} else {
			switch t.ReportKind {
			case "mini_chat":
				err = api.ReportSchoolMiniChat(acct)
			case "desktop_seq":
				err = api.ReportSchoolDesktopChat(acct)
			case "expert":
				err = api.ReportSchoolExpertUse(acct)
			default:
				err = fmt.Errorf("未知 report_kind %s", t.ReportKind)
			}
		}
		if err != nil {
			msg := fmt.Sprintf("%s：触发 #%d 失败：%v", code, ri+1, err)
			emit(msg)
			return msg
		}
		time.Sleep(schoolWriteGap)
		t2 := refetchSchoolTask(api, acct, code)
		if t2 == nil {
			continue
		}
		after := strings.ToLower(t2.Status)
		if t2.Progress > cur {
			cur = t2.Progress
			progressChanged = true
		}
		if after == "completed" || after == "claimed" || t2.Progress >= t.TargetCount {
			break
		}
	}
	t2 := refetchSchoolTask(api, acct, code)
	if t2 == nil {
		msg := code + "：回读失败"
		emit(msg)
		return msg
	}
	after := strings.ToLower(t2.Status)
	if after == "completed" {
		if err := api.SchoolClaim(acct, code); err != nil {
			msg := code + "：点亮完成但领奖失败：" + err.Error()
			emit(msg)
			return msg
		}
		time.Sleep(schoolWriteGap)
		msg := fmt.Sprintf("%s：%s/%d → claimed/%d 领奖成功", code, status, t.Progress, t2.Progress)
		emit(msg)
		return msg
	}
	if after == "claimed" || progressChanged {
		msg := fmt.Sprintf("%s：%s/%d → %s/%d（点亮）", code, status, t.Progress, after, t2.Progress)
		emit(msg)
		return msg
	}
	msg := fmt.Sprintf("%s：%s → %s（未变化）", code, status, after)
	emit(msg)
	return msg
}

func refetchSchoolTask(api schoolAPI, acct *auth.Auth, code string) *upstream.SchoolTask {
	res, err := api.SchoolTasks(acct)
	if err != nil {
		return nil
	}
	for i := range res.Tasks {
		if res.Tasks[i].TaskCode == code {
			return &res.Tasks[i]
		}
	}
	return nil
}

// RunSchoolAll 异步串行跑全部可用 workbuddy 账号。
func (a *App) RunSchoolAll() error {
	if !a.trySchoolBatch() {
		return fmt.Errorf("开学季批量任务正在执行")
	}
	api, rt := a.workbuddySchoolAPI()
	if api == nil {
		a.endSchoolBatch()
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
		a.endSchoolBatch()
		return fmt.Errorf("没有可用账号")
	}
	go func() {
		defer a.endSchoolBatch()
		a.NotifyTaskEvent("school", "", fmt.Sprintf("开学季批量开始（%d 个账号，串行防风控）", len(uids)))
		for i, uid := range uids {
			acct := rt.Pool.AuthByUID(uid)
			if acct == nil {
				a.NotifyTaskEvent("school", uid, fmt.Sprintf("[%d/%d] 跳过：账号不存在", i+1, len(uids)))
				continue
			}
			nick := uid
			if acct.Nickname != "" {
				nick = acct.Nickname
			}
			a.NotifyTaskEvent("school", uid, fmt.Sprintf("[%d/%d] %s 开始", i+1, len(uids), nick))
			if !a.tryLockTask(uid) {
				a.NotifyTaskEvent("school", uid, "跳过：该账号有任务正在执行")
				continue
			}
			_, err := a.runSchoolAccountLocked(api, acct)
			a.unlockTask(uid)
			if err != nil {
				a.NotifyTaskEvent("school", uid, "失败："+err.Error())
				log.Printf("school %s: %v", uid, err)
			}
			if i < len(uids)-1 {
				time.Sleep(schoolWriteGap)
			}
		}
		a.NotifyTaskEvent("school", "", fmt.Sprintf("开学季批量结束（%d 个账号）", len(uids)))
	}()
	return nil
}
