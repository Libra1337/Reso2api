// thinktag.go — @think 模式响应改写器。
// 把 SSE 流里的推理 delta（reasoning_content/reasoning）包装为
// <think>…</think> 正文标签，供只从正文标签提取思考的客户端
// （ZCode OpenAI 兼容模式、部分 Chat UI）显示推理过程；
// 标签在 content 文本里，可穿透不改写正文的中转站。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// stripThinkSuffix 剥离模型名的 @think 后缀，返回剥后名称与是否带后缀。
// 三种端点统一识别：chat completions 走标签包装，Anthropic/Responses 端点
// 剥后缀走原生推理（避免后缀导致模型名无法解析而错误回退/404）。
//
// 同时剥离 -vl/-vision 视觉别名后缀（visionVariant 的入口侧）：部分客户端
// （Cherry Studio / Open WebUI 等）按模型名字面启发式判断视觉能力（要含
// vl/vision/4o 等字样），deepseek-v4.1-flash 这类名字会被判为不支持视觉、
// 聊天里直接不发图（请求体无 image part，模型自然回答"没有图片"）。
// 暴露 <model>-vl 别名让客户端放行图片，网关剥后缀路由到原模型。
func stripThinkSuffix(model string) (string, bool) {
	stripped := false
	if strings.HasSuffix(model, "@think") {
		model = strings.TrimSuffix(model, "@think")
		stripped = true
	}
	for _, sfx := range []string{"-vl", "-vision"} {
		if strings.HasSuffix(model, sfx) {
			model = strings.TrimSuffix(model, sfx)
			stripped = true
		}
	}
	return model, stripped
}

// thinkTagWriter 包装 ResponseWriter，逐行改写 SSE data 帧。
type thinkTagWriter struct {
	w           http.ResponseWriter
	mu          sync.Mutex
	buf         []byte
	inThink     bool
	closedThink bool
}

func newThinkTagWriter(w http.ResponseWriter) *thinkTagWriter {
	return &thinkTagWriter{w: w}
}

func (t *thinkTagWriter) Header() http.Header { return t.w.Header() }

func (t *thinkTagWriter) WriteHeader(code int) { t.w.WriteHeader(code) }

func (t *thinkTagWriter) Flush() {
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *thinkTagWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 1<<20 {
		t.buf = t.buf[len(t.buf)-64*1024:]
	}
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := string(t.buf[:i+1])
		t.buf = t.buf[i+1:]
		if err := t.writeLine(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (t *thinkTagWriter) writeLine(line string) error {
	payload, ok := trimDataPrefix(strings.TrimRight(line, "\r\n"))
	if !ok || payload == "" || payload == "[DONE]" {
		_, err := t.w.Write([]byte(line))
		if f, ok := t.w.(http.Flusher); ok {
			f.Flush()
		}
		return err
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return t.raw(line)
	}
	if _, isErr := chunk["error"]; isErr {
		return t.raw(line)
	}
	choices, _ := chunk["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		rv, _ := delta["reasoning_content"].(string)
		if rv == "" {
			rv, _ = delta["reasoning"].(string)
		}
		delete(delta, "reasoning_content")
		delete(delta, "reasoning")
		if rv != "" {
			cv, hasCv := delta["content"].(string)
			if !t.inThink {
				t.inThink = true
				rv = "<think>" + rv
			}
			if hasCv {
				// 同帧携带推理+正文（少见形态）：正文前先闭合标签，避免正文被吞
				delta["content"] = rv + "</think>\n" + cv
				t.closedThink = true
			} else {
				delta["content"] = rv
			}
			continue
		}
		// 终帧（finish_reason）到达而标签未闭合：先补闭合帧再写终帧，
		// 否则闭合帧会落在 finish_reason 之后（客户端可能已按完成收尾）。
		if fr, _ := c["finish_reason"].(string); fr != "" && t.inThink && !t.closedThink {
			t.emitCloseThink(c["index"])
		}
		// 推理结束后的首个实质帧（正文/工具调用）前插入闭合标签
		if t.inThink && !t.closedThink {
			if _, hasContent := delta["content"]; hasContent {
				t.emitCloseThink(c["index"])
			} else if len(delta) > 0 {
				t.emitCloseThink(c["index"])
			}
		}
	}
	return t.writeChunk(chunk)
}

func (t *thinkTagWriter) emitCloseThink(index any) {
	if index == nil {
		index = 0
	}
	t.closedThink = true
	_ = t.writeChunk(map[string]any{
		"choices": []any{map[string]any{
			"index": index, "delta": map[string]any{"content": "</think>\n"},
		}},
	})
}

func (t *thinkTagWriter) writeChunk(chunk map[string]any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // <think> 标签保持字面量，不做 \u003c 转义
	if err := enc.Encode(chunk); err != nil {
		return nil
	}
	return t.raw("data: " + strings.TrimRight(b.String(), "\n") + "\n\n")
}

func (t *thinkTagWriter) raw(s string) error {
	_, err := t.w.Write([]byte(s))
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
	return err
}

// Finish 流结束时若思考标签未闭合（流只有推理即终止），补闭合。
func (t *thinkTagWriter) Finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inThink && !t.closedThink {
		t.emitCloseThink(0)
	}
}
