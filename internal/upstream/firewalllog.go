// firewalllog.go 防火墙命中事件永久落盘（jsonl 追加，永不删除）。
// 内存环形仅服务面板首屏；历史通过 Page 回看（与请求日志同语义：
// 永久保留，页面最多显示 1000 条/页）。
package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type firewallLogStore struct {
	mu      sync.Mutex
	path    string
	journal *os.File
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fwLog.path = path
	fwLog.journal = f
}

// appendFirewallLog 追加一条事件到永久日志（防火墙命中时调用）。
func appendFirewallLog(e FirewallEvent) {
	fwLog.mu.Lock()
	defer fwLog.mu.Unlock()
	if fwLog.journal == nil {
		return
	}
	if raw, err := json.Marshal(e); err == nil {
		_, _ = fwLog.journal.Write(append(raw, '\n'))
	}
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
	raw, err := os.ReadFile(fwLog.path)
	fwLog.mu.Unlock()
	if err != nil {
		// 日志为空/未启用：回落内存环
		mem := c.FirewallEvents()
		return mem, len(mem)
	}
	var all []FirewallEvent
	for _, line := range splitLines(raw) {
		var e FirewallEvent
		if json.Unmarshal(line, &e) == nil {
			all = append(all, e)
		}
	}
	total := len(all)
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
		out = append(out, all[i])
	}
	return out, total
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
