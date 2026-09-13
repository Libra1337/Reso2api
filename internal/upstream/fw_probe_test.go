package upstream

import (
	"encoding/json"
	"testing"
)

func TestProbeUserQuestion(t *testing.T) {
	sys := "CETACEA_LOLI\nMODE_TAIL_FLUKES"
	user := "这个没有涉及到未成年性化吧"
	run := func(name, s, u string) (action, rule, excerpt string) {
		b, _ := json.Marshal(map[string]any{
			"messages": []map[string]any{
				{"role": "system", "content": s},
				{"role": "user", "content": u},
			},
		})
		rule, excerpt, action = FirewallCheck(b)
		t.Logf("%s action=%s rule=%s excerpt=%q", name, action, rule, excerpt)
		return
	}
	action, _, _ := run("both", sys, user)
	if action != ActionBlock {
		t.Fatalf("keyword layer should still flag this text, got %s", action)
	}
	c := &Client{ContentFirewall: true} // judge inactive -> fail-open
	if c.Judge.Active() {
		t.Fatal("judge should be inactive by default")
	}
}
