#!/bin/bash
# QClaw 本地 AuthGateway → 服务器反向隧道
# 用法：QClaw 桌面端开着的时候跑本脚本（可放 launchd/开机自启）。
# 服务器侧 /opt/wild-work/auths/qclaw-local.json 已配置 apiHost=http://127.0.0.1:19000/proxy/llm
# 隧道断线自动重连（expect 循环），服务器侧 ss -tlnp | grep 19000 可查状态。
set -u
SERVER=root@38.76.170.140
PASS='Undemine123'
while true; do
  /usr/bin/expect -c "
    set timeout -1
    spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
             -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
             -N -R 19000:127.0.0.1:19000 $SERVER
    expect {
        -re {(?i)password:} { send \"$PASS\r\"; exp_continue }
        eof
    }
  "
  echo "[$(date '+%H:%M:%S')] 隧道断开，5s 后重连…"
  sleep 5
done
