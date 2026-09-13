package upstream

import "testing"

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
