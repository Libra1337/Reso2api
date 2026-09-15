# 一次性积分任务脚本

移植自 linguo2625469/workbuddy2api-panel 的成长任务系列，按 Reso 目录约定适配
（auths 目录：`WB2A_AUTHS` 环境变量 > 仓库根 `auths/`）。

全部默认 **dry-run**：写动作（accept / claim / report / chat）由 `--yes` 显式放行。

| 脚本 | 任务 | 说明 |
|---|---|---|
| `probe_active.py` | — | 账号活性探测（只读，任务进度总览） |
| `task_chat5.py` | chat_5（+100 积分 +5 能量） | 上报 chat_request_send 事件补齐到 5 次 |
| `task_first_buddy.py` | first_buddy | 猫猫旅行首次领养（accept 协议 + first） |
| `task_model_chat.py` | model_chat（GLM-5.2） | 真实对话一次（走 /v2/chat/completions 流式） |
| `task_richmeow.py` | richmeow | 富翁猫任务上报 |

共享库：`task_common.py`（端点常量、凭证加载、growth/report 域请求封装）。

## 用法

```bash
# 进度查看（只读，无需 --yes）
python3 scripts/tasks/probe_active.py <uid前缀>

# 补齐 chat_5 到 5 次（默认 dry-run）
python3 scripts/tasks/task_chat5.py <uid前缀>
python3 scripts/tasks/task_chat5.py <uid前缀> --yes
```

端点权威来源：Reso 的 Go 代码（`internal/upstream/report.go` / `travel.go`）
与 panel 仓库实测对齐：
- chat 域（copilot.tencent.com）：growth / tasks / buddy / streak / chat
- billing 域（www.codebuddy.cn）：/v2/report

注意：这些脚本模拟客户端事件刷任务，上游风控收紧时批量号有连坐风险；
逐号小量使用，频率对齐真实客户端（脚本内置 ≥1.05s 间隔）。
