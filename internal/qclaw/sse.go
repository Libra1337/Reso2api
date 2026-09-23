// sse.go QClaw LLM 代理的 SSE 中继与聚合。
// QClaw 流式为标准 OpenAI chunk 形态（delta 含 content / reasoning_content /
// thinking_blocks），中继无需改写；聚合把 delta 流合成完整 message。
package qclaw

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Stream 实现 provider.Upstream：SSE 透传中继。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	_, err := io.Copy(w, r)
	if fl != nil {
		fl.Flush()
	}
	return err
}

// Aggregate 实现 provider.Upstream：把上游 SSE 聚合为非流式响应对象。
func (c *Client) Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model, finish string
		content           strings.Builder
		reasoning         strings.Builder
		usage             map[string]any
	)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload == "[DONE]" {
					break
				}
				var chunk struct {
					ID      string `json:"id"`
					Model   string `json:"model"`
					Choices []struct {
						FinishReason any `json:"finish_reason"`
						Delta        struct {
							Content   string `json:"content"`
							Reasoning string `json:"reasoning_content"`
						} `json:"delta"`
					} `json:"choices"`
					Usage map[string]any `json:"usage"`
				}
				if jerr := json.Unmarshal([]byte(payload), &chunk); jerr == nil {
					if chunk.ID != "" {
						id = chunk.ID
					}
					if chunk.Model != "" {
						model = chunk.Model
					}
					if chunk.Usage != nil {
						usage = chunk.Usage
					}
					for _, ch := range chunk.Choices {
						content.WriteString(ch.Delta.Content)
						reasoning.WriteString(ch.Delta.Reasoning)
						if ch.FinishReason != nil {
							if s, ok := ch.FinishReason.(string); ok && s != "" {
								finish = s
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if finish == "" {
		finish = "stop"
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}
