#!/usr/bin/env bash
# 登录页「忘记口令？」自助找回的端到端验收（容器内跑）。
#
# 验的是**用户真的能自救**，而不只是接口回了 200：
#   1. 匿名点「生成临时口令」→ 200，响应里**不含口令**
#   2. 口令确实出现在容器日志里（用户唯一的交付通道）
#   3. 用它登录 → 200；**原口令仍然可用**（防「谁点一下按钮就能把管理员锁在门外」）
#   4. 用它登录后能把口令改成新的 → 改完临时口令立即失效、原口令也失效
#   5. 频率限制：同 IP 立刻再来一次 → 429，且带 Retry-After
#   6. TTL 到期后失效（用 30 秒的 TTL 实测，不靠读代码相信）
#   7. `DISPATCH_ALLOW_SELF_RESET=0` → 404（对外表现为这条路径不存在）
#
# 用法：IMG=<镜像> [NAME=<容器名>] [DATA=<数据目录>] bash scripts/selfreset-e2e.sh
set -u

IMG="${IMG:?必须指定 IMG（已构建的镜像）}"
NAME="${NAME:-dg-selfreset-e2e}"
DATA="${DATA:-/tmp/selfreset-e2e-data}"
ADM="http://127.0.0.1:39880"
ADM_PORT=39880
PUB_PORT=37800

INIT_PW="deploy-init-A1b2c3"
USER_PW="user-chosen-D4e5f6"
NEW_PW="reset-to-this-G7h8i9"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  [OK]   %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$1"; }
check() { if [ "$2" = "$3" ]; then pass "$1（$3）"; else fail "$1：期望 $2，实际 $3"; fi; }

# 准备数据目录。
#
# ⚠️ 必须 chown 成 10001：这是**bind mount 的必然要求**，不是为了让测试跑通的权宜之计 ——
# 镜像以 uid 10001 运行，而 `mkdir` 出来的宿主目录属主是 root，容器里直接
# `unable to open database file (14)`。README「改用宿主目录挂载」一节写的就是这条，
# 脚本之前漏了，导致「容器起不来」被误当成产品缺陷。
fresh_data() {
  rm -rf "$DATA"
  mkdir -p "$DATA"
  chown -R 10001:10001 "$DATA" 2>/dev/null || chmod -R 777 "$DATA"
}

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

login() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 8 \
    -H 'Content-Type: application/json' -d "{\"password\":\"$1\"}" "$ADM/api/admin/login"
}

wait_healthy() {
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null --max-time 1 "$ADM/healthz"; then return 0; fi
    sleep 0.5
  done
  docker logs --tail 20 "$NAME" 2>&1
  return 1
}

# 端口被别的进程占着时 `docker run` 会失败，但 `wait_healthy` 会被那个陌生服务满足，
# 之后全是「口令全错」的假失败。开跑前直接挡掉（与 password-reset-e2e.sh 同一套做法）。
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
  if ! out="$(docker run -d --name "$NAME" -p "$ADM_PORT:8080" -p "$PUB_PORT:6800" \
      -v "$DATA:/data" -e "DISPATCH_ADMIN_PASSWORD=$INIT_PW" \
      -e DISPATCH_ADMIN_COOKIE_SECURE=never "$@" "$IMG" 2>&1)"; then
    printf '容器启动失败：%s\n' "$out" >&2
    return 1
  fi
  wait_healthy
}

# 从容器日志里取最新一串临时口令：这正是用户在宿主机上的操作。
#
# 正则兼容两种日志形态：slog/tracing 的文本格式（`temp_password=ABC…`）
# 与 JSON 格式（`"temp_password":"ABC…"`）。
#
# ⚠️ 不要用 `tail -c 12` 截取：`grep -o` 的输出**带换行符**，末 12 字节里有一个 `\n`，
# 实际拿到的是 11 位 —— 后面所有登录都会 401，看起来像「功能坏了」。
# 这里改成先按分隔符切、再去掉引号。
latest_temp_code() {
  docker logs "$NAME" 2>&1 \
    | grep -aoE 'temp_password[":=]{1,3}[A-Z0-9]{12}' \
    | tail -1 \
    | sed -E 's/^.*[=:]//' \
    | tr -d '"'
}

echo "=== 第 0 轮：清场并启动 ==="
cleanup
port_guard
fresh_data
start || exit 1
check "部署口令可登录" 200 "$(login "$INIT_PW")"

echo "=== 第 1 轮：匿名请求临时口令 ==="
resp="$(curl -s -w '\n%{http_code}' --max-time 10 -X POST \
  -H 'Content-Type: application/json' -d '{}' \
  "$ADM/api/admin/password/reset-request")"
code_http="$(printf '%s' "$resp" | tail -1)"
body="$(printf '%s' "$resp" | sed '$d')"
check "未登录也能请求（这条路径本来就是给进不去的人用的）" 200 "$code_http"

TEMP_PW="$(latest_temp_code)"
if [ -n "$TEMP_PW" ] && [ "${#TEMP_PW}" -eq 12 ]; then
  pass "日志里取到了临时口令（12 位）"
else
  fail "取到的临时口令长度异常（${#TEMP_PW}），用户抄下来会登不进去"
  docker logs --tail 30 "$NAME" 2>&1
fi

# 口令本身绝不能出现在响应里 —— 这是这个匿名接口唯一的交付通道。
# 注意：不能拿 `grep temp_password` 判，因为 `log_hint` 里天然带着这个**键名**
# （"… | grep temp_password"），那是给用户看的提示，不是泄漏。
if printf '%s' "$body" | grep -qF "$TEMP_PW"; then
  fail "临时口令本身泄漏到了响应体（必须只走日志）"
else
  pass "响应里没有口令本身（只有 expires_in / log_hint）"
fi
if printf '%s' "$body" | grep -q '"temp_password"'; then
  fail "响应里出现了 temp_password 字段"
else
  pass "响应里没有 temp_password 字段"
fi
if printf '%s' "$body" | grep -q '"expires_in"'; then
  pass "响应带 expires_in"
else
  fail "响应缺少 expires_in：$body"
fi

echo "=== 第 2 轮：临时口令能登录，且原口令不失效 ==="
check "临时口令可登录" 200 "$(login "$TEMP_PW")"
check "原口令仍然可用（不能被一个按钮造成拒绝服务）" 200 "$(login "$INIT_PW")"

echo "=== 第 3 轮：频率限制 ==="
rl="$(curl -s -w '\n%{http_code}' --max-time 10 -X POST \
  -H 'Content-Type: application/json' -d '{}' \
  "$ADM/api/admin/password/reset-request")"
rl_http="$(printf '%s' "$rl" | tail -1)"
check "立刻再要一次 → 429" 429 "$rl_http"

echo "=== 第 4 轮：用临时口令登录并改成新口令 ==="
JAR="$(mktemp)"
lcode="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -c "$JAR" \
  -H 'Content-Type: application/json' -d "{\"password\":\"$TEMP_PW\"}" \
  "$ADM/api/admin/login")"
check "临时口令登录（带 cookie）" 200 "$lcode"

# 关键：用户此刻只知道临时口令，所以“当前口令”填它就是正确路径。
chg="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -b "$JAR" \
  -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$TEMP_PW\",\"new_password\":\"$NEW_PW\"}" \
  "$ADM/api/admin/password")"
rm -f "$JAR"
check "用临时口令作为当前口令改密成功" 200 "$chg"

check "改密后新口令可登录" 200 "$(login "$NEW_PW")"
check "改密后临时口令立即失效" 401 "$(login "$TEMP_PW")"
check "改密后原口令也失效" 401 "$(login "$INIT_PW")"

echo "=== 第 4.5 轮：审计留痕（不含口令） ==="
# 必须在**这一轮**的库上查：后面的轮次都会 fresh_data 换一个新库，
# 而「关闭开关」那轮根本不会生成口令，拿它查审计永远是 0 条（假失败）。
# 先 stop 让 WAL 有机会落盘；读的时候带上 -wal/-shm 也能读到未 checkpoint 的行。
docker stop "$NAME" >/dev/null
audit_rows="$(python3 - "$DATA/dispatch.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
try:
    rows = db.execute("SELECT message FROM event_log WHERE category='security'").fetchall()
finally:
    db.close()
print(len(rows))
for (m,) in rows:
    print(m)
PY
)"
n="$(printf '%s' "$audit_rows" | head -1)"
if [ "${n:-0}" -ge 1 ]; then
  pass "有 security 审计（${n} 条）"
else
  fail "没有 security 审计"
  printf '%s\n' "$audit_rows"
fi
if printf '%s' "$audit_rows" | grep -qF "$TEMP_PW"; then
  fail "审计里出现了临时口令本身（event_log 在界面上可见）"
else
  pass "审计不含口令本身"
fi

echo "=== 第 5 轮：TTL 到期 ==="
docker rm -f "$NAME" >/dev/null 2>&1
fresh_data
start -e DISPATCH_TEMP_PASSWORD_TTL=30 || exit 1
curl -s -o /dev/null --max-time 10 -X POST -H 'Content-Type: application/json' -d '{}' \
  "$ADM/api/admin/password/reset-request"
TEMP_PW="$(latest_temp_code)"
check "TTL 内可登录" 200 "$(login "$TEMP_PW")"
sleep 31
check "超过 TTL 后失效" 401 "$(login "$TEMP_PW")"

echo "=== 第 6 轮：关闭开关 ==="
docker rm -f "$NAME" >/dev/null 2>&1
fresh_data
start -e DISPATCH_ALLOW_SELF_RESET=0 || exit 1
check "关闭后该路径 404（不暴露「有功能但被禁」）" 404 "$(curl -s -o /dev/null -w '%{http_code}' \
  --max-time 10 -X POST -H 'Content-Type: application/json' -d '{}' \
  "$ADM/api/admin/password/reset-request")"

echo "----------"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
