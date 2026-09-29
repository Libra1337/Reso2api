// heartbeat.go — 流式请求派发期心跳：选号/等上游响应头/等首块期间保活下游连接。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// dispatchHeartbeatGrace 派发多久未完成才提前提交 200 + SSE 头开始心跳。
// 快速失败（内容审核 400、全池限流 429 等，实测 1-2.3s）在宽限期内完成时
// 仍按原状态码回给客户端，保留 monoize 类中转依赖的状态码语义。
var dispatchHeartbeatGrace = 3 * time.Second // var：测试可调

// dispatchHeartbeatInterval 提交后的心跳间隔。长上下文冷缓存首块 p90 ~23s，
// 上游拥塞时单号等响应头可达 120s × 轮转 5 次；期间零字节会被下游读超时掐断
// （nginx 499 → 用户侧 502）。
const dispatchHeartbeatInterval = 10 * time.Second

// sseHeartbeat 包装派发阶段的 ResponseWriter。
//   - 宽限期内 dispatchChat 写错误：原样透传（状态码/响应体不变）；
//   - 宽限期满仍未完成：提交 200 + SSE 头，周期写 ": keepalive" 注释行；
//     此后 dispatchChat 写的错误由 errFrame 转为端点对应的 SSE 错误帧。
//
// 自带 hdr 映射隔离 dispatchChat 的 Header().Set 与心跳协程的头写入（避免
// 并发写同一 map）。
type sseHeartbeat struct {
	w        http.ResponseWriter
	errFrame func(status int, body []byte) []byte

	mu          sync.Mutex
	hdr         http.Header
	wroteHeader bool // dispatchChat 已直接响应（宽限期内），心跳不再介入
	committed   bool // 心跳已提交 200 + SSE 头
	errStatus   int

	stopCh chan struct{}
	doneCh chan struct{}
}

func startSSEHeartbeat(w http.ResponseWriter, errFrame func(int, []byte) []byte) *sseHeartbeat {
	hb := &sseHeartbeat{
		w:        w,
		errFrame: errFrame,
		hdr:      http.Header{},
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go hb.run(dispatchHeartbeatGrace, dispatchHeartbeatInterval)
	return hb
}

func (hb *sseHeartbeat) run(grace, interval time.Duration) {
	defer close(hb.doneCh)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-hb.stopCh:
		return
	case <-t.C:
	}
	hb.mu.Lock()
	if hb.wroteHeader {
		hb.mu.Unlock()
		return
	}
	hb.committed = true
	h := hb.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	hb.w.WriteHeader(http.StatusOK)
	hb.pingLocked()
	hb.mu.Unlock()

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-hb.stopCh:
			return
		case <-tick.C:
			hb.mu.Lock()
			hb.pingLocked()
			hb.mu.Unlock()
		}
	}
}

func (hb *sseHeartbeat) pingLocked() {
	_, _ = io.WriteString(hb.w, ": keepalive\n\n")
	if fl, ok := hb.w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (hb *sseHeartbeat) Header() http.Header { return hb.hdr }

func (hb *sseHeartbeat) WriteHeader(code int) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	hb.writeHeaderLocked(code)
}

func (hb *sseHeartbeat) writeHeaderLocked(code int) {
	if hb.committed {
		hb.errStatus = code
		return
	}
	if hb.wroteHeader {
		return
	}
	hb.wroteHeader = true
	dst := hb.w.Header()
	for k, v := range hb.hdr {
		dst[k] = v
	}
	hb.w.WriteHeader(code)
}

func (hb *sseHeartbeat) Write(p []byte) (int, error) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.committed {
		status := hb.errStatus
		if status == 0 {
			status = http.StatusBadGateway
		}
		if _, err := hb.w.Write(hb.errFrame(status, p)); err != nil {
			return 0, err
		}
		if fl, ok := hb.w.(http.Flusher); ok {
			fl.Flush()
		}
		return len(p), nil
	}
	if !hb.wroteHeader {
		hb.writeHeaderLocked(http.StatusOK)
	}
	return hb.w.Write(p)
}

// Stop 结束心跳并返回供后续中继使用的 writer：已提交时屏蔽重复 WriteHeader
// （中继代码会再设 SSE 头并 WriteHeader(200)，避免 superfluous 告警）。
func (hb *sseHeartbeat) Stop() http.ResponseWriter {
	close(hb.stopCh)
	<-hb.doneCh
	if hb.committed {
		return committedWriter{hb.w}
	}
	return hb.w
}

// committedWriter 响应头已提交后的 writer：WriteHeader 为空操作。
type committedWriter struct{ http.ResponseWriter }

func (committedWriter) WriteHeader(int) {}

func (c committedWriter) Flush() {
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// parseErrorBody 从 dispatchChat 写出的错误体提取 message/code（OpenAI 形
// {"error":{...}}；解析失败时整体作为 message）。
func parseErrorBody(body []byte) (msg, code string) {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		if s, ok := e.Error.Code.(string); ok {
			code = s
		}
		return e.Error.Message, code
	}
	return string(body), ""
}

// openAIErrFrame chat/completions：错误体原样作为 data 帧（与中途断流错误帧同形）。
func openAIErrFrame(_ int, body []byte) []byte {
	out := make([]byte, 0, len(body)+32)
	out = append(out, "data: "...)
	out = append(out, body...)
	out = append(out, "\n\ndata: [DONE]\n\n"...)
	return out
}

// anthropicErrFrame /v1/messages：Anthropic 流式 error 事件。
func anthropicErrFrame(status int, body []byte) []byte {
	msg, _ := parseErrorBody(body)
	typ := "api_error"
	switch {
	case status == http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case status == http.StatusServiceUnavailable:
		typ = "overloaded_error"
	case status >= 400 && status < 500:
		typ = "invalid_request_error"
	}
	raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
	return append(append([]byte("event: error\ndata: "), raw...), "\n\n"...)
}

// responsesErrFrame /v1/responses：Responses API 流式 error 事件。
func responsesErrFrame(status int, body []byte) []byte {
	msg, code := parseErrorBody(body)
	if code == "" {
		code = http.StatusText(status)
	}
	raw, _ := json.Marshal(map[string]any{"type": "error", "code": code, "message": msg, "param": nil})
	return append(append([]byte("event: error\ndata: "), raw...), "\n\n"...)
}
