package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 回归：role=tool 的消息（工具结果）不参与脱敏变异——
// 上游审核不扫工具输出，变异会破坏文件内容（模型读后被写回脏数据）。
func TestSanitizeSkipsToolRole(t *testing.T) {
	const fileContent = "This function returns the configured value for the current user."
	var msgs []any
	if err := json.Unmarshal([]byte(`[
		{"role":"user","content":"This is a long enough prose sentence that will be mutated by the sanitizer pass."},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"/tmp/x\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"`+fileContent+`"}
	]`), &msgs); err != nil {
		t.Fatal(err)
	}
	sanitizeMessages(msgs)

	tool := msgs[2].(map[string]any)
	if tool["content"] != fileContent {
		t.Errorf("tool content mutated: %q", tool["content"])
	}
	// assistant 的 tool_calls.arguments 不受影响
	call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["arguments"] != `{"path":"/tmp/x"}` {
		t.Errorf("tool_call arguments mutated: %v", fn["arguments"])
	}
	// user 消息仍正常变异（反指纹功能不受影响）
	user := msgs[0].(map[string]any)["content"].(string)
	if user == "This is a long enough prose sentence that will be mutated by the sanitizer pass." {
		t.Errorf("user prose should be mutated")
	}
}

// 11128 渠道检测措辞改写（2026-11-03 实测：agent→assistant 一词即可绕过，
// 空白变形无效）。DeepSeek Harness 身份句与 coding agent 通用措辞都必须命中。
func TestSanitizeHarnessIdentityRewrite(t *testing.T) {
	src := "You are an AI agent powered by DeepSeek Harness.  You are a coding agent powered by the deepseek-v4.1-flash model."
	out := sanitizeText(src)
	if strings.Contains(out, "an AI agent powered by") || strings.Contains(out, "a coding agent") {
		t.Fatalf("harness 措辞未改写: %s", out[:80])
	}
	if !strings.Contains(out, "an AI assistant powered by DeepSeek Harness") {
		t.Fatalf("应保留 Harness 产品名只换 agent: %s", out[:80])
	}
	if !hasFingerprint(src) {
		t.Fatal("闸门应命中 AI agent powered by")
	}
	if hasFingerprint("You are a helpful assistant.") {
		t.Fatal("普通请求不应触发闸门")
	}
}
