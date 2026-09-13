package app

import (
	"fmt"
	"testing"

	"wild-work/internal/provider"
)

func TestClassifyBan(t *testing.T) {
	cases := []struct {
		err    error
		status string
	}{
		{nil, "ok"},
		{&provider.Error{Kind: provider.ErrSessionDead, Status: 401, Msg: "Offline user session not found"}, "session_dead"},
		{&provider.Error{Kind: provider.ErrClient, Status: 403, Msg: "account banned by security"}, "banned"},
		{fmt.Errorf("上游 403: 账号已被拉黑"), "banned"},
		{fmt.Errorf("network timeout"), "error"},
		{&provider.Error{Kind: provider.ErrHardCredit, Status: 402, Msg: "余额不足"}, "error"},
	}
	for _, c := range cases {
		got, _ := classifyBan(c.err)
		if got != c.status {
			t.Errorf("classifyBan(%v)=%q want %q", c.err, got, c.status)
		}
	}
}
