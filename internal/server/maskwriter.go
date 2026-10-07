// maskwriter.go 响应侧上游特征掩码（features.mask_upstream，默认开）。
//
// 目标不是"中性 OpenAI"，而是**按模型家族模仿各家官方 API 的响应指纹**
// （2026-10-07 官方 DeepSeek API 实测定稿）：
//
//	deepseek 家族（实测 api.deepseek.com）：
//	  id=纯 UUID（无 chatcmpl- 前缀）、model=官方名（deepseek-v4.1-flash →
//	  deepseek-flash，官方对别名请求也回显官方名）、usage 恰好六键 =
//	  {prompt_tokens, completion_tokens, total_tokens,
//	   prompt_tokens_details:{cached_tokens}, prompt_cache_hit_tokens,
//	   prompt_cache_miss_tokens}、每块带 system_fingerprint:<32hex>。
//	kimi 家族（moonshot 形）：id=cmpl-<hex>，usage 标准 OpenAI 三件套 +
//	  prompt_tokens_details.cached_tokens，无 cache 专有字段。
//	glm 家族（zhipu 形）：id=chatcmpl-<hex>，usage 同 openai 形。
//	其余：openai 中性形（id=chatcmpl-<hex>）。
//
// 上游真实指纹（cmb- 前缀 id、credit/completion_thinking_tokens 等 7 个非标
// usage 字段）全部剥除/改写。面板与 reqlog 的缓存统计在 tee 层解析（先于掩码），
// 不受影响。mask 关闭时整体旁路。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// officialModelNames 上游模型名 → 官方 API 模型名（deepseek 家族实测；
// 其余家族官方名与我们的裸名一致）。
var officialModelNames = map[string]string{
	"deepseek-v4.1-flash": "deepseek-flash",
	"deepseek-v4-flash":   "deepseek-flash",
	"deepseek-v4-pro":     "deepseek-v4-pro",
}

// maskFamily 模型 → 指纹家族。
func maskFamily(display string) string {
	switch {
	case strings.HasPrefix(display, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(display, "kimi"):
		return "kimi"
	case strings.HasPrefix(display, "glm"):
		return "glm"
	}
	return "openai"
}

// officialDisplayName 出站回显名：deepseek 家族映射官方名，其余用请求裸名。
func officialDisplayName(display string) string {
	if n, ok := officialModelNames[display]; ok {
		return n
	}
	return display
}

// stripUsageKeys 无论家族都剥除的上游特有键。
var stripUsageKeys = []string{
	"credit",
	"prompt_cache_write_tokens",
	"cache_read_input_tokens",
	"cache_creation_input_tokens",
	"completion_thinking_tokens",
	"completion_tokens_details",
	"cached_tokens", // 顶层 cached_tokens 是上游冗余；标准位在 prompt_tokens_details
}

// deepseekSysFingerprint 官方 DeepSeek 每响应带 system_fingerprint（模型版本
// 级稳定值）；进程内生成一个稳定假值，同进程所有 deepseek 响应一致。
var deepseekSysFingerprint = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

var maskRandMu sync.Mutex

func maskRandHex(n int) string {
	b := make([]byte, n)
	maskRandMu.Lock()
	_, _ = rand.Read(b)
	maskRandMu.Unlock()
	return hex.EncodeToString(b)
}

// maskID 按家族生成响应 id（流内由调用方保持稳定）。
func maskID(family string) string {
	switch family {
	case "deepseek": // 官方实测：纯 UUID v4 形态
		h := maskRandHex(16)
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	case "kimi":
		return "cmpl-" + maskRandHex(12)
	case "glm":
		// 官方实测：<14 位日期时间><17 hex>，且顶层 request_id 与 id 同值
		return time.Now().Format("20060102150405") + maskRandHex(9)[:17]
	default: // openai
		return "chatcmpl-" + maskRandHex(12)
	}
}

// sanitizeUsageFor 就地按家族清洗 usage。
func sanitizeUsageFor(u map[string]any, family string) {
	hit := 0.0
	if v, ok := u["prompt_cache_hit_tokens"].(float64); ok {
		hit = v
	}
	prompt := 0.0
	if v, ok := u["prompt_tokens"].(float64); ok {
		prompt = v
	}
	for _, k := range stripUsageKeys {
		delete(u, k)
	}
	switch family {
	case "deepseek":
		// 官方六键形态：保留 prompt_cache_hit/miss，重建 prompt_tokens_details。
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": hit}
		u["prompt_cache_hit_tokens"] = hit
		u["prompt_cache_miss_tokens"] = prompt - hit
	case "glm":
		// 官方恒三键（无任何 cache/明细字段）
		keep := map[string]any{}
		for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if v, ok := u[k]; ok {
				keep[k] = v
			}
		}
		for k := range u {
			delete(u, k)
		}
		for k, v := range keep {
			u[k] = v
		}
	default:
		// openai/moonshot 形：无 cache 专有字段，标准 cached_tokens 位。
		delete(u, "prompt_cache_hit_tokens")
		delete(u, "prompt_cache_miss_tokens")
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": hit}
	}
}

// sseMaskWriter 客户端侧 SSE 掩码写入器（行缓冲）。
type sseMaskWriter struct {
	w           http.ResponseWriter
	displayName string // 请求侧展示模型名（裸名）
	family      string
	newID       string
	buf         []byte
}

func newSSEMaskWriter(w http.ResponseWriter, displayName string) *sseMaskWriter {
	return &sseMaskWriter{w: w, displayName: displayName, family: maskFamily(displayName)}
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
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, false
	}
	changed := false
	// id：家族化改写（上游 cmb- → 官方形态），流内稳定
	if id, _ := obj["id"].(string); id != "" {
		if m.newID == "" {
			m.newID = maskID(m.family)
		}
		if id != m.newID {
			obj["id"] = m.newID
			changed = true
		}
	}
	// model：回显家族官方名
	if name := officialDisplayName(m.displayName); name != "" {
		if mdl, _ := obj["model"].(string); mdl != "" && mdl != name {
			obj["model"] = name
			changed = true
		}
	}
	// system_fingerprint：deepseek 家族官方指纹
	if m.family == "deepseek" && obj["object"] != nil {
		if fp, _ := obj["system_fingerprint"].(string); fp != deepseekSysFingerprint {
			obj["system_fingerprint"] = deepseekSysFingerprint
			changed = true
		}
	}
	// glm 家族：顶层 request_id 与 id 同值（官方实测）
	if m.family == "glm" {
		if rid, _ := obj["request_id"].(string); rid != m.newID {
			obj["request_id"] = m.newID
			changed = true
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		sanitizeUsageFor(u, m.family)
		obj["usage"] = u
		changed = true
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

// maskAggregateResp 非流式响应 map 掩码（id/model/usage/system_fingerprint）。
func maskAggregateResp(resp map[string]any, displayName string) {
	if resp == nil {
		return
	}
	family := maskFamily(displayName)
	if id, _ := resp["id"].(string); id != "" {
		resp["id"] = maskID(family)
	}
	if name := officialDisplayName(displayName); name != "" {
		resp["model"] = name
	}
	if family == "deepseek" {
		resp["system_fingerprint"] = deepseekSysFingerprint
	}
	if family == "glm" {
		if id, _ := resp["id"].(string); id != "" {
			resp["request_id"] = id
		}
	}
	if u, ok := resp["usage"].(map[string]any); ok {
		sanitizeUsageFor(u, family)
	}
}

// displayNameFor 对请求模型名取对外展示名（去渠道前缀）。
func displayNameFor(requested string) string {
	if before, after, ok := strings.Cut(requested, "/"); ok && before != "" && after != "" {
		return after
	}
	return requested
}
