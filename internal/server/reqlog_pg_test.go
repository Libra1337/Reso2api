package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"wild-work/internal/pgstore"
)

// fakePG 内存假后端：验证 PG 模式的入队/批量落库/内存环一致性。
type fakePG struct {
	mu       sync.Mutex
	inserted []pgstore.ReqLogRow
	queryNil bool // QueryReqLogs 返回空
}

func (f *fakePG) InsertReqLogs(_ context.Context, rows []pgstore.ReqLogRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, rows...)
	return nil
}

func (f *fakePG) QueryReqLogs(_ context.Context, flt pgstore.ReqLogFilter) ([]pgstore.ReqLogRow, int, error) {
	if f.queryNil {
		return nil, 0, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// 倒序一页
	var out []pgstore.ReqLogRow
	for i := len(f.inserted) - 1; i >= 0 && len(out) < flt.Limit; i-- {
		out = append(out, f.inserted[i])
	}
	return out, len(f.inserted), nil
}

func (f *fakePG) rows() []pgstore.ReqLogRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pgstore.ReqLogRow(nil), f.inserted...)
}

// PG 模式：add 走队列、批量落库、内存环同步维护、jsonl 停写。
func TestReqLogPGMode(t *testing.T) {
	fk := &fakePG{}
	s := &reqLogStore{pg: fk}
	s.load()

	now := time.Now()
	for i := 0; i < 3; i++ {
		s.add(ReqLog{Time: now.Format("01-02 15:04:05"), Model: "m", At: now.Add(time.Duration(i) * time.Second)})
	}
	s.close() // close 触发 flush，等 pgDone

	rows := fk.rows()
	if len(rows) != 3 {
		t.Fatalf("inserted=%d want 3", len(rows))
	}
	if !rows[0].T.Equal(now) {
		t.Fatalf("row0 t=%v want %v", rows[0].T, now)
	}
	if len(s.logs) != 3 {
		t.Fatalf("memory ring=%d want 3", len(s.logs))
	}
	if s.journal != nil {
		t.Fatal("pg mode must not open jsonl journal")
	}
}

// PG 模式 Page：走 QueryReqLogs，时间串还原为面板格式。
func TestReqLogPGPage(t *testing.T) {
	fk := &fakePG{}
	s := &reqLogStore{pg: fk}
	s.load()
	now := time.Now()
	s.add(ReqLog{Time: now.Format("01-02 15:04:05"), Model: "glm-5.3", UID: "u1", At: now})
	s.close()

	out, total := s.Page(0, 100)
	if total != 1 || len(out) != 1 {
		t.Fatalf("page total=%d len=%d", total, len(out))
	}
	if out[0].Model != "glm-5.3" || out[0].UID != "u1" {
		t.Fatalf("row=%+v", out[0])
	}
	if out[0].Time != now.Local().Format("01-02 15:04:05") {
		t.Fatalf("time=%q want panel format", out[0].Time)
	}
}

// PG 模式启动种子：load 时从 PG 恢复最近一页到内存环（旧→新）。
func TestReqLogPGSeed(t *testing.T) {
	fk := &fakePG{}
	t0 := time.Now().Add(-time.Hour)
	for i := 0; i < 2; i++ {
		_ = fk.InsertReqLogs(context.Background(), []pgstore.ReqLogRow{{T: t0.Add(time.Duration(i) * time.Minute), Model: "m"}})
	}
	s := &reqLogStore{pg: fk}
	s.load()
	defer s.close()
	if len(s.logs) != 2 {
		t.Fatalf("seeded=%d want 2", len(s.logs))
	}
	if !s.logs[0].At.Before(s.logs[1].At) {
		t.Fatal("ring must be oldest→newest")
	}
}

// file 模式不受影响（既有 TestReqLogJournalPersistence 覆盖 jsonl 语义；
// 此处验证 pg=nil 时 add 不 panic 且不写 journal 空路径场景由既有测试覆盖）。
func TestReqLogFileModeNoPG(t *testing.T) {
	s := &reqLogStore{}
	s.add(ReqLog{Model: "m", At: time.Now()})
	if len(s.logs) != 1 {
		t.Fatalf("ring=%d", len(s.logs))
	}
}
