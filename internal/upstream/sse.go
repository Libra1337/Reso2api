// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		sawFinish     bool
		sawDone       bool
		usage         map[string]any
		gotAnyContent bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				sawDone = true
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
								sawFinish = true
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								} else if rc, ok := delta["reasoning"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
								// message 形态的 tool_calls 同样合并（非流式回包兜底）
								if tcs, ok := msg["tool_calls"].([]any); ok {
									for j, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										merged, seen := toolCalls[j]
										if !seen {
											merged = map[string]any{"index": j}
											toolCalls[j] = merged
											toolOrder = append(toolOrder, j)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	if !sawDone && !sawFinish {
		return nil, errors.New("upstream stream truncated before completion (no [DONE], no finish_reason)")
	}
	// 有 [DONE] 但缺 finish_reason 的宽容兜底：带 tool_calls 就报 tool_calls，否则报 stop。
	if !sawFinish {
		if len(toolOrder) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		// 只发 reasoning_content 单字段：同时发 reasoning+reasoning_content 双字段时，
		// 累加型客户端（把两字段都拼进思考流）会逐段翻倍出现 "ThereThere's's" 乱码。
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// Stream 透传上游 SSE 到 w（每行 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// chunkRebuilder 把上游噪音分片重建成规范 OpenAI chunk（白名单 + 去空）。
// 背景：实测 glm-5.3 等模型的每个 delta 都携带全字段空值
// （content:""/reasoning_content:""/tool_calls:[]/function_call/extra_fields），
// 且 tool_calls 续片带空 name —— 逐字透传会让下游协议转换层
// （Anthropic 化网关/客户端）解析崩溃。重建规则（对齐 OpenAI 规范形态，
// 严格的下游转换器依赖这些约定）：
//   - 每帧都带 id/object/created/model（首帧记忆，后续复用）
//   - 首个带 delta 的帧强制携带 role:"assistant"（部分模型 role 只在末帧出现）
//   - delta 只保留非空字段
//   - tool_calls 片段剔除空 id/type/name/arguments；纯噪音帧整帧丢弃
//   - usage 单独成帧时补 "choices":[]（规范形态，防 choices[0] 越界）
//   - 上游 error 帧原样透传（不吞错）
type chunkRebuilder struct {
	id, model  string
	created    any
	headerSeen bool
	roleSent   bool
}

func (rb *chunkRebuilder) rebuild(chunk map[string]any) map[string]any {
	out := map[string]any{}
	// 上游 error 帧原样透传（含 choices/usage 均无的纯错误帧）
	if _, ok := chunk["error"]; ok {
		for _, k := range []string{"id", "object", "created", "model"} {
			if v, ok := chunk[k]; ok && v != nil {
				out[k] = v
			}
		}
		out["error"] = chunk["error"]
		return out
	}
	if !rb.headerSeen {
		for _, k := range []string{"id", "object", "created", "model"} {
			if v, ok := chunk[k]; ok && v != nil {
				out[k] = v
				switch k {
				case "id":
					rb.id, _ = v.(string)
				case "model":
					rb.model, _ = v.(string)
				case "created":
					rb.created = v
				}
			}
		}
		rb.headerSeen = true
	} else {
		// 每帧补全帧头（OpenAI 规范：所有 chunk 都携带这四字段）
		if rb.id != "" {
			out["id"] = rb.id
		}
		out["object"] = "chat.completion.chunk"
		if rb.created != nil {
			out["created"] = rb.created
		}
		if rb.model != "" {
			out["model"] = rb.model
		}
	}
	choices, _ := chunk["choices"].([]any)
	var outChoices []any
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		nc := map[string]any{}
		if v, ok := c["index"]; ok {
			nc["index"] = v
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			nc["finish_reason"] = fr
		}
		delta, _ := c["delta"].(map[string]any)
		nd := map[string]any{}
		if s, ok := delta["content"].(string); ok && s != "" {
			nd["content"] = s
		}
		// 推理内容单字段输出 reasoning_content：上游若同帧带 reasoning（OpenAI 系别名）
		// 只取其一；双字段同时发会被累加型客户端拼成逐段翻倍乱码。
		for _, rk := range []string{"reasoning_content", "reasoning"} {
			if s, ok := delta[rk].(string); ok && s != "" {
				nd["reasoning_content"] = s
				break
			}
		}
		if role, ok := delta["role"].(string); ok && role != "" && !rb.roleSent {
			nd["role"] = role
			rb.roleSent = true
		}
		if tcs := cleanToolCallFragments(delta["tool_calls"]); len(tcs) > 0 {
			nd["tool_calls"] = tcs
		}
		// 首个带实质 delta 的帧强制补 role（部分模型只在末帧发 role，
		// 下游转换器依赖首帧 role 开启 assistant 消息）
		if !rb.roleSent && len(nd) > 0 {
			nd["role"] = "assistant"
			rb.roleSent = true
		}
		if len(nd) > 0 || nc["finish_reason"] != nil {
			nc["delta"] = nd
			outChoices = append(outChoices, nc)
		}
	}
	if len(outChoices) > 0 {
		out["choices"] = outChoices
	}
	if u, ok := chunk["usage"]; ok && u != nil {
		out["usage"] = u
		// usage 单独成帧时补规范空 choices（防严格客户端 choices[0] 越界）
		if _, has := out["choices"]; !has {
			out["choices"] = []any{}
		}
	}
	// 空帧（无 choices 且无 usage）整帧丢弃
	if _, hasChoices := out["choices"]; !hasChoices {
		if _, hasUsage := out["usage"]; !hasUsage {
			return nil
		}
	}
	return out
}

// cleanToolCallFragments 剔除 tool_call 分片里的空值字段与纯噪音片。
func cleanToolCallFragments(v any) []map[string]any {
	tcs, ok := v.([]any)
	if !ok || len(tcs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tcs))
	for _, ti := range tcs {
		tc, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		n := map[string]any{}
		if idx, ok := tc["index"]; ok {
			n["index"] = idx
		}
		if s, ok := tc["id"].(string); ok && s != "" {
			n["id"] = s
		}
		if s, ok := tc["type"].(string); ok && s != "" {
			n["type"] = s
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			nfn := map[string]any{}
			if s, ok := fn["name"].(string); ok && s != "" {
				nfn["name"] = s
			}
			if s, ok := fn["arguments"].(string); ok && s != "" {
				nfn["arguments"] = s
			}
			if len(nfn) > 0 {
				n["function"] = nfn
			}
		}
		// 只有 index 没有任何实质内容的纯噪音片丢弃
		if len(n) <= 1 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Stream 逐行读取上游 SSE，经白名单重建后转发；保证恰好一个 [DONE]。
// 上游中途死亡（读错误 / 未发 finish_reason 与 [DONE] 就 EOF）时，
// 补写 error 帧 + [DONE] 让客户端优雅收尾（可自动重试），不裸断连接。
func Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64*1024)
	sawDone := false
	sawFinish := false
	rb := &chunkRebuilder{}
	write := func(payload string) error {
		if _, werr := io.WriteString(w, payload); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeTerminal := func(reason string) error {
		if sawDone {
			return nil
		}
		errFrame := map[string]any{
			"error": map[string]any{
				"type":    "upstream_stream_ended",
				"message": reason,
			},
		}
		if raw, merr := json.Marshal(errFrame); merr == nil {
			_ = write("data: " + string(raw) + "\n\n")
		}
		return write("data: [DONE]\n\n")
	}
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if payload, ok := trimDataPrefix(trimmed); ok {
				if payload == "[DONE]" {
					sawDone = true
					if werr := write("data: [DONE]\n\n"); werr != nil {
						return werr
					}
					continue
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if rebuilt := rb.rebuild(chunk); rebuilt != nil {
						if hasFinishReason(rebuilt) {
							sawFinish = true
						}
						raw, merr := json.Marshal(rebuilt)
						if merr == nil {
							if werr := write("data: " + string(raw) + "\n\n"); werr != nil {
								return werr
							}
						}
					}
				} else {
					// 非 JSON 数据行原样透传（error 帧等）
					if werr := write(line); werr != nil {
						return werr
					}
				}
			} else if trimmed != "" {
				// 非 data 行（如裸 error 事件行）原样透传
				if werr := write(line + "\n"); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			// 传输层错误：上游中途死亡 → 补终止帧（客户端还能拿到已生成内容）
			_ = writeTerminal("upstream stream aborted: " + err.Error())
			return err
		}
	}
	if !sawDone {
		if sawFinish {
			// 正常收尾但上游漏发 [DONE]：补写
			if werr := write("data: [DONE]\n\n"); werr != nil {
				return werr
			}
		} else {
			// EOF 且未见 finish_reason：模型未完成就断 → error 帧 + [DONE]
			if werr := writeTerminal("upstream stream ended before completion"); werr != nil {
				return werr
			}
		}
	}
	return nil
}

// trimDataPrefix 剥离 SSE data 前缀（兼容 "data: " 与 "data:" 两种形式）。
func trimDataPrefix(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	rest := line[5:]
	if strings.HasPrefix(rest, " ") {
		rest = rest[1:]
	}
	return rest, true
}

// hasFinishReason 判断重建后的 chunk 是否携带 finish_reason。
func hasFinishReason(chunk map[string]any) bool {
	choices, _ := chunk["choices"].([]any)
	for _, ci := range choices {
		if c, ok := ci.(map[string]any); ok {
			if _, ok := c["finish_reason"]; ok {
				return true
			}
		}
	}
	return false
}
