// bancheck.go 主动探测账号是否被上游封禁/拉黑。
//
// 口径：打 UserResource（账单只读）。只有明确封禁语义才停用；
// 登录失效（12153）单独标出，不与封禁混为一谈，避免误杀。
package app

import (
	"fmt"
	"log"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

const banCheckGap = 400 * time.Millisecond

// BanVerdict 单账号探测结论。
type BanVerdict struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Group    string `json:"group,omitempty"`
	Status   string `json:"status"` // ok | banned | session_dead | error
	Detail   string `json:"detail,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

var banMarkers = []string{
	"封禁", "拉黑", "已封", "封号", "禁言", "账号异常",
	"banned", "blacklist", "blacklisted", "account banned",
	"user banned", "forbidden account", "account disabled",
	"account suspended", "account frozen", "permanently banned",
}

func classifyBan(err error) (status, detail string) {
	if err == nil {
		return "ok", ""
	}
	detail = shortErr(err)
	s := strings.ToLower(err.Error())
	var ue *provider.Error
	if asProviderError(err, &ue) {
		if ue.Kind == provider.ErrSessionDead {
			return "session_dead", detail
		}
		s = strings.ToLower(ue.Msg + " " + err.Error())
		if ue.Status == 403 && looksBanned(s) {
			return "banned", detail
		}
	}
	if looksBanned(s) {
		return "banned", detail
	}
	if strings.Contains(s, "12153") || strings.Contains(s, "offline user session") {
		return "session_dead", detail
	}
	return "error", detail
}

func asProviderError(err error, target **provider.Error) bool {
	if e, ok := err.(*provider.Error); ok {
		*target = e
		return true
	}
	return false
}

func looksBanned(s string) bool {
	for _, m := range banMarkers {
		if strings.Contains(s, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func (a *App) probeAccountBan(rt *Runtime, au *auth.Auth) BanVerdict {
	v := BanVerdict{UID: au.UID, Nickname: au.Nickname, Group: rt.Kind.String(), Status: "ok"}
	_, err := rt.Upstream.UserResource(au)
	status, detail := classifyBan(err)
	v.Status = status
	v.Detail = detail
	if status == "banned" {
		rt.Pool.Disable(au.UID, "上游封禁/拉黑")
		v.Disabled = true
		log.Printf("ban-check disable platform=%s uid=%s detail=%s", rt.Kind, au.UID, detail)
	}
	return v
}

// CheckAccountBan 探测单个账号。
func (a *App) CheckAccountBan(uid string) (BanVerdict, error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil || rt.Upstream == nil {
		return BanVerdict{}, fmt.Errorf("unknown account %s", uid)
	}
	return a.probeAccountBan(rt, au), nil
}

// CheckAccountBanAll 串行探测全部账号（防风控）。
func (a *App) CheckAccountBanAll() []BanVerdict {
	var out []BanVerdict
	first := true
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil {
			continue
		}
		for _, st := range rt.Pool.List() {
			au := rt.Pool.AuthByUID(st.UID)
			if au == nil || au.AccessToken == "" {
				out = append(out, BanVerdict{UID: st.UID, Nickname: st.Nickname, Group: rt.Kind.String(), Status: "error", Detail: "no token"})
				continue
			}
			if !first {
				time.Sleep(banCheckGap)
			}
			first = false
			out = append(out, a.probeAccountBan(rt, au))
		}
	}
	banned, dead, fail := 0, 0, 0
	for _, v := range out {
		switch v.Status {
		case "banned":
			banned++
		case "session_dead":
			dead++
		case "error":
			fail++
		}
	}
	log.Printf("ban-check all: total=%d banned=%d session_dead=%d error=%d", len(out), banned, dead, fail)
	return out
}
