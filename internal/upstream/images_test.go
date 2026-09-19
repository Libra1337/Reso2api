package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeImageURLs(t *testing.T) {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	jpg := "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////2wBDAf//////////////////////////////////////////2wBDAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgID/wAARCAABAAEDASIA"[:100]
	cases := []struct {
		name string
		url  string
		want string // 期望补全后的前缀；"" 表示应保持原值不动
	}{
		{"裸PNG补前缀", png, "data:image/png;base64,"},
		{"裸JPEG补前缀", jpg, "data:image/jpeg;base64,"},
		{"已有dataURL不动", "data:image/png;base64," + png, ""},
		{"http引用不动", "https://example.com/a.png", ""},
		{"短值不动", "iVBORw0KGgo", ""},
		{"非魔数不动", strings.Repeat("Q", 128), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":[` +
				`{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":` +
				jsonString(c.url) + `}}]}]}`
			var obj map[string]any
			if err := json.Unmarshal([]byte(body), &obj); err != nil {
				t.Fatal(err)
			}
			normalizeImageURLs(obj)
			got := extractFirstImageURL(t, obj)
			if c.want == "" {
				if got != c.url {
					t.Fatalf("应保持原值，got %.60s", got)
				}
				return
			}
			if !strings.HasPrefix(got, c.want) || !strings.HasSuffix(got, c.url) {
				t.Fatalf("前缀补全错误，want prefix %q, got %.60s", c.want, got)
			}
		})
	}
}

// jsonString 编码 JSON 字符串字面量。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// extractFirstImageURL 取第一条消息第一个 image_url part 的 url。
func extractFirstImageURL(t *testing.T, obj map[string]any) string {
	t.Helper()
	msgs := obj["messages"].([]any)
	msg := msgs[0].(map[string]any)
	for _, pi := range msg["content"].([]any) {
		part, ok := pi.(map[string]any)
		if !ok || part["type"] != "image_url" {
			continue
		}
		iu, ok := part["image_url"].(map[string]any)
		if !ok {
			t.Fatal("image_url 不是对象")
		}
		u, _ := iu["url"].(string)
		return u
	}
	t.Fatal("无 image_url part")
	return ""
}

// TestPrepareBodyNormalizesImages 端到端：PrepareBody 管线包含裸 base64 归一化。
func TestPrepareBodyNormalizesImages(t *testing.T) {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	body := `{"model":"glm-5v-turbo","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"` + png + `"}}]}],"stream":false}`
	out := PrepareBody([]byte(body))
	if !strings.Contains(string(out), `"data:image/png;base64,`) {
		t.Fatalf("出站体未补全 data URL 前缀: %.200s", out)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatal("stream 强制改写丢失")
	}
}

// TestRelocateAssistantImages assistant 轮图片挪到合成 user 消息；
// user/system 轮不动；无 assistant 图时消息数组原样。
func TestRelocateAssistantImages(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"text","text":"shot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
		{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,REVG"}}]},
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,R0hJ"}},
			{"type":"text","text":"see?"}]}
	]}`
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	relocateAssistantImages(obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 7 {
		t.Fatalf("消息数应为 7（5 原始 + 2 合成），got %d", len(msgs))
	}
	// msg2: assistant 只剩 text part
	m2 := msgs[2].(map[string]any)
	p2 := m2["content"].([]any)
	if len(p2) != 1 || p2[0].(map[string]any)["type"] != "text" {
		t.Fatalf("assistant[2] 应只剩 text part: %v", m2["content"])
	}
	// msg3: 合成 user 只含第一张图
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "user" {
		t.Fatalf("msg3 应为合成 user，got %v", m3["role"])
	}
	p3 := m3["content"].([]any)
	if len(p3) != 1 || p3[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("合成 user 应只含图: %v", m3["content"])
	}
	// msg4: 纯图 assistant → content 置空串
	m4 := msgs[4].(map[string]any)
	if m4["role"] != "assistant" || m4["content"] != "" {
		t.Fatalf("纯图 assistant 应置空 content: %v", m4)
	}
	// msg5: 第二张图的合成 user
	m5 := msgs[5].(map[string]any)
	if m5["role"] != "user" {
		t.Fatalf("msg5 应为合成 user，got %v", m5["role"])
	}
	// msg6: 原最后一条 user 轮的图不动
	m6 := msgs[6].(map[string]any)
	if m6["role"] != "user" {
		t.Fatalf("msg6 应为原 user，got %v", m6["role"])
	}
	p6 := m6["content"].([]any)
	if len(p6) != 2 || p6[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("user 轮图片应原位保留: %v", m6["content"])
	}
}

func findUserWithImage(msgs []any, marker string) map[string]any {
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, pi := range parts {
			p, ok := pi.(map[string]any)
			if !ok || p["type"] != "image_url" {
				continue
			}
			iu, _ := p["image_url"].(map[string]any)
			if u, _ := iu["url"].(string); strings.Contains(u, marker) {
				return m
			}
		}
	}
	return nil
}

// TestRelocateAssistantImagesNoop 无 assistant 图时不改消息数组。
func TestRelocateAssistantImagesNoop(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
		{"role":"assistant","content":"plain string"}
	]}`
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	before := len(obj["messages"].([]any))
	relocateAssistantImages(obj)
	after := obj["messages"].([]any)
	if len(after) != before {
		t.Fatalf("无 assistant 图不应增删消息: %d -> %d", before, len(after))
	}
}
