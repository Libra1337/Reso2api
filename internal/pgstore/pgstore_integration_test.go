package pgstore

import (
	"context"
	"os"
	"testing"
	"time"
)

// 集成测试门控：设置 WILDWORK_PG_TEST_DSN 后才跑（否则 skip）。
// 例：docker run --rm -p 5433:5432 -e POSTGRES_USER=t -e POSTGRES_PASSWORD=t -e POSTGRES_DB=t postgres:16-alpine
//     WILDWORK_PG_TEST_DSN="postgres://t:t@127.0.0.1:5433/t?sslmode=disable" go test ./internal/pgstore/
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("WILDWORK_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("WILDWORK_PG_TEST_DSN 未设置，跳过真实 PG 集成测试")
	}
	return dsn
}

func TestIntegrationRoundTrip(t *testing.T) {
	store, err := Open(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	rows := []ReqLogRow{
		{T: now, Model: "glm-5.3", Channel: "workbuddy", UID: "u1", Status: 200, Stream: true, TTFBMS: 120, TotalMS: 3000, InTokens: 1000, OutTokens: 500, CachedTokens: 900, Credit: 0.5, BodyFile: "a.json"},
		{T: now.Add(time.Second), Model: "kimi", Channel: "workbuddy", UID: "u2", Status: 400},
	}
	if err := store.InsertReqLogs(ctx, rows); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, total, err := store.QueryReqLogs(ctx, ReqLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total < 2 {
		t.Fatalf("total=%d", total)
	}
	// 最新在前
	if got[0].Model != "kimi" {
		t.Fatalf("first=%+v want kimi", got[0])
	}

	// 过滤
	got, total, err = store.QueryReqLogs(ctx, ReqLogFilter{Model: "glm-5.3", StatusClass: "ok", Limit: 10})
	if err != nil {
		t.Fatalf("query filtered: %v", err)
	}
	if total != 1 || got[0].Model != "glm-5.3" {
		t.Fatalf("filtered total=%d rows=%v", total, got)
	}

	// 防火墙事件 + 统计
	ev := []FirewallEventRow{{At: now.Unix(), Rule: "r1", UID: "u1", Snippet: "s"}, {At: now.Unix(), Rule: "r1"}}
	if err := store.InsertFirewallEvents(ctx, ev); err != nil {
		t.Fatalf("insert fw: %v", err)
	}
	ft, today, rules, err := store.FirewallStats(ctx, time.Local)
	if err != nil {
		t.Fatalf("fw stats: %v", err)
	}
	if ft < 2 || today < 2 || rules["r1"] < 2 {
		t.Fatalf("fw stats total=%d today=%d rules=%v", ft, today, rules)
	}

	// meta
	if err := store.SetMeta(ctx, "k", "v"); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	if v, ok, _ := store.GetMeta(ctx, "k"); !ok || v != "v" {
		t.Fatalf("meta=%q ok=%v", v, ok)
	}
}
