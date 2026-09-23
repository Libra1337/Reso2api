#!/bin/bash
# QClaw 本地 AuthGateway → 服务器反向隧道（多实例版）。
#
# 用法：scripts/qclaw-tunnel.sh [端口...]     # 默认 19000（主实例）；多实例传 19000 19001 19002…
# 服务器侧 /opt/wild-work/auths/qclaw-*.json 的 apiHost=http://127.0.0.1:<端口>/proxy/llm
#
# 隧道断线自动重连（expect 循环），服务器侧 ss -tlnp | grep 1900 可查状态。
# 注意：多实例端口规则 = 19000 + 启动序号（scripts/qclaw-multi.sh start <名> <序号>），
# 主实例固定 19000，依次递增。
set -u
SERVER=root@38.76.170.140
PASS='Undemine123'
PORTS=("$@")
[ ${#PORTS[@]} -eq 0 ] && PORTS=(19000)

R_ARGS=()
for p in "${PORTS[@]}"; do
  R_ARGS+=(-R "$p:127.0.0.1:$p")
done

while true; do
  /usr/bin/expect -c "
    set timeout -1
    spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
             -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
             -N ${R_ARGS[*]} $SERVER
    expect {
        -re {(?i)password:} { send \"$PASS\r\"; exp_continue }
        eof
    }
  "
  echo "[$(date '+%H:%M:%S')] 隧道断开（端口 ${PORTS[*]}），5s 后重连…"
  sleep 5
done
