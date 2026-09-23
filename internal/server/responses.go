// responses.go — OpenAI Responses API（/v1/responses）兼容层。
// Codex 等 Responses 协议客户端 → 翻译为 chat completions 发上游 →
// 上游 chat SSE 翻译回 Responses 事件流（response.created / output_text.delta /
// function_call_arguments.delta / response.completed）。
package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// respInputItem Responses input 数组元素（宽松解析，未知类型跳过）。
type respInputItem struct {
	Type string `json:"type"`
	// message
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// function_call / function_call_output
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// respContentPart Responses 消息 content 部件（宽松解析，未知类型跳过）。
// input_image 的 image_url 在 Responses 协议里是字符串，但部分客户端发
// {url,detail} 对象，故用 any 兼容两种形态。
type respContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL any    `json:"image_url"`
	Detail   string `json:"detail"`
}

// respTool Responses 扁平函数定义 {type:function, name, description, parameters}。
type respTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// translateResponsesToChat 把 Responses 请求体翻译为 chat completions 请求体。
func translateResponsesToChat(raw []byte) ([]byte, string, error) {
	var req struct {
		Model        string          `json:"model"`
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
		Tools        []respTool      `json:"tools"`
		ToolChoice   any             `json:"tool_choice"`
		Stream       bool            `json:"stream"`
		Reasoning    *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		MaxOutputTokens *int     `json:"max_output_tokens"`
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"top_p"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, "", fmt.Errorf("responses parse: %w", err)
	}
	if req.Model == "" {
		return nil, "", fmt.Errorf("missing model")
	}

	chat := map[string]any{
		"model":  req.Model,
		"stream": true, // 上游只支持流式；非流式由本层聚合
	}
	var messages []map[string]any
	if strings.TrimSpace(req.Instructions) != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": req.Instructions,
		})
	}

	// input：字符串 或 item 数组
	if len(req.Input) > 0 && string(req.Input) != "null" {
		var asStr string
		if err := json.Unmarshal(req.Input, &asStr); err == nil && asStr != "" {
			messages = append(messages, map[string]any{"role": "user", "content": asStr})
		} else {
			var items []respInputItem
			if err := json.Unmarshal(req.Input, &items); err != nil {
				return nil, "", fmt.Errorf("input parse: %w", err)
			}
			var pendingCalls []map[string]any // 连续 function_call 合并为一条 assistant 消息
			flushCalls := func() {
				if len(pendingCalls) > 0 {
					messages = append(messages, map[string]any{
						"role":       "assistant",
						"tool_calls": pendingCalls,
					})
					pendingCalls = nil
				}
			}
			for _, it := range items {
				switch it.Type {
				case "message":
					flushCalls()
					role := it.Role
					if role == "" {
						role = "user"
					}
					text, images := responsesContent(it.Content)
					messages = append(messages, map[string]any{
						"role":    role,
						"content": responsesChatContent(text, images),
					})
				case "function_call":
					call := map[string]any{
						"id":   it.CallID,
						"type": "function",
						"function": map[string]any{
							"name":      it.Name,
							"arguments": it.Arguments,
						},
					}
					pendingCalls = append(pendingCalls, call)
				case "function_call_output":
					flushCalls()
					out := it.Output
					if len(out) == 0 {
						out = json.RawMessage(`""`)
					}
					text, images := responsesContent(out)
					messages = append(messages, map[string]any{
						"role":         "tool",
						"tool_call_id": it.CallID,
						"content":      text,
					})
					// chat 的 role:tool 只承载文本；工具结果里的图片挪到紧随
					// 其后的合成 user 消息（上游在非 user 轮丢弃图片，见
					// upstream/images.go 的 relocateAssistantImages）。保序：
					// 图仍在原工具结果之后，语义不漂移。
					if len(images) > 0 {
						messages = append(messages, map[string]any{
							"role":    "user",
							"content": responsesChatContent("", images),
						})
					}
				default:
					// reasoning / item_reference 等忽略
				}
			}
			flushCalls()
		}
	}
	if len(messages) == 0 {
		return nil, "", fmt.Errorf("empty input")
	}
	chat["messages"] = messages

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" || t.Name == "" {
				continue
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		if len(tools) > 0 {
			chat["tools"] = tools
			if tc := translateToolChoice(req.ToolChoice); tc != nil {
				chat["tool_choice"] = tc
			}
		}
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		chat["reasoning_effort"] = req.Reasoning.Effort
	}
	if req.MaxOutputTokens != nil {
		chat["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, "", err
	}
	return out, req.Model, nil
}

// responsesContent 解析 Responses 消息 content（字符串或部件数组），返回
// 拼接后的文本与其中的图片 part（chat image_url 形状）。
//
// 文本拼接规则与旧 contentToText 一致：非空 text 以 \n 连接，纯文本请求的
// 返回值逐字不变（调用方据此保持出站形状不退化）。
func responsesContent(raw json.RawMessage) (string, []any) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []respContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw), nil
	}
	var texts []string
	var images []any
	for _, p := range parts {
		switch p.Type {
		case "input_image":
			if img := responsesImagePart(p); img != nil {
				images = append(images, img)
			}
		case "input_text", "output_text", "text":
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

// responsesChatContent 文本 + 图片 → chat completions content：
// 无图返回字符串（形状与纯文本路径一致），有图返回多模态数组
// （text part 在前，image part 按原始顺序在后）。
func responsesChatContent(text string, images []any) any {
	if len(images) == 0 {
		return text
	}
	arr := make([]any, 0, len(images)+1)
	if text != "" {
		arr = append(arr, map[string]any{"type": "text", "text": text})
	}
	return append(arr, images...)
}

// responsesImagePart Responses image 部件 → chat image_url part。
// image_url 兼容字符串与 {url,detail} 对象两种形态；取不到 url 返回 nil
//（file_id 引用形态在 chat 协议里无对应承载，丢弃）。这里不补 data URL
// 前缀——出站管线 upstream.normalizeImageURLs 会按 base64 魔数嗅探补全。
func responsesImagePart(p respContentPart) map[string]any {
	url, detail := "", p.Detail
	switch v := p.ImageURL.(type) {
	case string:
		url = v
	case map[string]any:
		url, _ = v["url"].(string)
		if detail == "" {
			detail, _ = v["detail"].(string)
		}
	}
	if url == "" {
		return nil
	}
	iu := map[string]any{"url": url}
	if detail != "" {
		iu["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": iu}
}

// translateToolChoice Responses tool_choice → chat tool_choice。
func translateToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		return v
	case map[string]any:
		if t, _ := v["type"].(string); t == "function" {
			if name, _ := v["name"].(string); name != "" {
				return name
			}
			if fn, ok := v["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					return name
				}
			}
		} else if t == "auto" || t == "none" || t == "required" {
			return t
		}
	}
	return nil
}

// writeResponsesError 以 Responses 错误形状返回 HTTP 错误。
func writeResponsesError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    "responses_error",
			"message": msg,
		},
	})
}

// responsesRelay 把上游 chat SSE 翻译为 Responses 事件流（stream=true）或
// 聚合为单个 response JSON（stream=false）。返回 usage 与首事件耗时（TTFB）。
func (h *Handler) responsesRelay(w http.ResponseWriter, rc io.ReadCloser, model string, stream bool, t0 time.Time) (map[string]any, time.Duration) {
	respID := "resp_" + randHex(12)
	var ttfb time.Duration

	var text strings.Builder
	var reasoning strings.Builder
	reasoningIdx := -1
	reasoningItemID := "rs_0"
	type fnCall struct {
		ItemID string
		CallID string
		Name   string
		Args   strings.Builder
		Idx    int
	}
	var fnCalls map[int]*fnCall
	var usage map[string]any

	// order 按上游实际宣告顺序登记 output 项，保证事件 output_index 与
	// completed 数组一致（模型可能先出 function_call 再出文本，或反之）。
	type outItem struct {
		isMsg    bool
		isReason bool
		fn       *fnCall
	}
	var order []*outItem
	msgItemID := "msg_0"
	msgIdx := -1
	nextIdx := 0
	seq := 0
	dead := false

	emit := func(evtType string, payload map[string]any) {
		if dead {
			return
		}
		payload["type"] = evtType
		payload["sequence_number"] = seq
		seq++
		data, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evtType, data); err != nil {
			dead = true // 客户端断开：停止读取上游，不再烧账号额度
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if ttfb == 0 {
			ttfb = time.Since(t0)
		}
	}
	respEnvelope := func(status string, output []any) map[string]any {
		env := map[string]any{
			"id":     respID,
			"object": "response",
			"status": status,
			"model":  model,
			"output": output,
		}
		if usage != nil {
			env["usage"] = usage
		}
		return env
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		emit("response.created", map[string]any{"response": respEnvelope("in_progress", nil)})
		emit("response.in_progress", map[string]any{"response": respEnvelope("in_progress", nil)})
	}

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	sawFinish := false
	sawDone := false
	for scanner.Scan() {
		if dead {
			return usage, ttfb
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content    string `json:"content"`
					Reasoning  string `json:"reasoning_content"`
					Reasoning2 string `json:"reasoning"` // OpenRouter 派字段兼容
					ToolCalls  []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if chunk.Choices[0].FinishReason != "" {
			sawFinish = true
		}
		if delta.Reasoning == "" {
			delta.Reasoning = delta.Reasoning2
		}

		// 推理内容 → reasoning_summary 事件（Codex 据此显示思考过程）
		if delta.Reasoning != "" {
			if reasoningIdx < 0 {
				reasoningIdx = nextIdx
				nextIdx++
				order = append(order, &outItem{isReason: true})
				if stream {
					emit("response.output_item.added", map[string]any{
						"output_index": reasoningIdx,
						"item": map[string]any{
							"type": "reasoning", "id": reasoningItemID, "summary": []any{},
						},
					})
					emit("response.reasoning_summary_part.added", map[string]any{
						"item_id": reasoningItemID, "output_index": reasoningIdx, "summary_index": 0,
						"part": map[string]any{"type": "summary_text", "text": ""},
					})
				}
			}
			if stream {
				emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": reasoningItemID, "output_index": reasoningIdx, "summary_index": 0,
					"delta": delta.Reasoning,
				})
			}
			reasoning.WriteString(delta.Reasoning)
		}

		if delta.Content != "" {
			if stream && msgIdx < 0 {
				msgIdx = nextIdx
				nextIdx++
				order = append(order, &outItem{isMsg: true})
				emit("response.output_item.added", map[string]any{
					"output_index": msgIdx,
					"item": map[string]any{
						"type": "message", "id": msgItemID, "role": "assistant",
						"status": "in_progress", "content": []any{},
					},
				})
				emit("response.content_part.added", map[string]any{
					"item_id": msgItemID, "output_index": msgIdx, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": ""},
				})
			}
			if msgIdx < 0 {
				// 非流式也可能未开 msg；补登记
				msgIdx = nextIdx
				nextIdx++
				order = append(order, &outItem{isMsg: true})
			}
			if stream {
				emit("response.output_text.delta", map[string]any{
					"item_id": msgItemID, "output_index": msgIdx, "content_index": 0,
					"delta": delta.Content,
				})
			}
			text.WriteString(delta.Content)
		}

		for _, tc := range delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if fnCalls == nil {
				fnCalls = make(map[int]*fnCall)
			}
			call, ok := fnCalls[idx]
			if !ok {
				call = &fnCall{ItemID: fmt.Sprintf("fc_%d", idx), Idx: -1}
				fnCalls[idx] = call
			}
			if tc.ID != "" {
				call.CallID = tc.ID
			}
			if tc.Function.Name != "" {
				call.Name = tc.Function.Name
			}
			if call.Idx < 0 {
				call.Idx = nextIdx
				nextIdx++
				order = append(order, &outItem{fn: call})
				if stream {
					callID := call.CallID
					if callID == "" {
						callID = fmt.Sprintf("call_%d", idx)
						call.CallID = callID
					}
					emit("response.output_item.added", map[string]any{
						"output_index": call.Idx,
						"item": map[string]any{
							"type": "function_call", "id": call.ItemID, "call_id": callID,
							"name": call.Name, "arguments": "", "status": "in_progress",
						},
					})
				}
			}
			if tc.Function.Arguments != "" {
				if stream {
					emit("response.function_call_arguments.delta", map[string]any{
						"item_id": call.ItemID, "output_index": call.Idx,
						"delta": tc.Function.Arguments,
					})
				}
				call.Args.WriteString(tc.Function.Arguments)
			}
		}
	}

	// 按宣告顺序组装最终 output
	fnPayload := func(call *fnCall) map[string]any {
		callID := call.CallID
		if callID == "" {
			callID = "call_" + call.ItemID
			call.CallID = callID
		}
		return map[string]any{
			"type": "function_call", "id": call.ItemID, "call_id": callID,
			"name": call.Name, "arguments": call.Args.String(), "status": "completed",
		}
	}
	msgPayload := func() map[string]any {
		return map[string]any{
			"type": "message", "id": msgItemID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text.String()}},
		}
	}
	reasoningPayload := func() map[string]any {
		return map[string]any{
			"type": "reasoning", "id": reasoningItemID,
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning.String()}},
		}
	}
	output := make([]any, 0, len(order))
	for _, it := range order {
		switch {
		case it.isReason:
			output = append(output, reasoningPayload())
		case it.isMsg:
			output = append(output, msgPayload())
		default:
			output = append(output, fnPayload(it.fn))
		}
	}

	// 截断检测：未见 [DONE] 且（传输错误或未见 finish_reason）= 上游中途死亡。
	// 不能伪装成 completed —— Codex 会把残缺思考/文本当完整回复（表现为"思考链断链"）。
	truncated := !sawDone && (scanner.Err() != nil || !sawFinish)

	if !stream {
		// 非流式：聚合为单个 response JSON；截断时报错让客户端重试
		if truncated {
			writeResponsesError(w, http.StatusBadGateway, "upstream stream ended before completion")
			return usage, 0
		}
		writeJSONRaw(w, http.StatusOK, respEnvelope("completed", output))
		return usage, 0
	}

	// 收尾事件（与 output 数组同序同位）
	for i, it := range order {
		if it.isReason {
			full := reasoning.String()
			emit("response.reasoning_summary_text.done", map[string]any{
				"item_id": reasoningItemID, "output_index": i, "summary_index": 0, "text": full,
			})
			emit("response.reasoning_summary_part.done", map[string]any{
				"item_id": reasoningItemID, "output_index": i, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": full},
			})
			emit("response.output_item.done", map[string]any{
				"output_index": i,
				"item":         output[i],
			})
			continue
		}
		if it.isMsg {
			full := text.String()
			emit("response.output_text.done", map[string]any{
				"item_id": msgItemID, "output_index": i, "content_index": 0, "text": full,
			})
			emit("response.content_part.done", map[string]any{
				"item_id": msgItemID, "output_index": i, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": full},
			})
			emit("response.output_item.done", map[string]any{
				"output_index": i,
				"item":         output[i],
			})
		} else {
			emit("response.function_call_arguments.done", map[string]any{
				"item_id": it.fn.ItemID, "output_index": i,
				"arguments": it.fn.Args.String(),
			})
			emit("response.output_item.done", map[string]any{
				"output_index": i,
				"item":         output[i],
			})
		}
	}
	if truncated {
		// 流式截断：先收尾已开的项目，再以 failed 终止（客户端可感知并重试）
		env := respEnvelope("failed", output)
		env["error"] = map[string]any{
			"code": "upstream_stream_ended", "message": "upstream stream ended before completion",
		}
		emit("response.failed", map[string]any{"response": env})
		return usage, ttfb
	}
	emit("response.completed", map[string]any{
		"response": respEnvelope("completed", output),
	})
	return usage, ttfb
}

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	chatBody, model, err := translateResponsesToChat(raw)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, err.Error())
		return
	}
	var wantStream struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(raw, &wantStream)

	// @think 后缀统一识别：本端点原生输出 reasoning 事件，剥后缀即可
	model, _ = stripThinkSuffix(model)

	// 渠道前缀路由（与 chatCompletions 一致）
	rt, bareModel, err := h.runtimeForModel(model)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, err.Error())
		return
	}
	chatBody, err = rewriteModel(chatBody, bareModel)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, err.Error())
		return
	}
	rc, uid, ok := h.dispatchChat(rt, t0, model, chatBody, w)
	if !ok {
		return // dispatchChat 已写错误响应
	}
	defer rc.Close()
	bodyFile := h.reqLogs.SaveBodyArchive(chatBody)
	chatBody = nil
	usage, ttfb := h.responsesRelay(w, rc, model, wantStream.Stream, t0)
	h.finishReqLogFile(t0, model, rt.Kind.String(), uid, http.StatusOK, wantStream.Stream, ttfb, usage, bodyFile)
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// writeJSONRaw 写任意 JSON 值。
func writeJSONRaw(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
