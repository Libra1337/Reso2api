// autotask.go 成长任务「一键完成」执行器。
// 移植自 linguo2625469/workbuddy2api-panel（panel/autotask.go），
// 适配本网关结构：能力断言替代胖接口、任务事件流、per-account 互斥。
//
// 任务点亮机制（上游实测结论，2026-09-12 逆向）：各任务校验不同客户端
// 指纹（CLI / workbuddy-desktop / web）的行为事件链；进度异步计分；
// 领奖走 Web 域 /activity/growth/tasks/<code>/claim。所有动作幂等。
package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// taskAPI 成长任务所需的全部上游能力（仅 workbuddy Client 实现）。
type taskAPI interface {
	ListTasks(a *auth.Auth) ([]upstream.Task, error)
	AcceptTasks(a *auth.Auth, taskCodes []string) error
	ClaimReward(a *auth.Auth, taskCode string) (credit, energy int64, err error)
	ReportChatActivity(a *auth.Auth, conversationID, requestID string) error
	ReportChatActivityModel(a *auth.Auth, conversationID, modelID, modelName string) error
	ReportDesktopEvent(a *auth.Auth, events ...upstream.DesktopEvent) error
	ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error
	SetAppearanceTheme(a *auth.Auth, resourceKey string) error
	MarketExpertList(a *auth.Auth, expertType string) ([]upstream.MarketExpert, error)
	DesktopChatWithExpert(a *auth.Auth, expertID string) (conversationID, requestID string, err error)
	BlackcatNeed(a *auth.Auth) (int64, error)
	RunNightChats(a *auth.Auth, need int) (int64, error)
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	BuddyAgreement(a *auth.Auth) error
	BuddyFirst(a *auth.Auth) error
	UserResource(a *auth.Auth) (int64, error)
}

// autoAction 一个可自动化的任务动作。
type autoAction struct {
	TaskCode string
	Desc     string
	Attempt  bool // true = 尝试型（窗口期/上游未证实）
	run      func(p *taskRunner, a *auth.Auth) (string, error)
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
var autoActions = []autoAction{
	{TaskCode: "chat_5", Desc: "上报 5 条对话活跃事件（自动补足差额）", run: runChat5},
	{TaskCode: "first_buddy", Desc: "上报解锁 → 同意协议 → 领取 Buddy（+300 分）", run: runFirstBuddy},
	{TaskCode: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话一次 → 对齐模型上报", run: runModelChat},
	{TaskCode: "RichMeow_Chat", Desc: "桌面指纹对话事件链（实测可点亮）", run: runRichMeow},
	{TaskCode: "Buddy_App", Desc: "「进入 Buddy 应用」事件链（实测可点亮）", run: runBuddyApp},
	{TaskCode: "Buddy_App_QQ", Desc: "「进入企鹅教师助手」事件链（实测可点亮）", run: runBuddyApp},
	{TaskCode: "automation_1", Desc: "「定时任务创建」事件（实测可点亮）", run: runAutomationCreate},
	{TaskCode: "Library_read", Desc: "「读资料库介绍」Web 事件（实测可点亮）", run: runLibraryRead},
	{TaskCode: "template_5", Desc: "「使用模板创建任务」事件组 ×5（实测可点亮）", run: runTemplateUse},
	{TaskCode: "playbook_prompt", Desc: "「灵感案例做同款」事件组（实测可点亮）", run: runPlaybookPrompt},
	{TaskCode: "create_canvas", Desc: "「设计创意画布」事件组（+300 分，实测可点亮）", run: runCreateCanvas},
	{TaskCode: "expert_5", Desc: "真实专家召唤+使用链 ×5（实测可点亮）", run: runExpertUse},
	{TaskCode: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3（实测可点亮）", run: runExpertTeamUse},
	{TaskCode: "Hp_Appearance", Desc: "设置主题 API + 皮肤生效事件（实测可点亮）", run: runAppearance},
	{TaskCode: "skill_1", Desc: "真实对话 + skill_info 技能加载事件（实测可点亮）", run: runSkillFresh},
	{TaskCode: "Expert_lighthouse", Desc: "真实轻量云专家召唤+使用链（实测可点亮）", run: runExpertLighthouse},
	{TaskCode: "black_cat", Desc: "夜猫子：23:00–08:00 窗口 glm-5.2 对话补足", Attempt: true, run: runBlackCat},
}

func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == strings.TrimSpace(code) {
			return &autoActions[i]
		}
	}
	return nil
}

// AutoActionsMeta 暴露动作表元信息（面板展示）。
func AutoActionsMeta() []map[string]any {
	out := make([]map[string]any, 0, len(autoActions))
	for _, a := range autoActions {
		out = append(out, map[string]any{"task_code": a.TaskCode, "desc": a.Desc, "attempt": a.Attempt})
	}
	return out
}

// taskRunner 单账号执行上下文。
type taskRunner struct {
	api  taskAPI
	emit func(msg string) // 任务事件流回调
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询（上游计分异步，实测 5-8 秒）。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
	reportGap         = 1050 * time.Millisecond // 上游脚本实测 1.05s 风控口径
)

func (p *taskRunner) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.api.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

func (p *taskRunner) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	t, err := p.taskByCode(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCode(a, code)
		if err2 != nil {
			return t, nil
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// 各任务动作
// ---------------------------------------------------------------------------

func runChat5(p *taskRunner, a *auth.Auth) (string, error) {
	t, err := p.taskByCode(a, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := p.api.ReportChatActivity(a, cid, cid); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

func runFirstBuddy(p *taskRunner, a *auth.Auth) (string, error) {
	cid := fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli())
	if err := p.api.ReportChatActivity(a, cid, cid); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(reportGap)
	if err := p.api.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := p.api.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

func runModelChat(p *taskRunner, a *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	if err := p.api.AcceptTasks(a, []string{code}); err != nil {
		log.Printf("accept %s: %v（继续走行为链路）", code, err)
	}
	time.Sleep(reportGap)
	body, _ := json.Marshal(map[string]any{
		"model":    modelID,
		"messages": []map[string]any{{"role": "user", "content": "hi，请回复一句话"}},
		"stream":   true,
	})
	rc, status, respBody, err := p.api.ChatStream(a, body)
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("对话失败 http=%d: %s", status, truncateStr(string(respBody), 160))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	time.Sleep(reportGap)
	cid := fmt.Sprintf("wb2api-glm52-%d", time.Now().UnixMilli())
	if err := p.api.ReportChatActivityModel(a, cid, modelID, modelName); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil
	}
	return "已完成 glm-5.2 对话并上报", nil
}

func runRichMeow(p *taskRunner, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	events := upstream.DesktopChatSequence(
		fmt.Sprintf("wb2api-rm-%d", ms), fmt.Sprintf("wb2api-rm-req-%d", ms),
		fmt.Sprintf("req-%d-user", ms), "fast-model", "fast-model")
	if err := p.api.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链", nil
}

func runBuddyApp(p *taskRunner, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := p.api.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

func runAutomationCreate(p *taskRunner, a *auth.Auth) (string, error) {
	if err := p.api.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("wb2api 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

func runLibraryRead(p *taskRunner, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.api.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

func runBlackCat(p *taskRunner, a *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00 计数窗口；网关每日 23 点自动补足", nil
	}
	need, err := p.api.BlackcatNeed(a)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	done, err := p.api.RunNightChats(a, int(need))
	if err != nil {
		return fmt.Sprintf("完成 %d/%d 次后中断: %v", done, need, err), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", done), nil
}

func runSkillFresh(p *taskRunner, a *auth.Auth) (string, error) {
	conv, req, err := p.api.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls"
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode": "skill_info", "id": "润泽小馆·日报撰写",
		"skillId": "skill_2097350077599879168", "skillVersion": "1.0.0",
		"toolStatus": "success", "fileCount": 56, "source": "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model", "traceId": req,
	})
	if err := p.api.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

func runExpertLighthouse(p *taskRunner, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	if experts, err := p.api.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := p.api.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := p.api.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := p.api.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链", nil
}

func runAppearance(p *taskRunner, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j"
	if err := p.api.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := p.api.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

func runTemplateUse(p *taskRunner, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		events := upstream.DesktopTemplateUseSequence(
			fmt.Sprintf("wb2api-tpl-%d-%d", ms, i), fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i), tp[0], tp[1])
		if err := p.api.ReportDesktopEvent(a, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败: %v", i+1, err), nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

func runPlaybookPrompt(p *taskRunner, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	events := upstream.DesktopPlaybookPromptSequence(
		fmt.Sprintf("wb2api-pb-%d", ms), fmt.Sprintf("wb2api-pb-req-%d", ms),
		"pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := p.api.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_prompt_send", nil
}

func runCreateCanvas(p *taskRunner, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	events := upstream.DesktopDesignCanvasSequence(
		fmt.Sprintf("wb2api-canvas-%d", ms), fmt.Sprintf("wb2api-canvas-req-%d", ms))
	if err := p.api.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas 事件组（+300 分）", nil
}

const expertSummonGap = 6 * time.Second

func runExpertUse(p *taskRunner, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "agent", 5)
}

func runExpertTeamUse(p *taskRunner, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "team", 3)
}

func runExpertBatch(p *taskRunner, a *auth.Auth, expertType string, count int) (string, error) {
	experts, err := p.api.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok := 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		if err := p.api.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(e)...); err != nil {
			continue
		}
		conv, req, cerr := p.api.DesktopChatWithExpert(a, e.ExpertID)
		if cerr != nil {
			continue
		}
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := p.api.ReportDesktopEvent(a, events...); err != nil {
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType), nil
}

// ---------------------------------------------------------------------------
// 执行入口（App 方法）
// ---------------------------------------------------------------------------

// taskLocks per-account 任务互斥。
var taskLocks = struct {
	sync.Mutex
	running map[string]bool
}{running: map[string]bool{}}

func (a *App) tryLockTask(uid string) bool {
	taskLocks.Lock()
	defer taskLocks.Unlock()
	if taskLocks.running[uid] {
		return false
	}
	taskLocks.running[uid] = true
	return true
}

func (a *App) unlockTask(uid string) {
	taskLocks.Lock()
	defer taskLocks.Unlock()
	delete(taskLocks.running, uid)
}

// workbuddyTaskAPI 取 workbuddy runtime 的任务能力视图。
func (a *App) workbuddyTaskAPI() (taskAPI, *Runtime) {
	rt := a.runtime(provider.WorkBuddy)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, nil
	}
	api, ok := rt.Upstream.(taskAPI)
	if !ok {
		return nil, nil
	}
	return api, rt
}

// RunTaskAuto 执行单账号单任务（同步，含达标自动领奖）。
func (a *App) RunTaskAuto(uid, taskCode string) (map[string]any, error) {
	api, rt := a.workbuddyTaskAPI()
	if api == nil {
		return nil, fmt.Errorf("workbuddy 平台未启用")
	}
	acct := rt.Pool.AuthByUID(uid)
	if acct == nil {
		return nil, fmt.Errorf("unknown account %s", uid)
	}
	act := autoActionFor(taskCode)
	if act == nil {
		return nil, fmt.Errorf("该任务无法自动完成（需客户端内交互）")
	}
	if !a.tryLockTask(uid) {
		return nil, fmt.Errorf("该账号有任务动作正在执行中")
	}
	defer a.unlockTask(uid)

	emit := func(msg string) { a.NotifyTaskEvent("task", uid, msg) }
	p := &taskRunner{api: api, emit: emit}

	before, err := p.taskByCode(acct, act.TaskCode)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	if before == nil {
		return nil, fmt.Errorf("该账号没有此任务")
	}
	if before.Claimed {
		return map[string]any{"ok": true, "skipped": true, "message": "该任务已领取过奖励"}, nil
	}
	emit(act.TaskCode + " 开始：" + act.Desc)
	msg, err := act.run(p, acct)
	if err != nil {
		emit(act.TaskCode + " 失败：" + err.Error())
		return nil, fmt.Errorf("执行失败: %w", err)
	}
	after, _ := p.taskByCodeWaiting(acct, act.TaskCode)
	resp := map[string]any{
		"ok": true, "message": msg,
		"progress_before": taskProgressText(before), "progress_after": taskProgressText(after),
	}
	if after != nil && after.Claimable {
		if credit, energy, cerr := api.ClaimReward(acct, act.TaskCode); cerr == nil {
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
				emit(fmt.Sprintf("%s 领奖 +%d 分", act.TaskCode, credit))
				if remain, rerr := rt.Upstream.UserResource(acct); rerr == nil {
					rt.Pool.SetCredits(uid, remain)
				}
			}
		}
	}
	emit(act.TaskCode + " 完成：" + resp["message"].(string))
	return resp, nil
}

// RunTaskAutoAll 单账号全量任务（异步，进度走任务事件流）。
func (a *App) RunTaskAutoAll(uid string) error {
	api, rt := a.workbuddyTaskAPI()
	if api == nil {
		return fmt.Errorf("workbuddy 平台未启用")
	}
	acct := rt.Pool.AuthByUID(uid)
	if acct == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if !a.tryLockTask(uid) {
		return fmt.Errorf("该账号有任务动作正在执行中")
	}
	a.NotifyTaskEvent("task", uid, "一键完成全部可自动任务开始（17 项，约 2-4 分钟）")
	a.safeGo(func() {
		defer a.unlockTask(uid)
		p := &taskRunner{api: api, emit: func(msg string) { a.NotifyTaskEvent("task", uid, msg) }}

		// 阶段 0：批量接受未接受任务（行为事件才是进度判据，失败不阻塞）。
		if tasks, err := api.ListTasks(acct); err == nil {
			var codes []string
			for _, t := range tasks {
				if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
					codes = append(codes, t.TaskCode)
				}
			}
			if len(codes) > 0 {
				_ = api.AcceptTasks(acct, codes)
				a.NotifyTaskEvent("task", uid, fmt.Sprintf("已批量接受 %d 个任务", len(codes)))
				time.Sleep(reportGap)
			}
		}

		var doneN, skipN, errN int
		var gained int64
		for _, act := range autoActions {
			before, err := p.taskByCode(acct, act.TaskCode)
			if err != nil || before == nil {
				skipN++
				continue
			}
			if before.Claimed || (before.Target > 0 && before.Current >= before.Target) {
				skipN++
				continue
			}
			msg, err := act.run(p, acct)
			if err != nil {
				errN++
				a.NotifyTaskEvent("task", uid, act.TaskCode+" 失败："+err.Error())
				continue
			}
			after, _ := p.taskByCodeWaiting(acct, act.TaskCode)
			line := act.TaskCode + "：" + msg
			if after != nil && after.Claimable {
				if credit, _, cerr := api.ClaimReward(acct, act.TaskCode); cerr == nil && credit > 0 {
					gained += credit
					line += fmt.Sprintf("（领奖 +%d）", credit)
				}
			}
			doneN++
			a.NotifyTaskEvent("task", uid, line)
			time.Sleep(reportGap)
		}
		if remain, rerr := rt.Upstream.UserResource(acct); rerr == nil {
			rt.Pool.SetCredits(uid, remain)
		}
		a.NotifyTaskEvent("task", uid, fmt.Sprintf(
			"一键完成结束：成功 %d · 跳过 %d · 失败 %d · 本轮领奖 +%d 积分", doneN, skipN, errN, gained))
	})
	return nil
}
