package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

func TestSchoolTasksParseAndDecorate(t *testing.T) {
	body := `{"code":0,"msg":"ok","data":{"in_period":true,"tasks":[
		{"task_code":"share_invite","title":"分享","status":"pending","progress":0,"target_count":1,"reward_credit":10,"task_type":"single"},
		{"task_code":"chat_3_times","title":"对话","status":"in_progress","progress":1,"target_count":3,"reward_credit":6,"task_type":"recurring"},
		{"task_code":"task_student_verify","title":"学生认证","status":"pending","progress":0,"target_count":1},
		{"task_code":"brand_new","title":"新任务","status":"pending","progress":0,"target_count":1}
	]}}`
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.Contains(r.URL.Path, "/portal/activity/school/tasks") {
				t.Errorf("path=%s", r.URL.Path)
			}
			if r.Header.Get("User-Agent") != schoolMPUA {
				t.Errorf("ua=%q", r.Header.Get("User-Agent"))
			}
			return jsonResp(200, body), nil
		})},
		BillingBaseCN: "https://www.codebuddy.cn",
	}
	res, err := c.SchoolTasks(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.InPeriod || len(res.Tasks) != 4 {
		t.Fatalf("in_period=%v n=%d", res.InPeriod, len(res.Tasks))
	}
	want := map[string]string{
		"share_invite":        SchoolModeShare,
		"chat_3_times":        SchoolModeReport,
		"task_student_verify": SchoolModeManual,
		"brand_new":           SchoolModeUnknown,
	}
	for _, task := range res.Tasks {
		if task.Mode != want[task.TaskCode] {
			t.Errorf("%s mode=%s want %s", task.TaskCode, task.Mode, want[task.TaskCode])
		}
	}
	if res.Tasks[1].ReportKind != "mini_chat" {
		t.Errorf("chat_3_times report_kind=%s", res.Tasks[1].ReportKind)
	}
}

func TestSchoolPrizeLabel(t *testing.T) {
	if got := schoolPrizeLabel("school_credit_6", 6); !strings.Contains(got, "6积分") {
		t.Errorf("got %s", got)
	}
	if got := schoolPrizeLabel("school_voucher_luckin", 0); got != "瑞幸咖啡15元券" {
		t.Errorf("got %s", got)
	}
	if got := schoolPrizeLabel("unknown_x", 0); got != "unknown_x" {
		t.Errorf("unknown should pass through, got %s", got)
	}
}

func TestIsSchoolNoChance(t *testing.T) {
	err := &Error{Kind: ErrClient, Status: 409, Msg: `code=40900 msg=no chance`}
	if !IsSchoolNoChance(err) {
		t.Fatal("40900 should match")
	}
	if IsSchoolNoChance(fmt.Errorf("network down")) {
		t.Fatal("plain error must not match")
	}
}

func TestSchoolConfigParse(t *testing.T) {
	data := json.RawMessage(`{"in_period":true,"chance":{"balance":3,"total_earned":5,"voucher_won":false,"lottery_limit":10},"prizes":[{"prize_code":"school_credit_6"}]}`)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":0,"data":`+string(data)+`}`), nil
		})},
		BillingBaseCN: "https://www.codebuddy.cn",
	}
	got, err := c.SchoolConfigFetch(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.InPeriod || got.Chance.Balance != 3 || len(got.Prizes) != 1 {
		t.Fatalf("%+v", got)
	}
	if got.Prizes[0].Label == "" {
		t.Error("prize label should be filled")
	}
}
