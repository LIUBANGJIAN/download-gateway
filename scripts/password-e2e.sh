#!/usr/bin/env bash
# 管理口令「用户可自行设置」的端到端验收。
#
# 验证的不是「接口返回 200」，而是**口令真的换了**：
#   1. 环境变量注入的初始口令能登录
#   2. 在管理台改口令 → 204/200
#   3. 改完之后，**旧口令必须登不进**（否则等于没改）
#   4. 新口令能登进
#   5. 进程重启后，新口令仍然有效（证明落库了，不是只改了内存）
#   6. 重启时环境变量里仍带着旧口令，但**它不得复活**（证明库 > 环境变量）
#   7. 错误当前口令 / 过短新口令 必须被拒，且错误码正确
#
# 第 3 段是可选的**跨实现**互验：给 `CROSS_BIN` 再跑一遍同一个库，
# 证明 Rust 写下的 argon2id 哈希 Go 能验、反之亦然（PHC 串两边逐字节兼容）。
#
# 用法：BIN=<二进制路径> MIGRATIONS=<迁移目录> [CROSS_BIN=<另一份二进制>] bash scripts/password-e2e.sh
set -u

BIN="${BIN:?必须指定 BIN（编译好的 download-gateway）}"
MIGRATIONS="${MIGRATIONS:?必须指定 MIGRATIONS（migrations 目录）}"
CROSS_BIN="${CROSS_BIN:-}"
WORK="${WORK:-/tmp/pw-e2e}"

ADM="http://127.0.0.1:48880"
PUB="http://127.0.0.1:46800"

# 初始口令（模拟部署时的环境变量）与用户新设的口令。
INIT_PW="deploy-init-A1b2c3"
USER_PW="user-chosen-D4e5f6"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  [OK]   %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$1"; }

# check <描述> <期望状态码> <实际状态码>
check() {
  if [ "$2" = "$3" ]; then pass "$1（$3）"; else fail "$1：期望 $2，实际 $3"; fi
}

rm -rf "$WORK"
mkdir -p "$WORK"

PIDFILE="$WORK/pid"
LOG="$WORK/app.log"

# 当前这一轮用哪份二进制。跨实现段会把它换成 CROSS_BIN，
# 从而让「同一个库、两个不同实现」这件事在脚本里是显式的。
CUR_BIN="$BIN"

start() {
  env \
    DISPATCH_DB="$WORK/dispatch.db" \
    DISPATCH_MIGRATIONS="$MIGRATIONS" \
    DISPATCH_PUBLIC_ADDR="127.0.0.1:46800" \
    DISPATCH_ADMIN_ADDR="127.0.0.1:48880" \
    DISPATCH_ADMIN_PASSWORD="$INIT_PW" \
    DISPATCH_ADMIN_COOKIE_SECURE=never \
    DISPATCH_LOG=info \
    nohup "$CUR_BIN" >"$LOG" 2>&1 &
  echo $! >"$PIDFILE"
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null --max-time 1 "$ADM/healthz"; then return 0; fi
    sleep 0.5
  done
  echo "启动失败，日志："
  tail -20 "$LOG"
  return 1
}

stop() {
  [ -f "$PIDFILE" ] || return 0
  kill "$(cat "$PIDFILE")" 2>/dev/null
  for _ in $(seq 1 20); do
    kill -0 "$(cat "$PIDFILE")" 2>/dev/null || break
    sleep 0.2
  done
  rm -f "$PIDFILE"
}

# login <口令> <cookie jar> → 打印状态码
login() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -c "$2" -H 'Content-Type: application/json' \
    -d "{\"password\":\"$1\"}" "$ADM/api/admin/login"
}

trap 'stop' EXIT

echo "=== 第 1 轮：环境变量初始口令 ==="
start || exit 1

check "初始口令可登录" 200 "$(login "$INIT_PW" "$WORK/c1")"

status_json="$(curl -s --max-time 5 -b "$WORK/c1" "$ADM/api/admin/password")"
if printf '%s' "$status_json" | grep -q '"source":"environment"'; then
  pass "改密前来源标为 environment"
else
  fail "改密前来源应为 environment，实际：$status_json"
fi

echo "=== 改口令 ==="
chg="$(curl -s -w '\n%{http_code}' --max-time 15 -b "$WORK/c1" \
  -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$INIT_PW\",\"new_password\":\"$USER_PW\"}" \
  "$ADM/api/admin/password")"
chg_code="$(printf '%s' "$chg" | tail -1)"
check "改口令成功" 200 "$chg_code"

echo "=== 改完之后 ==="
check "旧口令必须登不进" 401 "$(login "$INIT_PW" "$WORK/c2")"
check "新口令可登录" 200 "$(login "$USER_PW" "$WORK/c3")"

status_json="$(curl -s --max-time 5 -b "$WORK/c3" "$ADM/api/admin/password")"
if printf '%s' "$status_json" | grep -q '"source":"database"'; then
  pass "改密后来源标为 database"
else
  fail "改密后来源应为 database，实际：$status_json"
fi

echo "=== 错误路径 ==="
check "当前口令错误应拒绝" 401 "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
  -b "$WORK/c3" -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"wrong-pass-xyz\",\"new_password\":\"another-pass-123\"}" \
  "$ADM/api/admin/password")"
check "新口令过短应 422" 422 "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
  -b "$WORK/c3" -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$USER_PW\",\"new_password\":\"short\"}" \
  "$ADM/api/admin/password")"
check "未登录改口令应 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
  -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$USER_PW\",\"new_password\":\"another-pass-123\"}" \
  "$ADM/api/admin/password")"

echo "=== 第 2 轮：重启（环境变量里仍是旧口令）==="
stop
start || exit 1

check "重启后旧口令仍然登不进（库 > 环境变量）" 401 "$(login "$INIT_PW" "$WORK/c4")"
check "重启后新口令仍有效（证明已落库）" 200 "$(login "$USER_PW" "$WORK/c5")"

# 口令哈希绝不能以明文形式落库。
#
# ⚠️ 必须连 `-wal` / `-shm` 一起查：SQLite 默认 WAL 模式，最近的写入先落在
# `dispatch.db-wal`，进程刚退出时主库文件里可能还看不到。只 grep 主库文件会
# 得到「找不到哈希」的假失败（本脚本第一版就栽在这里）。
DB_FILES=("$WORK"/dispatch.db*)
if grep -aq "$USER_PW" "${DB_FILES[@]}"; then
  fail "数据库里出现了新口令明文"
else
  pass "数据库里没有新口令明文（已哈希）"
fi
if grep -aq 'argon2id' "${DB_FILES[@]}"; then
  pass "数据库里存的是 argon2id 哈希"
else
  fail "数据库（含 WAL）里找不到 argon2id 哈希"
fi

stop

if [ -n "$CROSS_BIN" ]; then
  echo "=== 第 3 轮：跨实现（$CROSS_BIN 读同一个库）==="
  CUR_BIN="$CROSS_BIN"
  start || exit 1
  check "跨实现：新口令仍可登录" 200 "$(login "$USER_PW" "$WORK/c6")"
  check "跨实现：初始口令仍不可登录" 401 "$(login "$INIT_PW" "$WORK/c7")"
  status_json="$(curl -s --max-time 5 -b "$WORK/c6" "$ADM/api/admin/password")"
  if printf '%s' "$status_json" | grep -q '"source":"database"'; then
    pass "跨实现：来源仍标为 database"
  else
    fail "跨实现：来源应为 database，实际：$status_json"
  fi
  stop
else
  echo "=== 第 3 轮：跨实现（跳过，未提供 CROSS_BIN）==="
fi

echo "----------"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
