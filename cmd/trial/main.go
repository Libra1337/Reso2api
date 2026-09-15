// trial 一次性批量领取 global trial 加油包：遍历 auths 下全部账号，
// 仅对 global 账号执行 /billing/ide/trial（CN 无此端点，提示不适用）。
// 移植自 workbuddy2api 1b04cd2，按 Reso 的 auth 目录约定适配。
//
// 用法：go run ./cmd/trial [auths_dir]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"wild-work/internal/auth"
	"wild-work/internal/upstream"
)

// trialStatus 单账号 trial 领取结果状态。
type trialStatus string

const (
	trialOK      trialStatus = "OK"
	trialAlready trialStatus = "ALREADY"
	trialNotApp  trialStatus = "N/A" // CN 账号不适用
	trialFailed  trialStatus = "FAIL"
)

type trialRow struct {
	uid    string
	nick   string
	status trialStatus
	detail string
}

// classifyTrial 归一化 ClaimTrial 结果：err → FAIL；claimed → OK；否则（幂等已领）→ ALREADY。
func classifyTrial(claimed bool, err error) (trialStatus, string) {
	switch {
	case err != nil:
		return trialFailed, err.Error()
	case claimed:
		return trialOK, "trial granted"
	default:
		return trialAlready, "already claimed (idempotent)"
	}
}

func main() {
	authDir := "auths"
	if len(os.Args) > 1 {
		authDir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", authDir)
		os.Exit(1)
	}
	sort.Strings(files)

	up := upstream.New()
	var rows []trialRow
	ok, already, na, fail := 0, 0, 0, 0
	for _, f := range files {
		a, err := auth.Parse(mustRead(f))
		if err != nil {
			rows = append(rows, trialRow{uid: filepath.Base(f), status: trialFailed, detail: "parse: " + err.Error()})
			fail++
			continue
		}
		a.FilePath = f
		if a.Region() != "global" {
			rows = append(rows, trialRow{uid: a.UID, nick: a.Nickname, status: trialNotApp, detail: "CN account: not applicable"})
			na++
			continue
		}
		claimed, err := up.ClaimTrial(a)
		st, detail := classifyTrial(claimed, err)
		switch st {
		case trialOK:
			ok++
		case trialAlready:
			already++
		default:
			fail++
		}
		rows = append(rows, trialRow{uid: a.UID, nick: a.Nickname, status: st, detail: detail})
	}
	fmt.Printf("%-10s %-12s %-8s %s\n", "uid", "nick", "status", "detail")
	for _, r := range rows {
		fmt.Printf("%-10s %-12s %-8s %s\n", uidShort(r.uid), r.nick, r.status, r.detail)
	}
	fmt.Printf("total=%d ok=%d already=%d na=%d fail=%d\n", len(rows), ok, already, na, fail)
}

func mustRead(fp string) []byte {
	raw, err := os.ReadFile(fp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", fp, err)
		os.Exit(1)
	}
	return raw
}

func uidShort(uid string) string {
	if len(uid) > 10 {
		return uid[:10]
	}
	return uid
}
