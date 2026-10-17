#!/usr/bin/env bash
# 管理口令「忘了怎么救」的端到端验收（容器内 `reset-password`）。
#
# 验的不是「命令打印了一句话」，而是**忘了口令之后真的能回去**：
#   1. 部署口令能登录；在管理台改成用户自己的口令后，旧口令失效
#   2. 用户「忘了」→ 容器内 reset-password --password 指定新口令 → 命令成功
#   3. **不重启**时：旧口令仍然可用、新口令仍不可用
#      （这条是故意证明「必须重启」不是文案摆设 —— 内存凭据不会因为库变了而改变）
#   4. 重启后：新口令能登录，旧口令登不进
#   5. reset-password 不带参数 → 生成随机口令，重启后能用它登录
#   6. reset-password --clear → 清掉库里的口令，重启后回到部署时环境变量里的口令
#   7. 三条命令都留下 security 审计（事后能查「谁在什么时候离线改过口令」）
#   8. 错误用法（--clear 与 --password 同时给、口令过短、未知子命令、库不存在）都必须
#      非 0 退出，且不产生副作用
#
# 用法：IMG=<镜像> [NAME=<容器名>] [DATA=<数据目录>] bash scripts/password-reset-e2e.sh
#
# ⚠️ 端口故意避开冒烟脚本的 16800/18080 与手工实例的 26800/28080：
# 端口撞车时 curl 会打到别人的实例上，表现为「口令全错」的假失败（踩过一次）。
set -u

IMG="${IMG:?必须指定 IMG（已构建的镜像）}"
NAME="${NAME:-dg-reset-e2e}"
DATA="${DATA:-/tmp/reset-e2e-data}"
ADM="http://127.0.0.1:38880"
PUB_PORT=36800
ADM_PORT=38880

# 部署时环境变量注入的口令 / 用户自己改的口令 / 忘了之后用命令救回来的口令。
INIT_PW="deploy-init-A1b2c3"
USER_PW="user-chosen-D4e5f6"
RECOVER_PW="recovered-by-cli-789"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  [OK]   %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$1"; }

check() {
  if [ "$2" = "$3" ]; then pass "$1（$3）"; else fail "$1：期望 $2，实际 $3"; fi
}

# 准备数据目录。
#
# ⚠️ 必须 chown 成 10001：bind mount 的宿主目录属主是 root，而镜像以 uid 10001 运行，
# 容器里会直接 `unable to open database file (14)`。README「改用宿主目录挂载」一节
# 写的就是这条，脚本之前漏了。
fresh_data() {
  rm -rf "$DATA"
  mkdir -p "$DATA"
  chown -R 10001:10001 "$DATA" 2>/dev/null || chmod -R 777 "$DATA"
}

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# login <口令> → 打印 HTTP 状态码（不带 cookie jar，不关心会话，只看能不能进）
login() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 8 \
    -H 'Content-Type: application/json' \
    -d "{\"password\":\"$1\"}" "$ADM/api/admin/login"
}

wait_healthy() {
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null --max-time 1 "$ADM/healthz"; then return 0; fi
    sleep 0.5
  done
  echo "容器未就绪，最近日志："
  docker logs --tail 20 "$NAME" 2>&1
  return 1
}

# 端口被**别的进程**占着时（上次残留的实例、手工跑的二进制…），
# `docker run` 会失败，但 `wait_healthy` 仍能被那个陌生服务满足，
# 于是脚本一路跑下去、全是 401/429/404 —— 看起来像「功能坏了」，实际是打错了对象。
# 踩过一次（36800 被 /mnt/bitcomet/download-gateway 占着），所以开跑前直接挡掉。
port_guard() {
  for p in "$ADM_PORT" "$PUB_PORT"; do
    if ss -lntH "sport = :$p" 2>/dev/null | grep -q .; then
      printf '端口 %s 已被占用，请先清掉残留实例：\n' "$p" >&2
      ss -lntp "sport = :$p" >&2
      exit 1
    fi
  done
}

start() {
  local out
  if ! out="$(docker run -d --name "$NAME" \
      -p "$ADM_PORT:8080" -p "$PUB_PORT:6800" \
      -v "$DATA:/data" \
      -e "DISPATCH_ADMIN_PASSWORD=$INIT_PW" \
      -e DISPATCH_ADMIN_COOKIE_SECURE=never \
      "$IMG" 2>&1)"; then
    printf '容器启动失败：%s\n' "$out" >&2
    return 1
  fi
  wait_healthy
}

restart() {
  docker restart "$NAME" >/dev/null
  wait_healthy
}

echo "=== 第 0 轮：清场并启动 ==="
cleanup
port_guard
rm -rf "$DATA"
mkdir -p "$DATA"
chown -R 10001:10001 "$DATA" 2>/dev/null || chmod -R 777 "$DATA"
start || exit 1
pass "容器已启动"

check "部署口令可登录" 200 "$(login "$INIT_PW")"

# 用会话把口令改成用户自己的（模拟「用户在管理台设了口令，然后忘了」）。
JAR="$(mktemp)"
curl -s -o /dev/null --max-time 8 -c "$JAR" -H 'Content-Type: application/json' \
  -d "{\"password\":\"$INIT_PW\"}" "$ADM/api/admin/login"
chg_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -b "$JAR" \
  -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$INIT_PW\",\"new_password\":\"$USER_PW\"}" \
  "$ADM/api/admin/password")"
rm -f "$JAR"
check "用户在管理台改了口令" 200 "$chg_code"
check "改完旧口令登不进" 401 "$(login "$INIT_PW")"
check "新口令登得进" 200 "$(login "$USER_PW")"

echo "=== 第 1 轮：忘了口令 → 容器内重置（指定新口令）==="
out="$(docker exec "$NAME" download-gateway reset-password --password "$RECOVER_PW" 2>&1)"
rc=$?
check "reset-password 退出码" 0 "$rc"
if printf '%s' "$out" | grep -q '必须重启'; then
  pass "输出明确提示「必须重启才生效」"
else
  fail "输出缺少重启提示（用户会以为命令没生效）：$out"
fi
if printf '%s' "$out" | grep -q "$RECOVER_PW"; then
  fail "指定口令被回显到输出（应只写库不回显）"
else
  pass "指定口令未被回显"
fi

# 关键：命令只改库，运行中的进程凭据在内存里 ⇒ 不重启就不生效。
check "未重启时旧口令仍可登（证明重启是必需的）" 200 "$(login "$USER_PW")"
check "未重启时新口令不可登" 401 "$(login "$RECOVER_PW")"

restart || exit 1
check "重启后救回的口令可登录" 200 "$(login "$RECOVER_PW")"
check "重启后原口令登不进" 401 "$(login "$USER_PW")"

echo "=== 第 2 轮：不带参数 → 随机口令 ==="
out="$(docker exec "$NAME" download-gateway reset-password 2>&1)"
check "随机重置退出码" 0 "$?"
RAND_PW="$(printf '%s' "$out" | grep -oE '[0-9a-f]{32}' | head -1)"
if [ -n "$RAND_PW" ]; then
  pass "输出里给出了 32 位随机口令"
else
  fail "输出里找不到随机口令：$out"
fi
restart || exit 1
if [ -n "$RAND_PW" ]; then
  check "随机口令重启后可登录" 200 "$(login "$RAND_PW")"
  check "随机口令生效后旧口令失效" 401 "$(login "$RECOVER_PW")"
fi

echo "=== 第 3 轮：--clear → 回到部署时的环境变量口令 ==="
docker exec "$NAME" download-gateway reset-password --clear >/dev/null 2>&1
check "--clear 退出码" 0 "$?"
restart || exit 1
check "清掉后回到环境变量口令" 200 "$(login "$INIT_PW")"
check "清掉后随机口令失效" 401 "$(login "${RAND_PW:-none}")"

echo "=== 第 4 轮：错误用法必须失败且无副作用 ==="
docker exec "$NAME" download-gateway reset-password --clear --password "0123456789" >/dev/null 2>&1
if [ "$?" -ne 0 ]; then pass "--clear 与 --password 互斥被拒"; else fail "--clear 与 --password 同时给竟然成功"; fi
docker exec "$NAME" download-gateway reset-password --password short >/dev/null 2>&1
if [ "$?" -ne 0 ]; then pass "过短口令被拒"; else fail "过短口令竟然通过"; fi
docker exec "$NAME" download-gateway reset-password extra-arg >/dev/null 2>&1
if [ "$?" -ne 0 ]; then pass "多余参数被拒"; else fail "多余参数竟然通过"; fi

# 未知子命令：直接非 0 退出，不能静默启动一个抢端口的服务。
docker run --rm "$IMG" restar >/dev/null 2>&1
rc=$?
if [ "$rc" -ne 0 ]; then pass "未知子命令非 0 退出（$rc）"; else fail "未知子命令竟然返回 0"; fi
if [ "$rc" -eq 2 ]; then pass "未知子命令用退出码 2（与「运行出错」区分）"; else fail "未知子命令退出码应为 2，实际 $rc"; fi

# 库不存在时：给出「先启动一次」的指引，而不是 SQLite 的 unable to open。
EMPTY_DIR="$(mktemp -d)"
# 同样要放开权限：`mktemp -d` 是 0700 root，容器里的 app 连 stat 都做不到，
# 那样测出来的就不是「库不存在」而是「没权限」（假通过）。
chown -R 10001:10001 "$EMPTY_DIR" 2>/dev/null || chmod -R 777 "$EMPTY_DIR"
out="$(docker run --rm -v "$EMPTY_DIR:/data" "$IMG" reset-password 2>&1)"
rc=$?
rmdir "$EMPTY_DIR" 2>/dev/null || true
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q '不存在'; then
  pass "库不存在时报错并指向「先启动一次」"
else
  fail "库不存在时的报错不可操作（rc=$rc）：$out"
fi

echo "=== 第 5 轮：审计留痕 ==="
# 直接读库文件（容器里没有 sqlite3，宿主机用 python 读；先停容器让 WAL 落盘）。
docker stop "$NAME" >/dev/null
count="$(python3 - "$DATA/dispatch.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
try:
    n = db.execute("SELECT COUNT(*) FROM event_log WHERE category='security'").fetchone()[0]
finally:
    db.close()
print(n)
PY
)"
if [ "${count:-0}" -ge 3 ]; then
  pass "security 审计至少 3 条（实际 $count）"
else
  fail "security 审计应 ≥3 条，实际 ${count:-查询失败}"
fi

echo "----------"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
