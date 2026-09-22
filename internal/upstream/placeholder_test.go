package upstream

import (
	"encoding/json"
	"testing"
)

// 回归（11148 第三形态）：空 assistant 占位插在 tool_calls 与结果中间——
// 配对完整、repack 旧逻辑 break、cleanup 不删，上游仍判断裂。
// 修复后 repairToolSequence 应删除占位并让结果紧跟批。
func TestRepairDropsEmptyPlaceholderBeforeResults(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"先看一下","tool_calls":[
			{"id":"c0","type":"function","function":{"name":"pwsh","arguments":"{}"}},
			{"id":"c1","type":"function","function":{"name":"pwsh","arguments":"{}"}}]},
		{"role":"assistant","content":""},
		{"role":"tool","tool_call_id":"c0","content":"out0"},
		{"role":"tool","tool_call_id":"c1","content":"out1"},
		{"role":"user","content":"next"}]}`)
	fixed, changed := repairToolSequence(body)
	if !changed {
		t.Fatal("placeholder must trigger repair")
	}
	var obj map[string]any
	if err := json.Unmarshal(fixed, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages after dropping placeholder, got %d", len(msgs))
	}
	// msgs[1] 是批头 assistant(tool_calls)，结果必须紧跟其后（msgs[2]/msgs[3]）
	m2 := msgs[2].(map[string]any)
	m3 := msgs[3].(map[string]any)
	if m2["role"] != "tool" || m2["tool_call_id"] != "c0" || m3["tool_call_id"] != "c1" {
		t.Fatalf("results must directly follow the calls batch: %v/%v %v/%v",
			m2["role"], m2["tool_call_id"], m3["role"], m3["tool_call_id"])
	}
}

// 占位带正文/推理时不能删（有信息量）——旧行为保留。
func TestRepairKeepsMeaningfulMessageBeforeResults(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[
			{"id":"c0","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"assistant","content":"有意义的内容"},
		{"role":"tool","tool_call_id":"c0","content":"out"}]}`)
	// 有意义消息挡道：repair 至少要重排（挪到结果后）而不是删除
	fixed, changed := repairToolSequence(body)
	if !changed {
		t.Fatal("meaningful interleave must still repack")
	}
	var obj map[string]any
	_ = json.Unmarshal(fixed, &obj)
	msgs := obj["messages"].([]any)
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			if c, _ := mm["content"].(string); c == "有意义的内容" {
				return // 存在即可
			}
		}
	}
	t.Fatal("meaningful message was dropped")
}

// isEmptyPlaceholder 单元：各形态判定。
func TestIsEmptyPlaceholder(t *testing.T) {
	cases := []struct {
		msg string
		want bool
	}{
		{`{"role":"assistant","content":""}`, true},
		{`{"role":"assistant"}`, true},
		{`{"role":"assistant","content":"  "}`, true},
		{`{"role":"assistant","reasoning_content":""}`, true},
		{`{"role":"assistant","content":"x"}`, false},
		{`{"role":"assistant","content":[{"type":"text","text":"hi"}]}`, false},
		{`{"role":"assistant","tool_calls":[{"id":"c"}]}`, false},
		{`{"role":"user","content":""}`, false},
		{`{"role":"tool","tool_call_id":"c","content":""}`, false},
	}
	for _, c := range cases {
		var m map[string]any
		if err := json.Unmarshal([]byte(c.msg), &m); err != nil {
			t.Fatal(err)
		}
		if got := isEmptyPlaceholder(m); got != c.want {
			t.Errorf("%s: got %v want %v", c.msg, got, c.want)
		}
	}
}
