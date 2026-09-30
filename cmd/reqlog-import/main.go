// reqlog-import — 把既有 jsonl 历史一次性导入 PostgreSQL（切换存储模式前跑一次）。
//
// 用法（在网关数据目录所在机器）：
//
//	reqlog-import -dsn "postgres://wildwork:pass@127.0.0.1:5432/wildwork?sslmode=disable" \
//	              -reqlog /opt/wild-work/data/request_logs.jsonl \
//	              -firewall /opt/wild-work/data/firewall_events.jsonl
//
// 行为：
//   - 每个文件用 meta 表（key=imported:<file>）记录导入行数，重跑跳过已导过的文件；
//   - request_logs.jsonl 的 time 无年份（"01-02 15:04:05"），按 -tz（默认
//     Asia/Shanghai，即网关写入时的容器时区）解析后补当前年——导入容器自身
//     时区无关紧要，勿用 time.Local（踩过坑：UTC 容器把历史平移了 +8h）；
//   - 批量 INSERT（每批 1000 行），26 万行实测秒级完成；
//   - 导入不影响 jsonl 原文件（保留为冷备；切换后网关不再追加）。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"wild-work/internal/pgstore"
)

type legacyReqLog struct {
	Time         string  `json:"time"`
	Model        string  `json:"model"`
	Channel      string  `json:"channel"`
	UID          string  `json:"uid"`
	Status       int     `json:"status"`
	Stream       bool    `json:"stream"`
	TTFBMS       int64   `json:"ttfb_ms"`
	TotalMS      int64   `json:"total_ms"`
	InTokens     int64   `json:"in_tokens"`
	OutTokens    int64   `json:"out_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	Credit       float64 `json:"credit"`
	BodyFile     string  `json:"body_file"`
}

type legacyFirewallEvent struct {
	At      int64  `json:"at"`
	Rule    string `json:"rule"`
	UID     string `json:"uid,omitempty"`
	Nick    string `json:"nick,omitempty"`
	Model   string `json:"model,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Content string `json:"content,omitempty"`
	Match   string `json:"match,omitempty"`
	Observe bool   `json:"observe,omitempty"`
	Keyword string `json:"keyword,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Entry   string `json:"entry,omitempty"`
	Judge   string `json:"judge,omitempty"`
}

func main() {
	dsn := flag.String("dsn", "", "PostgreSQL DSN（必填）")
	reqlogPath := flag.String("reqlog", "", "request_logs.jsonl 路径（空=跳过）")
	firewallPath := flag.String("firewall", "", "firewall_events.jsonl 路径（空=跳过）")
	tzName := flag.String("tz", "Asia/Shanghai", "jsonl wall-clock 时间所在时区")
	flag.Parse()
	loc, err := time.LoadLocation(*tzName)
	if err != nil {
		// 无 zonedata 环境（alpine 裸容器）兜底：Asia/Shanghai 恒 +08 无夏令时
		if *tzName == "Asia/Shanghai" {
			loc = time.FixedZone("CST", 8*3600)
		} else {
			log.Fatalf("加载时区 %s 失败: %v", *tzName, err)
		}
	}
	if *dsn == "" {
		log.Fatal("必须提供 -dsn")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, err := pgstore.Open(ctx, *dsn)
	if err != nil {
		log.Fatalf("连接失败: %v", err)
	}
	defer store.Close()

	if *reqlogPath != "" {
		importReqLog(ctx, store, *reqlogPath, loc)
	}
	if *firewallPath != "" {
		importFirewall(ctx, store, *firewallPath)
	}
}

func alreadyImported(ctx context.Context, store *pgstore.Store, path string) (string, bool) {
	v, ok, err := store.GetMeta(ctx, "imported:"+path)
	if err != nil {
		log.Fatalf("meta 读取失败: %v", err)
	}
	return v, ok
}

func importReqLog(ctx context.Context, store *pgstore.Store, path string, loc *time.Location) {
	if v, ok := alreadyImported(ctx, store, path); ok {
		log.Printf("已导入过 %s（%s 行），跳过；强制重导请先清 meta 表该键", path, v)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer f.Close()

	year := time.Now().In(loc).Year()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var batch []pgstore.ReqLogRow
	total, bad := 0, 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := store.InsertReqLogs(ctx, batch); err != nil {
			log.Fatalf("批量插入失败（第 %d 行附近）: %v", total, err)
		}
		batch = batch[:0]
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l legacyReqLog
		if err := json.Unmarshal(line, &l); err != nil || l.Time == "" {
			bad++
			continue
		}
		t, err := time.ParseInLocation("01-02 15:04:05", l.Time, loc)
		if err != nil {
			bad++
			continue
		}
		t = t.AddDate(year, 0, 0)
		batch = append(batch, pgstore.ReqLogRow{
			T: t, Model: l.Model, Channel: l.Channel, UID: l.UID, Status: l.Status,
			Stream: l.Stream, TTFBMS: l.TTFBMS, TotalMS: l.TotalMS, InTokens: l.InTokens,
			OutTokens: l.OutTokens, CachedTokens: l.CachedTokens, Credit: l.Credit, BodyFile: l.BodyFile,
		})
		total++
		if len(batch) >= 1000 {
			flush()
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		log.Fatalf("读取 %s 失败: %v", path, err)
	}
	_ = store.SetMeta(ctx, "imported:"+path, strconv.Itoa(total))
	fmt.Printf("request_logs: 导入 %d 行，跳过非法 %d 行\n", total, bad)
}

func importFirewall(ctx context.Context, store *pgstore.Store, path string) {
	if v, ok := alreadyImported(ctx, store, path); ok {
		log.Printf("已导入过 %s（%s 行），跳过", path, v)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("%s 不存在，跳过", path)
			return
		}
		log.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var batch []pgstore.FirewallEventRow
	total, bad := 0, 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := store.InsertFirewallEvents(ctx, batch); err != nil {
			log.Fatalf("批量插入失败（第 %d 行附近）: %v", total, err)
		}
		batch = batch[:0]
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e legacyFirewallEvent
		if err := json.Unmarshal(line, &e); err != nil || e.At == 0 {
			bad++
			continue
		}
		batch = append(batch, pgstore.FirewallEventRow{
			At: e.At, Rule: e.Rule, UID: e.UID, Nick: e.Nick, Model: e.Model,
			Snippet: e.Snippet, Content: e.Content, Match: e.Match, Observe: e.Observe,
			Keyword: e.Keyword, Verdict: e.Verdict, Reason: e.Reason, Entry: e.Entry, Judge: e.Judge,
		})
		total++
		if len(batch) >= 1000 {
			flush()
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		log.Fatalf("读取 %s 失败: %v", path, err)
	}
	_ = store.SetMeta(ctx, "imported:"+path, strconv.Itoa(total))
	fmt.Printf("firewall_events: 导入 %d 行，跳过非法 %d 行\n", total, bad)
}
