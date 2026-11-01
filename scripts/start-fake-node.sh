#!/bin/bash
# 启动/重启伪装 BitComet 节点。
#
# 为什么要有这个脚本，而不是在 ssh 命令行里直接 pkill + nohup：
#   `pkill -f fake-bitcomet-node` 会匹配**执行这条命令的 bash 自己** ——
#   它的命令行里含同一个字符串，于是把自己的父进程杀掉，后面的 nohup 根本没跑，
#   而 ssh 只回报一个空输出。用独立脚本把"匹配串"与"执行体"分离。
set -u
cd "$(dirname "$0")"

PORT="${PORT:-19085}"
STEP_MS="${STEP_MS:-2500}"
STEP_PERMILLE="${STEP_PERMILLE:-250}"
# 日志与 pid 按端口分开：允许同时跑多个节点做"选点落点"对比，
# 共用一份 pid 文件会导致起第二个时把第一个杀掉。
LOG="fakenode-$PORT.log"
PIDF="fakenode-$PORT.pid"

if [ -f "$PIDF" ]; then
  old="$(cat "$PIDF" 2>/dev/null || true)"
  if [ -n "${old:-}" ] && kill -0 "$old" 2>/dev/null; then
    kill "$old" 2>/dev/null || true
    sleep 1
    kill -9 "$old" 2>/dev/null || true
  fi
  rm -f "$PIDF"
fi

setsid nohup python3 fake-bitcomet-node.py \
  --port "$PORT" --step-ms "$STEP_MS" --step-permille "$STEP_PERMILLE" \
  > "$LOG" 2>&1 < /dev/null &
echo $! > "$PIDF"
sleep 2

echo -n "pid=$(cat "$PIDF") pid_alive="
kill -0 "$(cat "$PIDF")" 2>/dev/null && echo yes || echo no
echo -n "listen: "
ss -lntp 2>/dev/null | grep -c ":$PORT" || true
echo -n "about: "
curl -s -m 3 -o /dev/null -w '%{http_code}\n' -X POST "http://127.0.0.1:$PORT/api/config/about/get"
