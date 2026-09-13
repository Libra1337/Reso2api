// school.go 小程序「开学季」活动（school_open_day_2026）。
//
// 来源：上游 workbuddy2api scripts/school_open_day_2026.py（2026-09-14 实测口径）。
// 与成长中心任务（/activity/growth/tasks）不是同一套：活动在
// www.codebuddy.cn/portal/activity/school，对话上报必须带
// activityId=school_open_day_2026，抽奖是活动转盘 /wheel/draw。
//
// 学生认证（task_student_verify）为人工环节，本模块不碰。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
)

const (
	SchoolActivityID = "school_open_day_2026"

	schoolTasksPath         = "/portal/activity/school/tasks"
	schoolConfigPath        = "/portal/activity/school/config"
	schoolShareCompletePath = "/portal/activity/school/tasks/share-complete"
	schoolWheelDrawPath     = "/portal/activity/school/wheel/draw"
	schoolExpertListPath    = "/v2/operation-platform/market/expert/list"
	schoolExpertCategory    = "16-BackToSchool"

	schoolMPUA = "Mozilla/5.0 (Linux; Android 14; MicroMessenger/8.0.49 WeChat/0.8.0 MiniProgramEnv/android; wkbrowser xweb)"
)

// SchoolTaskMode 开学季任务处理方式。
const (
	SchoolModeManual  = "manual"
	SchoolModeShare   = "share"
	SchoolModeReport  = "report"
	SchoolModeUnknown = "unknown"
)

// schoolKnownTasks 已打通的 task_code。未在表内的服务端新任务保守跳过。
var schoolKnownTasks = map[string]struct {
	Mode       string
	Note       string
	ReportKind string
}{
	"task_student_verify": {Mode: SchoolModeManual, Note: "微信学生认证（人工，不碰）"},
	"share_invite":        {Mode: SchoolModeShare, Note: "分享活动给好友；share-complete 点亮"},
	"chat_3_times":        {Mode: SchoolModeReport, Note: "与 AI 对话 3 次；小程序 chat_request_send + activityId", ReportKind: "mini_chat"},
	"desktop_chat_1_time": {Mode: SchoolModeReport, Note: "桌面端对话 1 次；桌面指纹 6 连 + activityId", ReportKind: "desktop_seq"},
	"expert_use":          {Mode: SchoolModeReport, Note: "召唤开学季专家并对话；BackToSchool 专家 + expert_actual_use", ReportKind: "expert"},
}

var schoolExpertFallback = []struct {
	ID, Name, Title string
}{
	{"ex_jB0dyFIQJEWa", "论小舟", "论文写作导师"},
	{"ex_lQjkerakvIex", "英语学习教练", "大学英语学习教练"},
}

// SchoolLotteryPrizeLabels 转盘 prize_code → 展示名（未知码原样返回）。
var SchoolLotteryPrizeLabels = map[string]string{
	"school_credit_6":        "6积分",
	"school_credit_66":       "66积分",
	"school_voucher_luckin":  "瑞幸咖啡15元券",
	"school_voucher_kfc_ok":  "肯德基OK餐券",
	"school_voucher_kfc_ice": "肯德基冰淇淋券",
	"school_voucher_kugou":   "酷狗会员月卡券",
}

// SchoolTask 开学季单个任务。
type SchoolTask struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	Status       string `json:"status,omitempty"`
	Progress     int64  `json:"progress"`
	TargetCount  int64  `json:"target_count"`
	TaskType     string `json:"task_type,omitempty"`
	RewardCredit int64  `json:"reward_credit,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Note         string `json:"note,omitempty"`
	ReportKind   string `json:"report_kind,omitempty"`
}

// SchoolTasksResult GET /portal/activity/school/tasks。
type SchoolTasksResult struct {
	InPeriod bool         `json:"in_period"`
	Tasks    []SchoolTask `json:"tasks"`
}

// SchoolChance 转盘次数。
type SchoolChance struct {
	Balance      int  `json:"balance"`
	TotalEarned  int  `json:"total_earned"`
	VoucherWon   bool `json:"voucher_won"`
	LotteryLimit int  `json:"lottery_limit"`
}

// SchoolPrize 转盘奖品格。
type SchoolPrize struct {
	PrizeCode string `json:"prize_code"`
	Label     string `json:"label,omitempty"`
}

// SchoolConfig GET /portal/activity/school/config。
type SchoolConfig struct {
	InPeriod bool          `json:"in_period"`
	StartAt  any           `json:"start_at,omitempty"`
	EndAt    any           `json:"end_at,omitempty"`
	Chance   SchoolChance  `json:"chance"`
	Prizes   []SchoolPrize `json:"prizes,omitempty"`
}

// SchoolDrawResult POST /wheel/draw 成功结果。
type SchoolDrawResult struct {
	PrizeCode     string `json:"prize_code"`
	Label         string `json:"label"`
	CreditAmount  int    `json:"credit_amount"`
	ChanceBalance int    `json:"chance_balance"`
}

func schoolSpec(code string) (mode, note, reportKind string) {
	if s, ok := schoolKnownTasks[code]; ok {
		return s.Mode, s.Note, s.ReportKind
	}
	return SchoolModeUnknown, "未知任务，保守跳过", ""
}

func decorateSchoolTask(t *SchoolTask) {
	t.Mode, t.Note, t.ReportKind = schoolSpec(t.TaskCode)
}

func schoolPrizeLabel(code string, credit int) string {
	if s, ok := SchoolLotteryPrizeLabels[code]; ok {
		if credit > 0 && strings.Contains(s, "积分") {
			return fmt.Sprintf("%s（+%d Credit）", s, credit)
		}
		return s
	}
	if code == "" {
		return "?"
	}
	return code
}

// schoolJSON 开学季 portal 请求：billing 域 + 小程序 UA。
func (c *Client) schoolJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.billingBase(a.Region())+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("User-Agent", schoolMPUA)
	return c.doJSONBilling(req)
}

// SchoolTasks 拉取开学季任务列表。
func (c *Client) SchoolTasks(a *auth.Auth) (*SchoolTasksResult, error) {
	data, err := c.schoolJSON(a, http.MethodGet, schoolTasksPath, nil)
	if err != nil {
		return nil, err
	}
	var out SchoolTasksResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("school/tasks parse: %w", err)
	}
	for i := range out.Tasks {
		decorateSchoolTask(&out.Tasks[i])
	}
	return &out, nil
}

// SchoolViewed 接任务：pending → in_progress。
func (c *Client) SchoolViewed(a *auth.Auth, code string) error {
	_, err := c.schoolJSON(a, http.MethodPost, schoolTasksPath+"/"+code+"/viewed", nil)
	return err
}

// SchoolShareComplete 点亮 share_invite。
func (c *Client) SchoolShareComplete(a *auth.Auth) error {
	_, err := c.schoolJSON(a, http.MethodPost, schoolShareCompletePath, map[string]any{"channel": "wechat"})
	return err
}

// SchoolClaim 领奖：completed → claimed（发转盘次数）。
func (c *Client) SchoolClaim(a *auth.Auth, code string) error {
	_, err := c.schoolJSON(a, http.MethodPost, schoolTasksPath+"/"+code+"/claim", nil)
	return err
}

// SchoolConfigFetch 转盘配置 + 余额。
func (c *Client) SchoolConfigFetch(a *auth.Auth) (*SchoolConfig, error) {
	data, err := c.schoolJSON(a, http.MethodGet, schoolConfigPath, nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		InPeriod bool `json:"in_period"`
		StartAt  any  `json:"start_at"`
		EndAt    any  `json:"end_at"`
		Chance   SchoolChance
		Prizes   []struct {
			PrizeCode string `json:"prize_code"`
		} `json:"prizes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("school/config parse: %w", err)
	}
	out := &SchoolConfig{InPeriod: raw.InPeriod, StartAt: raw.StartAt, EndAt: raw.EndAt, Chance: raw.Chance}
	for _, p := range raw.Prizes {
		out.Prizes = append(out.Prizes, SchoolPrize{PrizeCode: p.PrizeCode, Label: schoolPrizeLabel(p.PrizeCode, 0)})
	}
	return out, nil
}

// SchoolWheelDraw 抽转盘一次。余额 0 时上游 HTTP 409 code=40900。
func (c *Client) SchoolWheelDraw(a *auth.Auth, drawUUID string) (*SchoolDrawResult, error) {
	data, err := c.schoolJSON(a, http.MethodPost, schoolWheelDrawPath, map[string]any{"draw_uuid": drawUUID})
	if err != nil {
		return nil, err
	}
	var d struct {
		PrizeCode     string `json:"prize_code"`
		CreditAmount  int    `json:"credit_amount"`
		ChanceBalance int    `json:"chance_balance"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("wheel/draw parse: %w", err)
	}
	return &SchoolDrawResult{
		PrizeCode:     d.PrizeCode,
		Label:         schoolPrizeLabel(d.PrizeCode, d.CreditAmount),
		CreditAmount:  d.CreditAmount,
		ChanceBalance: d.ChanceBalance,
	}, nil
}

// ReportSchoolMiniChat 小程序域 chat_request_send（点亮 chat_3_times）。
func (c *Client) ReportSchoolMiniChat(a *auth.Auth) error {
	now := time.Now().UnixMilli()
	conv := fmt.Sprintf("wbmp-%d", now)
	ev := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
		"extName": "workbuddy-mp", "extVersion": "SaaS", "mode": "chat",
		"conversationId": conv, "requestId": conv, "inputLength": 12,
		"activityId": SchoolActivityID, "mentionContexts": []any{}, "mentionContextCount": 0,
		"userId": a.UID,
	}
	_, err := c.schoolJSON(a, http.MethodPost, reportPath, []any{ev})
	return err
}

// ReportSchoolDesktopChat 桌面指纹 6 连 + activityId（点亮 desktop_chat_1_time）。
func (c *Client) ReportSchoolDesktopChat(a *auth.Auth) error {
	now := time.Now().UnixMilli()
	conv := fmt.Sprintf("wbdesk-%d", now)
	seq := DesktopChatSequence(conv, conv, conv, "fast-model", "fast-model")
	for i := range seq {
		seq[i]["activityId"] = SchoolActivityID
	}
	return c.ReportDesktopEvent(a, seq...)
}

// ReportSchoolExpertUse BackToSchool 专家 + expert_actual_use（点亮 expert_use）。
func (c *Client) ReportSchoolExpertUse(a *auth.Auth) error {
	id, name, title := c.pickSchoolExpert(a)
	now := time.Now().UnixMilli()
	conv := fmt.Sprintf("wbexp-%d", now)
	ev := map[string]any{
		"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
		"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
		"extName": "workbuddy-mp", "extVersion": "SaaS",
		"machineId": deriveID(a, "machine"), "os": "android", "osVersion": "14",
		"arch": "arm64", "timezone": "Asia/Shanghai",
		"userId": a.UID, "userNickname": a.Nickname,
		"id": id, "name": id, "expertTitle": firstNonEmpty(title, name),
		"type": "send_message", "characterCount": 12, "expertType": "agent",
		"conversationId": conv, "activityId": SchoolActivityID,
	}
	_, err := c.schoolJSON(a, http.MethodPost, reportPath, []any{ev})
	return err
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func (c *Client) pickSchoolExpert(a *auth.Auth) (id, name, title string) {
	body := map[string]any{
		"edition_mode": "all,domestic", "page": 1, "page_size": 20,
		"sort_by": "use_count", "sort_order": "desc",
		"categories": []string{schoolExpertCategory}, "expert_type": "agent",
	}
	data, err := c.schoolJSON(a, http.MethodPost, schoolExpertListPath, body)
	if err == nil {
		var out struct {
			Experts []struct {
				ExpertID      string          `json:"expert_id"`
				DisplayNameZH json.RawMessage `json:"display_name_zh"`
				ProfessionZH  json.RawMessage `json:"profession_zh"`
			} `json:"experts"`
		}
		if json.Unmarshal(data, &out) == nil {
			for _, e := range out.Experts {
				if e.ExpertID == "" {
					continue
				}
				return e.ExpertID, zhText(e.DisplayNameZH), zhText(e.ProfessionZH)
			}
		}
	}
	fb := schoolExpertFallback[0]
	return fb.ID, fb.Name, fb.Title
}

func zhText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		ZH string `json:"zh"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.ZH
	}
	return ""
}

// IsSchoolNoChance 转盘余额耗尽（HTTP 409 code=40900）。
func IsSchoolNoChance(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if errors.As(err, &ue) && (ue.Status == http.StatusConflict || strings.Contains(ue.Msg, "40900") || strings.Contains(strings.ToLower(ue.Msg), "no chance")) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "40900") || strings.Contains(strings.ToLower(s), "no chance")
}
