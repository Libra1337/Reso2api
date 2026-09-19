# 2026-09-19 视觉链路修复实录（WorkBuddy 渠道）

> 本文记录一天内围绕「视觉模型（deepseek-v4.1-flash / glm-5v-turbo 等）反代后无法使用视觉功能」的完整排查与七项修复，以及过程中发现的账号导入、调试开关等配套变更。所有修复已在生产网关（38.76.170.140:/opt/wild-work，Docker 部署）验证后部署。
>
> 对应服务器端提交（granular）：`045426d` `e83b67f` `c9dc8a5` `3890c97` `4a71102` `5fa07de` `43b3af1`；本文档与代码修复整体同步提交至本仓库。

## 0. 配套变更：账号批量导入

- 微信卡密导出（70 个 `accessToken----refreshToken` 对，copilot.tencent.com 签发，app_type=codebuddy）→ 解析 JWT（sub→uid、preferred_username→nickname、exp→expiresAt）→ 生成 `auths/workbuddy-<uid>.json`（嵌套形，domain=`www.codebuddy.cn`，region=cn）。
- 与服务器原有 170 个账号无 uid 重复，导入后 `loaded accounts: workbuddy=240 cn`。
- 要点：**只新增不覆盖**——若 uid 已存在且服务器侧已刷过 token，卡密里的旧 refresh token 可能已因轮换失效，覆盖会打死账号。

## 1. 裸 base64 图片补全 data URL 前缀（`045426d`）

- **现象**：部分客户端发图后模型回答「没有图片」，回放请求体发现 `image_url.url` 是**裸 base64**（无 `data:image/xxx;base64,` 前缀）。
- **根因**：上游 `/v2/chat/completions` 的模型提供方对裸 base64 直接 `400 code=11133 "rejected by the model provider"`，网关原样透传。
- **修复**：`internal/upstream/images.go` 新增 `normalizeImageURLs`——出站前按 base64 魔数嗅探（PNG/JPEG/GIF/WebP）补全前缀；已带 scheme（`data:`/`http:`）、过短（<64）、未命中魔数的值一律不动，避免误伤 URL 引用。
- **验证**：原样回放失败请求 → 200 且模型正确描述图片（红色矩形 "READ OK"）。

## 2. Anthropic 入口保留图片（`045426d`）

- **现象**：`/v1/messages`（ZCode / Claude Code 类客户端）视觉全灭。
- **根因**：`anthropic.go` 把 image 块替换成 `[image omitted]`（注释写「保留」，实现是丢弃）。
- **修复**：`anthropicImagePart` 把 Anthropic image 块转成 OpenAI `image_url` part（base64 拼 data URL、url 直传），带图时 user 消息 content 用多模态数组，纯文本形态不变。

## 3. assistant 轮图片搬迁（`e83b67f`）

- **现象**：同一张图挂在 user 轮模型可见、挂在 assistant 轮模型回答 "NO IMAGE"（客户端自带视觉测试即此形态，正文里写着 "(image previously attached to the assistant message)"）。
- **根因**：上游**静默丢弃 assistant 角色消息里的 image_url part**（不报错）。
- **修复**：`relocateAssistantImages` 把 assistant 消息 content 数组里的 image part 挪到紧随其后合成的 user 消息（保序；纯图 assistant 消息搬迁后 content 置空串）。实测合成纯图 user 消息上游正常识图。

## 4. thinking 吃光小 max_tokens 预算（`c9dc8a5`）

- **现象**：客户端视觉测试（max_tokens=300）无限重试，网关日志全部 200 但 `out_tok` 恰好停在 300、`finish=length`、**content 空串**。
- **根因**：网关对 deepseek 强制注入 `thinking:{type:enabled}` + `reasoning_effort:high`，思考烧穿全部输出预算。`budget_tokens` 实测上游**不保证尊重**（10 连发 3 次 think=300 照样烧穿）。
- **修复**（两层，对齐 cline2api-workers 对同症的处理——Cline 免费 deepseek 通道 200 但 content 全空，其方案是剥 max_tokens + 空内容换号重试）：
  - `max_tokens < 1024` 且开思考 → **直接剥离 max_tokens**（思考自然结束正文必有；测试类请求实际总输出 100-400 token）；
  - `1024 ≤ max_tokens ≤ 4096` → 注入 `thinking.budget_tokens = max(64, mt/3)`，正文留 ≥2/3 余量（客户端自带 budget 不覆盖）；
  - 大预算 / 非 deepseek 零改动。
- **验证**：原始失败请求 max_tokens=300 连发 12 次，12/12 正文非空；流式同样正常。

## 5. `-vl` 视觉别名（`3890c97`）

- **现象**：部分客户端按模型名字面启发式判定视觉能力，名字不含 vl/vision/4o 字样 → 聊天里不发图。
- **修复**：`/v1/models` 对视觉可用但名字无视觉字样的模型追加 `<model>-vl` 别名（如 `workbuddy/deepseek-v4.1-flash-vl`）；`stripThinkSuffix` 统一剥离 `-vl`/`-vision` 后缀（@think 之后），三个端点（chat/anthropic/responses）共用。

## 6. 带图不注入思考 + SSE 首字节心跳（`4a71102`）

- **现象**：Caddy journal 记录客户端（UA `monoize/0.1`，即 Kelivo 自定义 UA）2 小时**主动掐断 716 个请求**（含 3.2MB 带图请求），掐断时长两档：~1s（首字节超时）与 8~46s（等正文超时）。被掐请求回放全部正确——回答在送达前被客户端放弃。
- **根因**：deepseek 思考期 5-20s（视觉编码+推理），正文迟迟不出，超出下游等待窗口。
- **修复**：
  - `requestHasImage` 判定带图请求**不再注入 thinking**（客户端显式开启照常尊重）——正文首字从 5-20s 压到 ~2-4s；
  - chat / anthropic 流式分支在转发上游前立刻下发 `: keepalive` 注释行并冲刷（合法 SSE，解析器忽略），首字节提前到 dispatch 完成即达。
- **验证**：带图流式首字节 0.63s、首个正文 token 0.63s；部署后掐断归零（17:27-17:48 窗口 0 次）。

## 7. `deepseek-flash` 官方名别名（`43b3af1`）——最终真因

- **现象**：客户端（Kelivo）发图聊天仍失败；全量抓包显示失败请求 messages **只有纯文本**，图在客户端就被丢弃。
- **根因**（读 Kelivo 源码 `model_provider.dart` `_isDeepSeekVisionModel`）：

  ```dart
  /// DeepSeek V4.1 Flash (`deepseek-flash`) is native multimodal.
  /// `deepseek-v4-pro` stays text-only until that SKU is retired.
  RegExp(r'(^|[/_:@])(?:deepseek-flash|deepseek-v4-flash)(?:$|[/_:@.-])')
  ```

  Kelivo 只认官方名 **`deepseek-flash`**（和退役别名 `deepseek-v4-flash`）为视觉模型；上游动态列表暴露的是 `deepseek-v4.1-flash`，名字不匹配 → `canImageInput=false` → **Kelivo 发请求前就把图片丢掉**。`-vl` 后缀也不匹配它的正则。
- **修复**：`modelAliases` 把 `deepseek-flash` 归一路由到 `deepseek-v4.1-flash`（带/不带渠道前缀均生效），`/v1/models` 同时暴露 `workbuddy/deepseek-flash`。
- **验证**：完全模拟 Kelivo 请求形态（官方名 + thinking disabled + temperature 0.6 + 流式 + base64 图）→ 0.61s 出正文，正确识图。改名后用户请求体从 2KB 变 130-400KB（图真的进来了）。
- **附**：`glm-5.3-flash` 也在 Kelivo 视觉名单（`_isGlmVisionModel`），网关实测识图正常，可直接使用。

## 8. 调试开关：`WB2A_LOG_ALL_BODIES`（`5fa07de`）

- 默认请求体存档 10% 采样 + 256KiB 截断，带图大请求基本采不到，排查屡次受阻。
- 环境变量非空时 100% 存档、单条上限 8MiB（对齐入口 chatBodyLimit）。
- **运维提醒**：排查结束后从 `docker-compose.yml` 删掉 `WB2A_LOG_ALL_BODIES=1` 再 `up -d` 恢复采样，省磁盘。

## 排查方法论备忘

1. **三层日志各看各的**：网关 request_logs（模型/token/时延）+ reqlog_bodies（请求体原文，注意采样与截断）+ 反代层（Caddy journal 的 abort 记录带 UA/IP/Content-Length/时长——本次定位掐流的唯一入口）。
2. **回放是金标准**：所有「客户端说失败」的请求，取原文回放，能立刻区分网关/上游问题与客户端问题。
3. **改一个变量测一轮**：part 顺序 / system 有无 / detail 字段 / 会话预热 / cache key 均单独排除（18/18 通过），最终靠「请求体里根本没有图」+ 客户端源码正则定位。
4. **客户端生态按名字判能力**：Kelivo（vision 正则表）、Cherry Studio / Open WebUI（vl/vision/4o 启发式）。上游模型名与生态知名名不一致时，网关侧暴露别名是最稳的兼容手段。

## 遗留观察（未定案）

- 17:48-17:49 窗口（`-vl`/官方名改名生效后）观察到 Kelivo 流式请求再次被掐，**掐断时刻与网关首字节（心跳）时刻吻合**（5 组样本 0.64s≈609ms / 1.51s≈1513ms / 5.36s≈5369ms）。Kelivo 的 SSE 解析器（`sse_framing.dart`）按规范处理注释行，理论上不应被 `: keepalive` 触发中断；不排除其上游层（HTTP 客户端/重试器）对首帧有额外假设。若后续确认心跳与某客户端不兼容，可将心跳改为 `event: ping` 形态（Anthropic/OpenRouter 生态惯例）或加环境变量开关。
- 服务器 `/opt/wild-work` 仓库 master 领先 origin 数个提交（含代理池等本地提交）且与 origin 有内容重复的 cherry-pick 对（f3dced3/c957efa vs 1a13802/3252254），合并时注意。
