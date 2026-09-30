// pgstore — 可选的 PostgreSQL 存储模式（请求日志 + 防火墙事件）。
//
// 设计要点：
//   - 纯 Go 驱动（jackc/pgx v5），兼容 CGO_ENABLED=0 交叉编译；
//   - 依赖方向单向：server / upstream → pgstore，本包不 import 任何内部包，
//     行结构在此独立定义（ReqLogRow / FirewallEventRow），避免环；
//   - 启动时 Open 建表（幂等 CREATE TABLE IF NOT EXISTS），连不上由调用方
//     决定 fatal（网关 main 显式失败）；
//   - 写路径为批量 INSERT（调用方异步聚合），读路径为分页/过滤/聚合查询。
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReqLogRow 请求日志行（与 server.ReqLog 字段一一对应，Time 用真实时间戳）。
type ReqLogRow struct {
	T            time.Time
	Model        string
	Channel      string
	UID          string
	Status       int
	Stream       bool
	TTFBMS       int64
	TotalMS      int64
	InTokens     int64
	OutTokens    int64
	CachedTokens int64
	Credit       float64
	BodyFile     string
}

// FirewallEventRow 防火墙事件行（与 upstream.FirewallEvent 字段对应，At 为 unix 秒）。
type FirewallEventRow struct {
	At      int64
	Rule    string
	UID     string
	Nick    string
	Model   string
	Snippet string
	Content string
	Match   string
	Observe bool
	Keyword string
	Verdict string
	Reason  string
	Entry   string
	Judge   string
}

// Store PostgreSQL 连接池封装。
type Store struct {
	pool *pgxpool.Pool
}

const schema = `
CREATE TABLE IF NOT EXISTS request_logs (
	id            BIGSERIAL PRIMARY KEY,
	t             TIMESTAMPTZ NOT NULL,
	model         TEXT NOT NULL DEFAULT '',
	channel       TEXT NOT NULL DEFAULT '',
	uid           TEXT NOT NULL DEFAULT '',
	status        INT NOT NULL DEFAULT 0,
	stream        BOOLEAN NOT NULL DEFAULT false,
	ttfb_ms       BIGINT NOT NULL DEFAULT 0,
	total_ms      BIGINT NOT NULL DEFAULT 0,
	in_tokens     BIGINT NOT NULL DEFAULT 0,
	out_tokens    BIGINT NOT NULL DEFAULT 0,
	cached_tokens BIGINT NOT NULL DEFAULT 0,
	credit        DOUBLE PRECISION NOT NULL DEFAULT 0,
	body_file     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_logs_t ON request_logs (t DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs (model, t DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs (status) WHERE status >= 400;
CREATE INDEX IF NOT EXISTS idx_request_logs_uid ON request_logs (uid);

CREATE TABLE IF NOT EXISTS firewall_events (
	id      BIGSERIAL PRIMARY KEY,
	at      BIGINT NOT NULL,
	rule    TEXT NOT NULL DEFAULT '',
	uid     TEXT NOT NULL DEFAULT '',
	nick    TEXT NOT NULL DEFAULT '',
	model   TEXT NOT NULL DEFAULT '',
	snippet TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	match   TEXT NOT NULL DEFAULT '',
	observe BOOLEAN NOT NULL DEFAULT false,
	keyword TEXT NOT NULL DEFAULT '',
	verdict TEXT NOT NULL DEFAULT '',
	reason  TEXT NOT NULL DEFAULT '',
	entry   TEXT NOT NULL DEFAULT '',
	judge   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_firewall_events_at ON firewall_events (at DESC);
CREATE INDEX IF NOT EXISTS idx_firewall_events_rule ON firewall_events (rule);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`

// Open 连接并建表。dsn 例：postgres://user:pass@host:5432/db?sslmode=disable
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: parse dsn: %w", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close 关闭连接池。
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping 探活。
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("pgstore: not open")
	}
	return s.pool.Ping(ctx)
}

// InsertReqLogs 批量插入请求日志（空切片直接成功）。
func (s *Store) InsertReqLogs(ctx context.Context, rows []ReqLogRow) error {
	if len(rows) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	const q = `INSERT INTO request_logs
		(t, model, channel, uid, status, stream, ttfb_ms, total_ms, in_tokens, out_tokens, cached_tokens, credit, body_file)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	for _, r := range rows {
		b.Queue(q, r.T, r.Model, r.Channel, r.UID, r.Status, r.Stream,
			r.TTFBMS, r.TotalMS, r.InTokens, r.OutTokens, r.CachedTokens, r.Credit, r.BodyFile)
	}
	return s.pool.SendBatch(ctx, b).Close()
}

// InsertFirewallEvents 批量插入防火墙事件。
func (s *Store) InsertFirewallEvents(ctx context.Context, rows []FirewallEventRow) error {
	if len(rows) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	const q = `INSERT INTO firewall_events
		(at, rule, uid, nick, model, snippet, content, match, observe, keyword, verdict, reason, entry, judge)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`
	for _, r := range rows {
		b.Queue(q, r.At, r.Rule, r.UID, r.Nick, r.Model, r.Snippet, r.Content, r.Match,
			r.Observe, r.Keyword, r.Verdict, r.Reason, r.Entry, r.Judge)
	}
	return s.pool.SendBatch(ctx, b).Close()
}

// ReqLogFilter 分页 + 过滤。StatusClass: "" | "ok" | "err"；Since/Until 为 unix 秒。
type ReqLogFilter struct {
	Model       string
	UID         string
	StatusClass string
	Since       int64
	Until       int64
	Limit       int
	Offset      int
}

// QueryReqLogs 按 id 倒序取一页 + 过滤条件下的真实总数。
func (s *Store) QueryReqLogs(ctx context.Context, f ReqLogFilter) ([]ReqLogRow, int, error) {
	where, args := reqLogWhere(f)
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t, model, channel, uid, status, stream, ttfb_ms, total_ms, in_tokens, out_tokens, cached_tokens, credit, body_file
		FROM request_logs`+where+`
		ORDER BY id DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2),
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ReqLogRow
	for rows.Next() {
		var r ReqLogRow
		if err := rows.Scan(&r.T, &r.Model, &r.Channel, &r.UID, &r.Status, &r.Stream,
			&r.TTFBMS, &r.TotalMS, &r.InTokens, &r.OutTokens, &r.CachedTokens, &r.Credit, &r.BodyFile); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func reqLogWhere(f ReqLogFilter) (string, []any) {
	var conds []string
	var args []any
	n := 0
	add := func(cond string, val any) {
		n++
		conds = append(conds, fmt.Sprintf(cond, n))
		args = append(args, val)
	}
	if f.Model != "" {
		add("model = $%d", f.Model)
	}
	if f.UID != "" {
		add("uid = $%d", f.UID)
	}
	switch f.StatusClass {
	case "ok":
		add("status < $%d", 400)
	case "err":
		add("status >= $%d", 400)
	}
	if f.Since > 0 {
		add("t >= to_timestamp($%d)", f.Since)
	}
	if f.Until > 0 {
		add("t <= to_timestamp($%d)", f.Until)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// QueryFirewallEvents 按 id 倒序取一页 + 总数。
func (s *Store) QueryFirewallEvents(ctx context.Context, limit, offset int) ([]FirewallEventRow, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT at, rule, uid, nick, model, snippet, content, match, observe, keyword, verdict, reason, entry, judge
		FROM firewall_events ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []FirewallEventRow
	for rows.Next() {
		var r FirewallEventRow
		if err := rows.Scan(&r.At, &r.Rule, &r.UID, &r.Nick, &r.Model, &r.Snippet, &r.Content,
			&r.Match, &r.Observe, &r.Keyword, &r.Verdict, &r.Reason, &r.Entry, &r.Judge); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM firewall_events`).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// FirewallStats 聚合：总数 / 今日数（本地时区）/ 规则计数。
func (s *Store) FirewallStats(ctx context.Context, local *time.Location) (total int, today int, rules map[string]int64, err error) {
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM firewall_events`).Scan(&total); err != nil {
		return 0, 0, nil, err
	}
	now := time.Now().In(local)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, local).Unix()
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM firewall_events WHERE at >= $1`, todayStart).Scan(&today); err != nil {
		return 0, 0, nil, err
	}
	rules = map[string]int64{}
	rows, err := s.pool.Query(ctx, `SELECT rule, count(*) FROM firewall_events GROUP BY rule`)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule string
		var n int64
		if err := rows.Scan(&rule, &n); err != nil {
			return 0, 0, nil, err
		}
		if rule != "" {
			rules[rule] = n
		}
	}
	return total, today, rules, rows.Err()
}

// EnforceRetention 删除保留期外的请求日志，返回删除行数；days<=0 不删除。
func (s *Store) EnforceRetention(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM request_logs WHERE t < now() - ($1 || ' days')::interval`, days)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// GetMeta / SetMeta 一次性导入标记等元信息。
func (s *Store) GetMeta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM meta WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO meta (key, value) VALUES ($1,$2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
