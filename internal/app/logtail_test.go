package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 小文件（未触发尾部定位）：全部行都应返回，不丢首行。
func TestReadLogTailSmallFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	if err := os.WriteFile(fp, []byte("line1\nline2\nline3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readLogTail(fp, 1<<20, 300)
	// 末尾换行会产生一个空串元素，内容行必须齐全
	if len(got) < 3 || got[0] != "line1" || got[1] != "line2" || got[2] != "line3" {
		t.Fatalf("小文件不应丢行，got=%q", got)
	}
}

// 文件超过窗口：只返回尾部，且首行不能是被切断的半截行。
func TestReadLogTailTruncatesToWholeLines(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "log line %04d %s\n", i, strings.Repeat("x", 100))
	}
	if err := os.WriteFile(fp, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// 窗口 10KB 远小于文件（~570KB），必然触发尾部定位
	got := readLogTail(fp, 10*1024, 300)
	if len(got) == 0 {
		t.Fatal("want lines, got none")
	}
	// 每一行要么是空串（末尾换行产物），要么是完整的 "log line NNNN xxx..." 形态
	for i, ln := range got {
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "log line ") {
			t.Fatalf("第 %d 行是半截行: %q", i, ln)
		}
	}
	// 必须取到的是文件末尾的内容
	last := ""
	for _, ln := range got {
		if ln != "" {
			last = ln
		}
	}
	if !strings.HasPrefix(last, "log line 4999 ") {
		t.Fatalf("应返回文件尾部，末行=%q", last)
	}
}

// keep 上限生效。
func TestReadLogTailKeepLimit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&sb, "l%d\n", i)
	}
	if err := os.WriteFile(fp, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readLogTail(fp, 1<<20, 50)
	if len(got) > 50 {
		t.Fatalf("keep=50 应裁到 50 行以内，got=%d", len(got))
	}
}

// 文件不存在：返回空切片而非 nil，避免面板 JSON 出现 null。
func TestReadLogTailMissingFile(t *testing.T) {
	got := readLogTail(filepath.Join(t.TempDir(), "nope.log"), 1<<20, 300)
	if got == nil {
		t.Fatal("want empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("want no lines, got %q", got)
	}
}
