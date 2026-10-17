#!/usr/bin/env bash
# =============================================================================
# 用**容器**验证 Go 版：alpine 镜像真跑起来 + 业务链路断言
# =============================================================================
#
# 前置：
#   1. 已构建镜像（`docker build -f Dockerfile-go -t dg-go:check .`）；
#   2. 已跑过 `scripts/dev-run-go.sh` 生成一次性凭据 `$ROOT/gorun/env.sh`
#      （本脚本只借用其中的 token / 口令，不借用地址与库路径 —— 容器里必须用
#        镜像自带的 `DISPATCH_DB=/data/dispatch.db`，否则会指向宿主路径而失败）。
#
# 用法：
#   HOST_PUB=36801 HOST_ADM=38881 bash scripts/docker-verify-go.sh
# =============================================================================
set -euo pipefail

ROOT="${ROOT:-/mnt/bitcomet}"
IMAGE="${IMAGE:-dg-go:check}"
HOST_PUB="${HOST_PUB:-36801}"
HOST_ADM="${HOST_ADM:-38881}"
NAME="${NAME:-dg-go-verify}"
ENVFILE="$ROOT/gorun/env.sh"
SRC="${SRC:-$ROOT/src}"

[ -f "$ENVFILE" ] || { echo "缺少凭据文件 $ENVFILE，请先跑 scripts/dev-run-go.sh" >&2; exit 1; }

# 只抽出凭据三项，避免把宿主路径（DISPATCH_DB 等）带进容器。
DOCKER_ENV="$ROOT/gorun/docker.env"
umask 077
awk -F'=' '/^export DISPATCH_(PUBLIC_TOKEN|ADMIN_PASSWORD|ADMIN_COOKIE_SECURE)=/ {
  sub(/^export /, ""); print
}' "$ENVFILE" > "$DOCKER_ENV"
# 容器里 8080 是明文 HTTP，Cookie 不能带 Secure，否则浏览器不回传（curl 不受影响，
# 但这里统一成 never，让容器验证与真实浏览器行为一致）。
if ! grep -q '^DISPATCH_ADMIN_COOKIE_SECURE=' "$DOCKER_ENV"; then
  echo 'DISPATCH_ADMIN_COOKIE_SECURE=never' >> "$DOCKER_ENV"
fi
umask 022

echo "── 启动容器（${IMAGE}）────────────────────────────────────────"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" \
  -p "${HOST_PUB}:6800" \
  -p "${HOST_ADM}:8080" \
  --env-file "$DOCKER_ENV" \
  "$IMAGE" >/dev/null

READY=0
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:${HOST_ADM}/healthz" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then READY=1; break; fi
  sleep 1
done
if [ "$READY" != "1" ]; then
  echo "容器未就绪，日志尾部：" >&2
  docker logs "$NAME" 2>&1 | tail -30 >&2
  exit 1
fi
echo "  容器就绪：对外口 ${HOST_PUB} → 6800，管理口 ${HOST_ADM} → 8080"

echo
echo "── 运行环境自证 ──────────────────────────────────────────────"
printf '  镜像大小        : %s\n' "$(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.1f MB", $1/1048576}')"
printf '  OCI 版本标签    : %s\n' "$(docker image inspect "$IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')"
printf '  运行用户        : %s\n' "$(docker image inspect "$IMAGE" --format '{{.Config.User}}')"
printf '  HEALTHCHECK     : %s\n' "$(docker image inspect "$IMAGE" --format '{{if .Config.Healthcheck}}有{{else}}无{{end}}')"
printf '  发行版          : %s\n' "$(docker exec "$NAME" cat /etc/alpine-release 2>/dev/null || echo '?')"
printf '  自报版本        : %s\n' "$(curl -s "http://127.0.0.1:${HOST_ADM}/healthz" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"

echo
echo "── 业务链路断言 ──────────────────────────────────────────────"
# shellcheck disable=SC1090
. "$ENVFILE"
cd "$SRC"
PUB="http://127.0.0.1:${HOST_PUB}" ADM="http://127.0.0.1:${HOST_ADM}" bash scripts/business-check.sh
