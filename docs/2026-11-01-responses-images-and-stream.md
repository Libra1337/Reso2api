# 2026-11-01 视觉链路：monoize 双向报错定位与 /v1/responses 修复

> 链路：客户端 → **monoize**（GPT/Claude 协议转换网关）→ **wild-work** → WorkBuddy 等上游。
> 症状：走 chat/completions 时带图 400 `Chat tool results support text only; image content cannot be discarded.`；
> 走 responses 时 400 `responses_invalid_event: response.created.response.output must be an array`。

## 1. 两个报错分属两方

### 1.1 chat/completions 的 400 —— monoize 侧，请求未到 wild-work

来自 monoize `src/urp/media.rs:536`（`prepare_tool_result_content`）：

```rust
ToolResultContent::Image { .. } => {
    if target == ProviderProtocol::ChatCompletion {
        return Err("Chat tool results support text only; image content cannot be discarded.".into());
    }
```

- 触发条件：**图片位于 tool result 内**（chat 的 `role=tool` 多模态数组 / Responses 的
  `function_call_output` 带 `input_image` / Anthropic 的 `tool_result` image 块），且 monoize
  该渠道（或 `api_type_overrides` 命中的模型）的 `provider_type == "chat_completion"`。
- monoize 把每种协议解码为 URP，再按目标协议编码；`encode/openai_chat.rs:599` 对
  ChatCompletion 目标**无条件**执行 `prepare_request`，所以即使源协议也是 chat 也会拒绝。
- chat completions 没有「工具结果带图」的载体，monoize 选择报错而非静默丢图。
  这是 monoize 的设计，不是 wild-work 的缺陷；**wild-work 收不到该请求**。

### 1.2 responses 的 400 —— wild-work 侧，`output` 不能是 null

来自 monoize `src/urp/stream_decode/openai_responses/stream_loop_part1.inc.rs`
（`validate_responses_event_shape`）：

```rust
if object.get("output").is_some_and(|value| !value.is_array()) {
    return Err(format!("{event}.response.output must be an array"));
}
```

- monoize 解码**上游** Responses 事件流时校验：`response.created` / `response.in_progress`
  的 `response.output` 必须是数组。
- wild-work `responsesRelay` 原先用 `respEnvelope("in_progress", nil)`；Go 把 nil 切片序列化成
  `"output": null` → 首个事件即被判非法，整条流 400。
- Responses 规范本身也要求 `output` 是数组，严格客户端（Codex SDK 等）同样会拒绝。

## 2. 顺带发现的第三个问题：/v1/responses 静默丢图

`translateResponsesToChat` 的 `contentToText` 只拼 `text` 字段：

- `message.content` 数组里的 `input_image` 被丢弃；
- `function_call_output.output` 数组里的 `input_image` 被丢弃。

于是即使绕过 1.1（monoize 渠道改成 `responses`），wild-work 仍会把图丢掉，模型看不到图。

## 3. 修复（internal/server/responses.go）

1. `respEnvelope`：`output == nil` 时改为空数组 `[]any{}`，`created`/`in_progress`/`failed`
   全部符合「output 必须是数组」。
2. `contentToText` → `responsesContent`：按 part 类型拆分，`input_image`/`image_url`/
   `output_image` 转成 chat `image_url` part（`image_url` 支持 Responses 的字符串形态与
   chat 的 `{"url":...}` 对象形态，`detail` 透传）。
3. 图片落位（与 `upstream/images.go` 的 `relocateAssistantImages` 同思路）：
   - `role=user` 的图：直接内联为多模态 content 数组；
   - 工具结果（`function_call_output`）与非 user 轮（assistant/system）的图：chat 的 tool
     消息只能承载文本、上游只认 user 轮的图 → 按批攒起来，在**该批工具结果之后**合成一条
     user 消息。不得插进 `assistant(tool_calls)` 与 tool 结果之间，否则工具调用配对断裂。

## 4. 回归测试

`internal/server/responses_translate_test.go`：

- `TestResponsesStreamCreatedOutputIsArray`：`response.created`/`response.in_progress` 携带
  `"output":[]`，completed 的 output 为数组；
- `TestTranslateResponsesPreservesUserImage`：user 消息的 `input_image` → chat `image_url`；
- `TestTranslateResponsesMovesToolResultImage`：`function_call_output` 的图 → tool 消息保持
  纯文本 + 紧随其后的合成 user 消息，消息序 `user,assistant,tool,user`。

全量 `go test ./...` 通过。

## 5. 运维/配置建议

- monoize 渠道指向 wild-work 时，`provider_type` 用 **`responses`**（`base_url` 指向
  `http://<wild-work>/v1`，路径 `/responses`）。`chat_completion` 无法承载工具结果图片，
  带图工具调用会被 monoize 直接拒绝。
- 仅在 chat_completion 渠道下：让工具以文本返回结果（截图/图片改为随后由 user 轮附带），
  否则 monoize 的拒绝是预期行为。
- 历史请求体排查用 `WB2A_LOG_ALL_BODIES=1`（见 `docs/2026-09-19-vision-fixes.md` §8）。
