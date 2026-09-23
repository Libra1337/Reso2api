// Package qclaw 封装 QClaw（腾讯 QClaw 桌面端，OpenClaw 内核）本地
// AuthGateway 的 LLM 代理通道。
//
// 逆向结论（2026-09-23，QClaw 0.2.37 macOS）：
//   - 桌面端在 127.0.0.1:19000 起本地 AuthGateway（写于 ~/.qclaw/qclaw.json
//     的 authGatewayBaseUrl，默认 http://127.0.0.1:19000/proxy）；
//   - LLM 代理为 OpenAI 兼容：GET /proxy/llm/models、POST /proxy/llm/chat/completions，
//     鉴权由网关自注入（账号凭据在网关侧加密保管），调用方无需携带 API Key；
//   - 上游校验要求 messages 必须含 system 消息，缺失时整单
//     {"type":"invalid_request_error","message":"invalid request"}（本包
//     PrepareBody 负责兜底补入）；
//   - 模型 id：modelroute（Auto 路由，input 含 image）与 pool-* 系列；
//   - 流式 delta 含 reasoning_content 与 thinking_blocks（deepseek 风格）。
//
// 通道约束：依赖桌面端进程在线（网关在 localhost）。网关 base 可经 auth 文件
// apiHost 覆盖（远程部署可配 SSH 隧道转发地址）。
package qclaw

import "wild-work/internal/provider"

// DefaultBase 本地 AuthGateway 的 LLM 代理默认地址。
const DefaultBase = "http://127.0.0.1:19000/proxy/llm"

// staticModels 静态兜底模型表（动态 /models 不可用时）。
// 字段来自 QClaw 0.2.37 模型目录实测。
var staticModels = []provider.ModelInfo{
	{ID: "modelroute", Name: "Auto", ContextWindow: 250000, MaxTokens: 8192},
	{ID: "pool-hy3-preview", Name: "Hy3", ContextWindow: 262144},
	{ID: "pool-deepseek-v4-pro", Name: "DeepSeek-V4-Pro", ContextWindow: 1048576},
	{ID: "pool-deepseek-v4-flash", Name: "DeepSeek-V4-Flash", ContextWindow: 131072},
	{ID: "pool-glm-5.3", Name: "GLM-5.3", ContextWindow: 131072},
	{ID: "pool-kimi-k3-1", Name: "Kimi-K3.1", ContextWindow: 262144},
}

// StaticModels 返回静态模型表拷贝。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, staticModels...)
}
