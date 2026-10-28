package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 动态工具提升：消息内 tools 合并到顶层，空 content 的 system 消息删除。
func TestAdaptHoistMessageTools(t *testing.T) {
	var obj map[string]any
	body := `{"model":"kimi-k3-1","messages":[
		{"role":"system","content":"","tools":[{"type":"function","function":{"name":"dyn_a"}}]},
		{"role":"user","content":"hi","tools":[{"type":"function","function":{"name":"dyn_b"}}]}],
		"tools":[{"type":"function","function":{"name":"top_c"}}]}`
	json.Unmarshal([]byte(body), &obj)
	hoistMessageTools(obj)
	out, _ := json.Marshal(obj)
	if !strings.Contains(string(out), `"dyn_a"`) || !strings.Contains(string(out), `"dyn_b"`) {
		t.Fatalf("消息内 tools 未提升: %s", out)
	}
	if strings.Count(string(out), `"role":"system"`) != 0 {
		t.Fatalf("提升后空 system 应删除: %s", out)
	}
	// 顶层合并含原有 top_c 与提升的 dyn_a/dyn_b
	var chk struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	json.Unmarshal(out, &chk)
	names := map[string]bool{}
	for _, tt := range chk.Tools {
		names[tt.Function.Name] = true
	}
	for _, want := range []string{"top_c", "dyn_a", "dyn_b"} {
		if !names[want] {
			t.Fatalf("顶层 tools 缺 %s: %v", want, names)
		}
	}
}

// effort 优先级：thinking.effort 覆盖 reasoning_effort。
func TestAdaptEffortPrecedence(t *testing.T) {
	var obj map[string]any
	json.Unmarshal([]byte(`{"thinking":{"type":"enabled","effort":"high"},"reasoning_effort":"low"}`), &obj)
	normalizeEffortPrecedence(obj)
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort 应被 thinking.effort 覆盖: %v", obj["reasoning_effort"])
	}
}

// assistant 开头 → 预置 user 占位。
func TestAdaptAssistantHeadStub(t *testing.T) {
	var obj map[string]any
	json.Unmarshal([]byte(`{"messages":[{"role":"assistant","content":"prev"},{"role":"user","content":"hi"}]}`), &obj)
	assistantHeadStub(obj)
	msgs := obj["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("头部应为 user 占位: %v", msgs[0])
	}
}

// json_schema → json_object + Schema 注入。
func TestAdaptEmulateJSONSchema(t *testing.T) {
	var obj map[string]any
	json.Unmarshal([]byte(`{"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object","required":["city"]}}},"messages":[{"role":"user","content":"北京天气"}]}`), &obj)
	emulateJSONSchema(obj)
	rf := obj["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Fatalf("应转为 json_object: %v", rf)
	}
	msgs := obj["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "system" || !strings.Contains(last["content"].(string), `"city"`) {
		t.Fatalf("Schema 指令未注入尾部: %v", last)
	}
}

// 裸 base64 视频魔数识别。
func TestVideoIsVideoBytes(t *testing.T) {
	if !isVideoBytes([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}) {
		t.Fatal("mp4 魔数应识别")
	}
	if !isVideoBytes([]byte{0x1A, 0x45, 0xDF, 0xA3, 1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatal("EBML(webm/mkv) 魔数应识别")
	}
	if isVideoBytes([]byte("hello world!!")) {
		t.Fatal("非视频不应识别")
	}
}
