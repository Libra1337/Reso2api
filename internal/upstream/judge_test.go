package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestJudgeEndpointStripsV1(t *testing.T) {
	c := JudgeConfig{BaseURL: "https://api.example.com/v1/"}
	if got := c.endpoint(); got != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("endpoint=%s", got)
	}
	c.BaseURL = "https://api.example.com"
	if got := c.endpoint(); got != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("endpoint=%s", got)
	}
}

func TestJudgeActive(t *testing.T) {
	c := JudgeConfig{Enabled: true, BaseURL: "https://x", APIKey: "k", Model: "m"}
	if !c.Active() {
		t.Fatal("should be active")
	}
	c.APIKey = ""
	if c.Active() {
		t.Fatal("missing key should be inactive")
	}
}

func TestParseJudgeVerdictTakesLast(t *testing.T) {
	content := `The text mentions {"category":"porn"} only as a quoted word.
{"category":"benign","reason":"moderation policy discussion"}`
	v, ok := parseJudgeVerdict(content)
	if !ok || v.Category != JudgeBenign || v.Reason != "moderation policy discussion" {
		t.Fatalf("%+v ok=%v", v, ok)
	}
	if _, ok := parseJudgeVerdict(`{"category":"weapon"}`); ok {
		t.Fatal("unknown category must not parse")
	}
	v, ok = parseJudgeVerdict("Hard to tell.\n{\"category\":\"uncertain\",\"reason\":\"ambiguous\"}")
	if !ok || v.Category != JudgeUncertain {
		t.Fatalf("%+v", v)
	}
}

func TestBuildJudgeUserMessageHintsFirst(t *testing.T) {
	msg := buildJudgeUserMessage([]string{"色情"}, []string{"hello"})
	if msg[:len("Keyword matcher flagged: 色情")] != "Keyword matcher flagged: 色情" {
		t.Fatalf("hint missing: %q", msg)
	}
}

func TestJudgeVerdictBlocks(t *testing.T) {
	if !(JudgeVerdict{Category: JudgePorn}.Blocks()) || !(JudgeVerdict{Category: JudgePolitical}.Blocks()) {
		t.Fatal("porn/political must block")
	}
	if (JudgeVerdict{Category: JudgeBenign}.Blocks()) || (JudgeVerdict{Category: JudgeUncertain}.Blocks()) {
		t.Fatal("benign/uncertain must not block")
	}
}

func TestJudgeRetryableSkipsTimeouts(t *testing.T) {
	timeout := fmt.Errorf("judge request failed: Post \"https://x/v1/chat/completions\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)")
	if judgeRetryable(timeout) {
		t.Fatal("timeout must not retry")
	}
	if !judgeRetryable(fmt.Errorf("judge HTTP 502")) {
		t.Fatal("502 must retry")
	}
	if judgeRetryable(fmt.Errorf("judge HTTP 429")) {
		t.Fatal("429 must not retry")
	}
	if judgeRetryable(fmt.Errorf("judge HTTP 400")) {
		t.Fatal("400 must not retry")
	}
}

func TestCallJudgeRetries502Once(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"category":"benign","reason":"ok"}`}},
			},
		})
	}))
	defer srv.Close()
	cfg := JudgeConfig{Enabled: true, BaseURL: srv.URL, APIKey: "k", Model: "m", TimeoutMS: 2000}
	v, err := callJudge(cfg, []string{"色情"}, []string{"hello"})
	if err != nil || v.Category != JudgeBenign {
		t.Fatalf("verdict=%+v err=%v calls=%d", v, err, n.Load())
	}
	if n.Load() != 2 {
		t.Fatalf("calls=%d want 2", n.Load())
	}
}

// 缓存：同文本第二次审查不再打网络；失败结论不缓存。
func TestCallJudgeCachesVerdict(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = n.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"category":"benign","reason":"same"}`}},
			},
		})
	}))
	defer srv.Close()
	cfg := JudgeConfig{Enabled: true, BaseURL: srv.URL, APIKey: "k", Model: "m", TimeoutMS: 2000}
	for i := 0; i < 3; i++ {
		v, err := callJudge(cfg, []string{"裸聊"}, []string{"same persona prompt"})
		if err != nil || v.Category != JudgeBenign {
			t.Fatalf("iter %d verdict=%+v err=%v", i, v, err)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("judge calls=%d want 1 (cached)", n.Load())
	}
}
