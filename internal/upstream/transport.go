// transport.go 聊天流出站连接：HTTP/2 健康检查 + 按账号分片多连接。
//
// 背景（2026-09-29 生产实测）：Go 对 HTTP/2 把所有并发请求复用到同一条 TCP
// 连接（到 copilot.tencent.com 仅 1 条）。该连接静默卡死时全站请求一起挂：
// 15:31-15:39 所有新请求首字节 >100s，8 分钟后同一毫秒 10 个流一起 EOF；
// 22:00-22:31 换不同账号仍连续 120s 响应头超时——换号重试救不了共享坏连接。
//
//   - ReadIdleTimeout(SendPingTimeout) + PingTimeout：连接无帧 15s 即发 ping，
//     10s 不回即关连接，挂在其上的请求立刻报错走换号重试，而非干等；
//   - 分片：每片独立 http.Transport（独立连接池），账号 uid 哈希定片——同账号
//     固定同连接（上游视角稳定），一片卡死只波及约 1/N 流量；
//   - ResponseHeaderTimeout 60s：冷缓存超长上下文首字节 p99 ~22s，60s 足够；
//     派发期有 SSE 心跳保活下游，卡死时 5 次轮转最坏 300s（原 600s）。
package upstream

import (
	"hash/fnv"
	"net/http"
	"time"
)

const (
	streamShards                = 4
	streamResponseHeaderTimeout = 60 * time.Second
	h2ReadIdleTimeout           = 15 * time.Second
	h2PingTimeout               = 10 * time.Second
)

// newStreamTransport 单片聊天流 Transport。
func newStreamTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 ProxyFunc,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       300 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: streamResponseHeaderTimeout,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: h2ReadIdleTimeout,
			PingTimeout:     h2PingTimeout,
		},
	}
}

// shardedTransport 按请求头 X-WB-Proxy-UID 哈希选片；无 uid 走 0 号片。
type shardedTransport struct {
	shards []*http.Transport
}

func newShardedTransport(n int) *shardedTransport {
	if n < 1 {
		n = 1
	}
	st := &shardedTransport{shards: make([]*http.Transport, n)}
	for i := range st.shards {
		st.shards[i] = newStreamTransport()
	}
	return st
}

func (st *shardedTransport) pick(uid string) *http.Transport {
	if uid == "" || len(st.shards) == 1 {
		return st.shards[0]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(uid))
	return st.shards[h.Sum32()%uint32(len(st.shards))]
}

func (st *shardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return st.pick(r.Header.Get("X-WB-Proxy-UID")).RoundTrip(r)
}

// CloseIdleConnections 供 http.Client.CloseIdleConnections 透传。
func (st *shardedTransport) CloseIdleConnections() {
	for _, t := range st.shards {
		t.CloseIdleConnections()
	}
}
