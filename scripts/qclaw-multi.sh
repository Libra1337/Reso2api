#!/bin/bash
# QClaw 多实例启动器 —— 每实例一个账号、独立 userData、独立 state、网关端口自动递增。
#
# 用法：scripts/qclaw-multi.sh start <实例名>   # 启动/复用实例，打印其网关端口
#       scripts/qclaw-multi.sh stop <实例名>
#       scripts/qclaw-multi.sh list
#
# 原理（2026-09-23 实测 QClaw 0.2.37 macOS）：
#   - Electron 多开：--user-data-dir 隔离 userData（登录态/网关配置）；
#   - HOME 重定向隔离 ~/.qclaw state（openclaw 配置/会话）；
#   - AuthGateway 端口自动递增：首个 19000，后续 19001、19002…（实测三实例并存）；
#   - 实例网关：http://127.0.0.1:<port>/proxy/llm，OpenAI 兼容、免鉴权（登录在
#     实例内完成，扫码/账号登录一次即持久）。
#   - 每实例首启未登录时网关返回 {"code":"9001","登录已失效"}——在实例窗口里
#     登录一次即可；之后网关侧挂 auth 文件（apiHost 指向该端口）。
#
# 网关侧账号文件（/opt/wild-work/auths/qclaw-<实例名>.json）：
#   {"apiHost": "http://127.0.0.1:1900N/proxy/llm", "uid": "qclaw-<实例名>", ...}
#   服务器使用时经反向隧道映射端口（见 qclaw-tunnel.sh，支持多端口）。
set -euo pipefail

INST_ROOT="${QCLAW_INST_ROOT:-/tmp/qclaw-instances}"
APP="/Applications/QClaw.app"

port_of() { # 实例网关端口 = 19000 + 序号
  local idx=$1
  echo $((19000 + idx))
}

cmd_start() {
  local name=$1
  local idx="${2:-}"
  if [ -z "$idx" ]; then
    # 从现有实例目录推断下一个序号
    mkdir -p "$INST_ROOT"
    idx=$(ls -d "$INST_ROOT"/* 2>/dev/null | wc -l | tr -d ' ')
  fi
  local dir="$INST_ROOT/$name"
  mkdir -p "$dir"
  if pgrep -f "user-data-dir=$dir" >/dev/null; then
    echo "实例 $name 已在运行，端口 $(port_of "$idx")"
  else
    HOME="$dir/home" open -na "$APP" --args --user-data-dir="$dir/userdata" >/dev/null 2>&1
    echo "实例 $name 已启动（序号 $idx），等待网关就绪…"
  fi
  local port; port=$(port_of "$idx")
  for _ in $(seq 1 20); do
    if curl -s -m 2 "http://127.0.0.1:$port/proxy/llm/models" >/dev/null 2>&1; then
      echo "网关: http://127.0.0.1:$port/proxy/llm"
      echo "auths 文件: {\"apiHost\": \"http://127.0.0.1:$port/proxy/llm\", \"uid\": \"qclaw-$name\", \"nickname\": \"QClaw $name\"}"
      return 0
    fi
    sleep 1
  done
  echo "警告: 端口 $port 未就绪（实例可能在更高端口——用 list 查实际监听）" >&2
}

cmd_stop() {
  local name=$1
  pkill -f "user-data-dir=$INST_ROOT/$name" 2>/dev/null || true
  echo "实例 $name 已停止"
}

cmd_list() {
  echo "运行中的 QClaw 实例："
  pgrep -fl "user-data-dir=" | grep -i qclaw | while read -r pid rest; do
    local dir
    dir=$(echo "$rest" | grep -oE "user-data-dir=[^ ]+" | cut -d= -f2)
    local port
    port=$(lsof -p "${pid%% *}" -a -iTCP -sTCP:LISTEN 2>/dev/null | grep -oE 'localhost:(19[0-9]{3})' | head -1 | cut -d: -f2)
    echo "  ${dir} -> 网关端口 ${port:-未知} (pid ${pid%% *})"
  done
  echo "（网关端口规则：19000 起按启动顺序递增）"
}

case "${1:-}" in
  start) shift; cmd_start "$@" ;;
  stop) shift; cmd_stop "$@" ;;
  list) cmd_list ;;
  *) echo "用法: $0 {start <实例名> [序号] | stop <实例名> | list}" >&2; exit 1 ;;
esac
