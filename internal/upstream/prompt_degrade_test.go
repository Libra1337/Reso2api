package upstream

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// 11128 降级窗口只应改写「带指纹句」的请求；无指纹请求的 system 不陪葬
// （2026-10-27 事故：窗口期内全站 system 被换成中性提示词，模型自述无上下文）。
func TestApplyPromptDegradeWindowOnlyFingerprinted(t *testing.T) {
	c := &Client{PromptMode: "passthrough", degradeUntil: time.Now().Add(time.Hour)}

	plain := []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"SECRETWORD-PINEAPPLE-7749. Reply with exactly the secret word."},{"role":"user","content":"what is the secret word?"}]}`)
	out, degraded := c.applyPrompt(plain)
	if degraded {
		t.Fatal("fingerprint-free request must not be degraded inside window")
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("fingerprint-free request body must pass through unchanged")
	}

	fp := []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}]}`)
	out, degraded = c.applyPrompt(fp)
	if !degraded {
		t.Fatal("fingerprinted request should be degraded inside window")
	}
	if strings.Contains(string(out), "Anthropic's official CLI for Claude") {
		t.Fatal("fingerprint sentence should be replaced by degraded prompt")
	}
	if !strings.Contains(string(out), DegradedTestNeedle) {
		t.Fatalf("degraded prompt missing:\n%s", out)
	}
}

// 窗口未开启时指纹请求也原样透传（零开销快速路径）。
func TestApplyPromptNoWindowPassthrough(t *testing.T) {
	c := &Client{PromptMode: "passthrough"}
	fp := []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}]}`)
	out, degraded := c.applyPrompt(fp)
	if degraded || !bytes.Equal(out, fp) {
		t.Fatalf("outside window must pass through unchanged, degraded=%v", degraded)
	}
}

// DegradedTestNeedle 降级提示词的可识别片段（与 prompt.Degraded 首句同步）。
const DegradedTestNeedle = "helpful assistant"
