package upstream

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// redMP4 1.5s 96x96 纯红测试视频（ffmpeg lavfi 生成，~2KB）。
const redMP4 = "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"

// 内嵌视频抽帧：video_url 段被替换为提示文本 + ≤10 帧 image_url。
func TestExpandVideoParts(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	body := `{"model":"kimi-k3.1-pro","stream":false,"messages":[{"role":"user","content":[
		{"type":"text","text":"what color"},
		{"type":"video_url","video_url":{"url":"data:video/mp4;base64,` + redMP4 + `"}}]}]}`
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	expandVideoParts(obj)
	msgs := obj["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
 imgs := 0
	var firstURL string
	for _, p := range parts {
		pm := p.(map[string]any)
		if pm["type"] == "image_url" {
			imgs++
			if firstURL == "" {
				firstURL, _ = pm["image_url"].(map[string]any)["url"].(string)
			}
		}
	}
	if imgs < 1 || imgs > videoMaxFrames {
		t.Fatalf("frames=%d want 1..%d", imgs, videoMaxFrames)
	}
	if !strings.HasPrefix(firstURL, "data:image/jpeg;base64,/9j/") {
		t.Fatalf("frame not jpeg data URL: %.60s", firstURL)
	}
	out, _ := json.Marshal(obj)
	if strings.Contains(string(out), "video_url") {
		t.Fatal("video_url part should be replaced")
	}
}

// URL 形态视频不处理（不下载），原段保留。
func TestExpandVideoPartsURLPassthrough(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[
		{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}}]}]}`
	var obj map[string]any
	json.Unmarshal([]byte(body), &obj)
	expandVideoParts(obj)
	out, _ := json.Marshal(obj)
	if !strings.Contains(string(out), "https://example.com/v.mp4") {
		t.Fatal("URL video must stay untouched")
	}
}
