package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// KVV 报告回归（2026-10-28）：content 为空但携带其他字段（消息内 tools）的
// 消息不得被按"空占位/剥空"删除——动态工具请求曾被静默改成普通对话。
func TestRepairKeepsPayloadBearingEmptyContent(t *testing.T) {
	body := []byte(`{"model":"kimi-k3-1","messages":[
		{"role":"system","content":"","tools":[{"type":"function","function":{"name":"get_weather"}}]},
		{"role":"user","content":"北京天气"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"晴 26 度"},
		{"role":"user","content":"谢谢"}]}`)
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	// 直接打两个删除判定
	sysMsg := msgs[0].(map[string]any)
	if isEmptyPlaceholder(sysMsg) {
		t.Fatal("system 消息携带 tools 字段,不得判为空占位")
	}
	if msgHasPayloadFields(map[string]any{"role": "assistant", "content": ""}) {
		t.Fatal("裸空 assistant 应无载荷字段")
	}
	// 整链路:repairToolSequence 不丢带 tools 的空 content 消息
	fixed, _ := repairToolSequence(body)
	if !strings.Contains(string(fixed), `"get_weather"`) || !strings.Contains(string(fixed), `"tools"`) {
		t.Fatalf("消息内 tools 被删除:\n%.300s", fixed)
	}
	// 原有行为不回退:真·空占位 assistant(仅 role+空 content)仍会被清
	bare := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"assistant","content":""},
		{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	fixed2, _ := repairToolSequence(bare)
	if strings.Count(string(fixed2), `"role":"assistant"`) != 1 {
		t.Fatalf("裸空 assistant 应仍被清除:\n%.300s", fixed2)
	}
}
