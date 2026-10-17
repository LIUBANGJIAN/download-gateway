#!/usr/bin/env bash
# =============================================================================
# 开发用启动器：在**构建机**上跑 Go 版，并生成一次性凭据
# =============================================================================
#
# 为什么要有这个脚本（而不是命令行里直接 `env DISPATCH_ADMIN_PASSWORD=... `）：
#   · 口令/令牌不该出现在 shell 历史、进程列表、CI 日志里；
#   · 每次起实例都换随机值，能证明"鉴权真的在比对"，而不是碰巧对上硬编码。
#
# 生成的凭据写进 `$ROOT/gorun/env.sh`（0600），验证脚本 `source` 它即可。
#
# 用法（在远端构建机上）：
#   bash scripts/dev-run-go.sh                 # 默认 127.0.0.1:36800 / 38880
#   PUBLIC_ADDR=0.0.0.0:6800 ADMIN_ADDR=0.0.0.0:8080 bash scripts/dev-run-go.sh
# =============================================================================
set -euo pipefail

ROOT="${ROOT:-/mnt/bitcomet}"
BIN="${BIN:-$ROOT/download-gateway}"
SRC="${SRC:-$ROOT/src}"
RUN="$ROOT/gorun"

PUBLIC_ADDR="${PUBLIC_ADDR:-127.0.0.1:36800}"
ADMIN_ADDR="${ADMIN_ADDR:-127.0.0.1:38880}"
VERSION="${VERSION:-0.1.3}"

[ -x "$BIN" ] || { echo "找不到可执行文件 $BIN（先 go build）" >&2; exit 1; }

TOKEN="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PASS="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"

mkdir -p "$RUN"
ENVFILE="$RUN/env.sh"
umask 077
cat > "$ENVFILE" <<EOF
# 一次性凭据（每次启动重新生成；仅用于本机验证）
export DISPATCH_DB=$RUN/dispatch.db
export DISPATCH_MIGRATIONS=$SRC/migrations
export DISPATCH_PUBLIC_ADDR=$PUBLIC_ADDR
export DISPATCH_ADMIN_ADDR=$ADMIN_ADDR
export DISPATCH_PUBLIC_TOKEN=$TOKEN
export DISPATCH_ADMIN_PASSWORD=$PASS
export DISPATCH_ADMIN_COOKIE_SECURE=never
export DISPATCH_LOG=info
export EXPECT_VERSION=$VERSION
EOF
umask 022
chmod 600 "$ENVFILE"

# 停掉上一次的实例（只匹配本目录下的这个二进制，避免误杀）。
if pgrep -f "^$BIN$" >/dev/null 2>&1; then
  pkill -f "^$BIN$" || true
  sleep 1
fi

# 用子 shell 加载环境后启动，凭据不进入本脚本的命令行。
(
  # shellcheck disable=SC1090
  . "$ENVFILE"
  nohup "$BIN" > "$RUN/app.log" 2>&1 &
)

# 等就绪（最多 20 秒）：必须真的拿到 200，不能只看 curl 退出码
# （连不上时 curl 非 0，但 4xx/5xx 也是 0 —— 只判退出码会把「起来但坏着」当成功）。
for _ in $(seq 1 20); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${ADMIN_ADDR}/healthz" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then
    echo "启动成功：对外口 ${PUBLIC_ADDR}，管理口 ${ADMIN_ADDR}"
    echo "凭据文件：$ENVFILE（source 后可跑 scripts/business-check.sh）"
    exit 0
  fi
  sleep 1
done

echo "启动失败，日志尾部：" >&2
tail -20 "$RUN/app.log" >&2
exit 1
