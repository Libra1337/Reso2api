// firewalllog.go 防火墙命中事件永久落盘（jsonl 追加）。
// 面板翻页只读文件尾，统计走增量计数；启动时若文件过大则压缩保留尾部。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	firewallContentMaxRunes = 4000
	firewallLogCompactBytes = 32 << 20 // 超过 32MB 启动时压缩
	firewallLogKeepBytes    = 8 << 20  // 压缩后 / 翻页最多读 8MB 尾
	firewallLogMaxLine      = 8 << 20
)

type firewallLogStore struct {
	mu      sync.Mutex
	path    string
	journal *os.File
	total   int
	today   int
	todayY  string
	rules   map[string]int64
}

var fwLog firewallLogStore

// SetFirewallLogPath 初始化防火墙事件持久化文件（main 启动时调用）。
func SetFirewallLogPath(path string) {
	fwLog.mu.Lock()
	defer fwLog.mu.Unlock()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	fwLog.path = path
	if fi, err := os.Stat(path); err == nil && fi.Size() > firewallLogCompactBytes {
		compactFirewallLogLocked(path)
	}
	rebuildFirewallStatsLocked()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fwLog.journal = f
}

func compactFirewallLogLocked(path string) {
	raw, err := readLastBytes(path, firewallLogKeepBytes)
	if err != nil || len(raw) == 0 {
		return
	}
	tmp := path + ".compact"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func rebuildFirewallStatsLocked() {
	fwLog.total = 0
	fwLog.today = 0
	fwLog.todayY = time.Now().Format("2006-01-02")
	fwLog.rules = map[string]int64{}
	if fwLog.path == "" {
		return
	}
	f, err := os.Open(fwLog.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), firewallLogMaxLine)
	today := fwLog.todayY
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e struct {
			At   int64  `json:"at"`
			Rule string `json:"rule"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		fwLog.total++
		if e.Rule != "" {
			fwLog.rules[e.Rule]++
		}
		if time.Unix(e.At, 0).Format("2006-01-02") == today {
			fwLog.today++
		}
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func appendFirewallLog(e FirewallEvent) {
	fwLog.mu.Lock()
	defer fwLog.mu.Unlock()
	e.Content = truncateRunes(e.Content, firewallContentMaxRunes)
	if fwLog.journal != nil {
		if raw, err := json.Marshal(e); err == nil {
			_, _ = fwLog.journal.Write(append(raw, '\n'))
		}
	}
	if fwLog.rules == nil {
		fwLog.rules = map[string]int64{}
	}
	day := time.Now().Format("2006-01-02")
	if fwLog.todayY != day {
		fwLog.todayY = day
		fwLog.today = 0
	}
	fwLog.total++
	fwLog.today++
	if e.Rule != "" {
		fwLog.rules[e.Rule]++
	}
}

func firewallLogStats() (total, today int, rules map[string]int64) {
	fwLog.mu.Lock()
	defer fwLog.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	if fwLog.todayY != day {
		fwLog.todayY = day
		fwLog.today = 0
	}
	rules = make(map[string]int64, len(fwLog.rules))
	for k, v := range fwLog.rules {
		rules[k] = v
	}
	return fwLog.total, fwLog.today, rules
}

// FirewallLogStats 面板统计：增量计数，不扫全文件。
func (c *Client) FirewallLogStats() (total, today int, rules map[string]int64) {
	return firewallLogStats()
}

// FirewallEventsPaged 按页读取永久日志（newest-first；page=0 最新一页）。
func (c *Client) FirewallEventsPaged(page, size int) ([]FirewallEvent, int) {
	if size <= 0 || size > 1000 {
		size = 100
	}
	if page < 0 {
		page = 0
	}
	fwLog.mu.Lock()
	path := fwLog.path
	total := fwLog.total
	fwLog.mu.Unlock()
	if path == "" {
		mem := c.FirewallEvents()
		return pageSlice(mem, page, size)
	}
	raw, err := readLastBytes(path, firewallLogKeepBytes)
	if err != nil {
		mem := c.FirewallEvents()
		return pageSlice(mem, page, size)
	}
	var all []FirewallEvent
	for _, line := range splitLines(raw) {
		var e FirewallEvent
		if json.Unmarshal(line, &e) == nil {
			all = append(all, e)
		}
	}
	if total < len(all) {
		total = len(all)
	}
	start := len(all) - (page+1)*size
	if start < 0 {
		start = 0
	}
	end := len(all) - page*size
	if end < 0 {
		return nil, total
	}
	out := make([]FirewallEvent, 0, end-start)
	for i := end - 1; i >= start; i-- {
		out = append(out, all[i])
	}
	return out, total
}

func pageSlice(mem []FirewallEvent, page, size int) ([]FirewallEvent, int) {
	total := len(mem)
	start := total - (page+1)*size
	if start < 0 {
		start = 0
	}
	end := total - page*size
	if end < 0 {
		return nil, total
	}
	out := make([]FirewallEvent, 0, end-start)
	for i := end - 1; i >= start; i-- {
		out = append(out, mem[i])
	}
	return out, total
}

func readLastBytes(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	start := size - max
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, nil
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}
