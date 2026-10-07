// maskwriter.go 响应侧上游特征掩码（features.mask_upstream，默认开）。
//
// 出站响应里可被识别为特定上游的指纹：
//   - 响应 id 前缀（如 cmb-…）：OpenAI 形态是 chatcmpl-…；
//   - usage 的非标字段：credit / prompt_cache_hit_tokens / prompt_cache_miss_tokens /
//     prompt_cache_write_tokens / cache_read_input_tokens / cache_creation_input_tokens /
//     completion_thinking_tokens（上游计费口径特有）；
//   - model 回显与请求名不一致（带前缀调用时回显裸名）。
//
// 处理：SSE 逐行变换（id 换为流内稳定的 chatcmpl- 随机段；model 改为请求的
// 展示名；含 usage 的块解析后剔除非标字段，命中数并入标准
// prompt_tokens_details.cached_tokens 与顶层 cached_tokens）。非 data 行
// （注释心跳/空行）原样透传；mask 关闭时整体旁路。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// nonstandardUsageKeys 上游特有 usage 字段（对外剥除；面板统计在 tee 层
// 解析，先于掩码，不受影响）。
var nonstandardUsageKeys = []string{
	"credit",
	"prompt_cache_hit_tokens",
	"prompt_cache_miss_tokens",
	"prompt_cache_write_tokens",
	"cache_read_input_tokens",
	"cache_creation_input_tokens",
	"completion_thinking_tokens",
}

// sseMaskWriter 客户端侧 SSE 掩码写入器（行缓冲）。
type sseMaskWriter struct {
	w           http.ResponseWriter
	displayName string // 请求侧展示模型名（裸名）；空=不改写 model
	newID       string
	buf         []byte
}

func newSSEMaskWriter(w http.ResponseWriter, displayName string) *sseMaskWriter {
	return &sseMaskWriter{w: w, displayName: displayName}
}

func (m *sseMaskWriter) Header() http.Header { return m.w.Header() }

func (m *sseMaskWriter) WriteHeader(code int) {
	if wh, ok := m.w.(http.ResponseWriter); ok {
		wh.WriteHeader(code)
	}
}

func (m *sseMaskWriter) Write(p []byte) (int, error) {
	m.buf = append(m.buf, p...)
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		line := m.buf[:i+1]
		m.buf = m.buf[i+1:]
		if out, ok := m.transformLine(line); ok {
			_, _ = m.w.Write(out)
		} else {
			_, _ = m.w.Write(line)
		}
	}
	return len(p), nil
}

func (m *sseMaskWriter) Flush() {
	if len(m.buf) > 0 { // 流结尾未换行的残行
		if out, ok := m.transformLine(append(m.buf, '\n')); ok {
			_, _ = m.w.Write(out)
		} else {
			_, _ = m.w.Write(m.buf)
		}
		m.buf = nil
	}
	if f, ok := m.w.(http.Flusher); ok {
		f.Flush()
	}
}

// transformLine 尝试掩码一行 SSE；返回 (新行, 是否修改)。
func (m *sseMaskWriter) transformLine(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) || len(trimmed) < 6 {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[5:])
	if len(payload) == 0 || payload[0] != '{' {
		return nil, false
	}
	changed := false
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, false
	}
	// id：非 chatcmpl- 前缀 → 流内稳定的随机 chatcmpl-
	if id, _ := obj["id"].(string); id != "" && !strings.HasPrefix(id, "chatcmpl-") {
		if m.newID == "" {
			m.newID = "chatcmpl-" + randHex12()
		}
		obj["id"] = m.newID
		changed = true
	}
	// model：回显请求侧展示名
	if m.displayName != "" {
		if mdl, _ := obj["model"].(string); mdl != "" && mdl != m.displayName {
			obj["model"] = m.displayName
			changed = true
		}
	}
	// usage：剥非标字段，命中数并入标准位
	if u, ok := obj["usage"].(map[string]any); ok {
		if sanitizeUsage(u) {
			obj["usage"] = u
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	nl := "\n"
	if bytes.HasSuffix(line, []byte("\r\n")) {
		nl = "\r\n"
	}
	return append([]byte("data: "), append(out, nl...)...), true
}

// sanitizeUsage 就地清洗 usage map：删非标键；prompt_cache_hit_tokens 值并入
// prompt_tokens_details.cached_tokens 与顶层 cached_tokens。返回是否修改。
func sanitizeUsage(u map[string]any) bool {
	changed := false
	var hit float64
	if v, ok := u["prompt_cache_hit_tokens"].(float64); ok {
		hit = v
	}
	for _, k := range nonstandardUsageKeys {
		if _, ok := u[k]; ok {
			delete(u, k)
			changed = true
		}
	}
	if hit > 0 {
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			d["cached_tokens"] = hit
			u["prompt_tokens_details"] = d
		}
		u["cached_tokens"] = hit
		changed = true
	}
	return changed
}

// maskAggregateResp 非流式响应 map 掩码（id/model/usage 同上）。
func maskAggregateResp(resp map[string]any, displayName string) {
	if resp == nil {
		return
	}
	if id, _ := resp["id"].(string); id != "" && !strings.HasPrefix(id, "chatcmpl-") {
		resp["id"] = "chatcmpl-" + randHex12()
	}
	if displayName != "" {
		if mdl, _ := resp["model"].(string); mdl != "" {
			resp["model"] = displayName
		}
	}
	if u, ok := resp["usage"].(map[string]any); ok {
		sanitizeUsage(u)
	}
}

func randHex12() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// displayNameFor 对请求模型名取对外展示名（去渠道前缀）。
func displayNameFor(requested string) string {
	if before, after, ok := strings.Cut(requested, "/"); ok && before != "" && after != "" {
		return after
	}
	return requested
}
