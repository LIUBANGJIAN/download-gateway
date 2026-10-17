#!/usr/bin/env bash
# =============================================================================
# download-gateway · Go 版业务链路验证（非 CI 门，手动/远端用）
# =============================================================================
#
# 与 `.github/scripts/smoke.sh` 的分工：
#   · smoke.sh 验的是**已上线 Rust 镜像**的 68 条契约（含节点 CRUD、策略校验）；
#   · 本脚本验的是 **Go 版接线后的业务链路**：任务能不能真的从 :6800 进来、
#     在 :8080 看到、能被改状态/删除，以及两条承重边界（鉴权门禁、管理台不得建任务）。
#
# 用法：
#   PUB=http://127.0.0.1:26800 ADM=http://127.0.0.1:28880 \
#   PUBLIC_TOKEN=... ADMIN_PASSWORD=... bash scripts/business-check.sh
#
# 说明：刻意不用 `set -e` —— 要跑完全部断言再一次性汇总，否则看不到「坏了几处」。
# =============================================================================
set -uo pipefail

PUB="${PUB:?必须传入对外口基址，如 http://127.0.0.1:6800}"
ADM="${ADM:?必须传入管理口基址，如 http://127.0.0.1:8080}"
# 允许直接复用 `dev-run-go.sh` 生成的 env.sh（里面是 DISPATCH_* 原名）。
PUBLIC_TOKEN="${PUBLIC_TOKEN:-${DISPATCH_PUBLIC_TOKEN:-}}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${DISPATCH_ADMIN_PASSWORD:-}}"
: "${PUBLIC_TOKEN:?必须传入对外口令牌（否则无法验证拒绝路径）}"
: "${ADMIN_PASSWORD:?必须传入管理口令}"
EXPECT_VERSION="${EXPECT_VERSION:-}"

JAR="$(mktemp)"
FAILS=0
CHECKS=0

pass() { printf '  [OK]   %s\n' "$1"; CHECKS=$((CHECKS + 1)); }
fail() { printf '  [FAIL] %s\n' "$1"; CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); }

cleanup() { rm -f "$JAR"; }
trap cleanup EXIT

# expect_code <描述> <期望码> <实际码> <响应体>
expect_code() {
  local desc="$1" want="$2" got="$3" body="${4:-}"
  if [ "$got" = "$want" ]; then
    pass "$desc（HTTP $got）"
  else
    fail "$desc — 期望 HTTP $want，实际 $got；响应：$(printf '%s' "$body" | head -c 200)"
  fi
}

# expect_in <描述> <响应体> <子串>
expect_in() {
  local desc="$1" body="$2" needle="$3"
  if printf '%s' "$body" | grep -qF -- "$needle"; then
    pass "$desc（含「$needle」）"
  else
    fail "$desc — 未找到「$needle」；实际：$(printf '%s' "$body" | head -c 200)"
  fi
}

# expect_not_in <描述> <响应体> <子串>
expect_not_in() {
  local desc="$1" body="$2" needle="$3"
  if printf '%s' "$body" | grep -qF -- "$needle"; then
    fail "$desc — 不应出现「$needle」；实际：$(printf '%s' "$body" | head -c 200)"
  else
    pass "$desc（未出现「$needle」）"
  fi
}

echo "==================================================================="
echo " Go 版业务链路验证"
echo "   对外口 ${PUB}   管理口 ${ADM}"
echo "==================================================================="

echo
echo "── 1. 就绪 ─────────────────────────────────────────────────────"
READY=0
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${ADM}/healthz" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then READY=1; break; fi
  sleep 1
done
if [ "$READY" != "1" ]; then
  fail "30 秒内 ${ADM}/healthz 未返回 200"
  echo "  断言总数 / 失败数: ${CHECKS} / ${FAILS}"
  exit 1
fi
pass "管理口就绪"

body="$(curl -s --max-time 5 "${ADM}/healthz")"
expect_in "健康检查含 status" "$body" '"status":"ok"'
expect_in "健康检查含 db" "$body" '"db":true'
if [ -n "$EXPECT_VERSION" ]; then
  expect_in "自报版本 = ${EXPECT_VERSION}" "$body" "\"version\":\"${EXPECT_VERSION}\""
fi

echo
echo "── 2. 对外口鉴权门禁（拒绝 + 放行两边都验）────────────────────"
body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"a","method":"aria2.getVersion","params":[]}' "${PUB}/jsonrpc")"
expect_in "aria2 无 token 被拒（code=-1）" "$body" '"code":-1'
expect_not_in "aria2 被拒时无 result" "$body" '"result"'

body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":\"b\",\"method\":\"aria2.getVersion\",\"params\":[\"token:${PUBLIC_TOKEN}\"]}" "${PUB}/jsonrpc")"
expect_in "aria2 带正确 token 放行" "$body" '"result"'

code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H 'Content-Type: application/json' \
  -d '{}' "${PUB}/api/config/about/get")"
expect_code "BitComet 无 Bearer 401" 401 "$code"

code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${PUBLIC_TOKEN}" -d '{}' "${PUB}/api/config/about/get")"
expect_code "BitComet 带正确 Bearer 200" 200 "$code"

echo
echo "── 3. 业务链：入库 → 可查 → 可改 → 可删（aria2 面）──────────────"
body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":\"3\",\"method\":\"aria2.addUri\",\"params\":[\"token:${PUBLIC_TOKEN}\",[\"http://example.test/verify-go.iso\"],{\"out\":\"verify-go.iso\"}]}" \
  "${PUB}/jsonrpc")"
GID="$(printf '%s' "$body" | sed -n 's/.*"result":"\([0-9a-f]\{16\}\)".*/\1/p')"
if [ -n "$GID" ]; then
  pass "addUri 入库成功，GID=${GID}"
else
  fail "addUri 未返回 16 位 GID；响应：$(printf '%s' "$body" | head -c 200)"
fi

if [ -n "${GID:-}" ]; then
  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"4\",\"method\":\"aria2.tellStatus\",\"params\":[\"token:${PUBLIC_TOKEN}\",\"${GID}\"]}" \
    "${PUB}/jsonrpc")"
  expect_in "tellStatus 查得到同一条" "$body" "\"gid\":\"${GID}\""
  expect_in "tellStatus 状态为 waiting" "$body" '"status":"waiting"'
  # 两处诚实字段：代理不知道节点落盘路径 / 未派发。
  expect_in "dir 为空串" "$body" '"dir":""'
  expect_in "dirKnown=false" "$body" '"dirKnown":false'
  expect_in "dispatched=false" "$body" '"dispatched":false'

  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"5\",\"method\":\"aria2.pause\",\"params\":[\"token:${PUBLIC_TOKEN}\",\"${GID}\"]}" \
    "${PUB}/jsonrpc")"
  expect_in "pause 返回 GID" "$body" "\"result\":\"${GID}\""

  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"6\",\"method\":\"aria2.tellStatus\",\"params\":[\"token:${PUBLIC_TOKEN}\",\"${GID}\"]}" \
    "${PUB}/jsonrpc")"
  expect_in "pause 后状态变为 paused" "$body" '"status":"paused"'

  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"7\",\"method\":\"aria2.remove\",\"params\":[\"token:${PUBLIC_TOKEN}\",\"${GID}\"]}" \
    "${PUB}/jsonrpc")"
  expect_in "remove 返回 GID（软删）" "$body" "\"result\":\"${GID}\""

  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"8\",\"method\":\"aria2.tellStopped\",\"params\":[\"token:${PUBLIC_TOKEN}\",0,10]}" \
    "${PUB}/jsonrpc")"
  expect_in "软删后仍能被 tellStopped 查到" "$body" "\"gid\":\"${GID}\""

  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"9\",\"method\":\"aria2.removeDownloadResult\",\"params\":[\"token:${PUBLIC_TOKEN}\",\"${GID}\"]}" \
    "${PUB}/jsonrpc")"
  expect_in "removeDownloadResult 返回 OK（硬删）" "$body" '"result":"OK"'

  body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"10\",\"method\":\"aria2.tellStopped\",\"params\":[\"token:${PUBLIC_TOKEN}\",0,10]}" \
    "${PUB}/jsonrpc")"
  expect_not_in "硬删后 tellStopped 不再含该 GID" "$body" "\"gid\":\"${GID}\""
fi

echo
echo "── 4. 业务链：BitComet 面入库 → 列表可见 ───────────────────────"
body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${PUBLIC_TOKEN}" \
  -d '{"url":"http://example.test/bitcomet-verify.iso","filename":"bitcomet-verify.iso"}' \
  "${PUB}/api/task/http/add")"
expect_in "http/add 返回 proxy_task_id" "$body" '"proxy_task_id"'

body="$(curl -s --max-time 5 -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${PUBLIC_TOKEN}" -d '{}' "${PUB}/api_v2/task_list/get")"
expect_in "task_list/get 能看到刚提交的任务" "$body" 'bitcomet-verify.iso'
expect_in "task_list/get 带 filtered_task_count" "$body" '"filtered_task_count"'

echo
echo "── 5. 管理口：登录 / 会话 / 只读 ───────────────────────────────"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H 'Content-Type: application/json' \
  -d '{"password":"definitely-wrong"}' "${ADM}/api/admin/login")"
expect_code "错误口令 401" 401 "$code"

code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -c "$JAR" -H 'Content-Type: application/json' \
  -d "{\"password\":\"${ADMIN_PASSWORD}\"}" "${ADM}/api/admin/login")"
expect_code "正确口令 200" 200 "$code"

# Cookie 名必须**恰好**是 sid（按 Netscape jar 的第 6 列比对，避免"sid"子串误判）。
if awk -F'\t' '$6=="sid"' "$JAR" 2>/dev/null | grep -q .; then
  pass "登录下发了会话 Cookie（名称恰为 sid）"
else
  fail "登录未下发 sid Cookie；jar：$(head -c 200 "$JAR" 2>/dev/null)"
fi

body="$(curl -s --max-time 5 -b "$JAR" "${ADM}/api/admin/tasks")"
expect_in "管理台任务列表含刚提交的任务" "$body" 'bitcomet-verify.iso'
expect_in "任务列表带 total" "$body" '"total"'

body="$(curl -s --max-time 5 -b "$JAR" "${ADM}/api/admin/summary")"
expect_in "总览返回 task_counts" "$body" '"task_counts"'

body="$(curl -s --max-time 5 -b "$JAR" "${ADM}/api/admin/config")"
expect_in "设置返回策略表" "$body" '"policies"'
expect_in "设置返回环境快照" "$body" '"env"'

echo
echo "── 6. 承重边界：管理台不得产生任务 ─────────────────────────────"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -b "$JAR" -X POST \
  -H 'Content-Type: application/json' -d '{"url":"http://203.0.113.9/x.torrent"}' \
  "${ADM}/api/admin/tasks")"
if [ "$code" = "405" ] || [ "$code" = "404" ]; then
  pass "POST /api/admin/tasks 不存在（HTTP $code）"
else
  fail "POST /api/admin/tasks 仍可用（HTTP $code）—— 管理台不该能提交任务"
fi

code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${ADM}/tasks/new")"
expect_code "GET /tasks/new 已删除" 404 "$code"

echo
echo "── 7. 策略保存与校验 ──────────────────────────────────────────"
body="$(curl -s --max-time 5 -b "$JAR" -X PUT -H 'Content-Type: application/json' \
  -d '{"policies":[{"key":"least_tasks","enabled":false,"priority":300}]}' \
  "${ADM}/api/admin/config")"
expect_in "保存策略后回读到 priority=300" "$body" '"priority":300'

for spec in \
  '优先级越界|{"policies":[{"key":"least_tasks","priority":99999}]}' \
  '未知策略键|{"policies":[{"key":"no_such_policy","enabled":true}]}' \
  '关闭安全阀|{"policies":[{"key":"max_concurrent","enabled":false}]}'
do
  name="${spec%%|*}"
  payload="${spec#*|}"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -b "$JAR" -X PUT \
    -H 'Content-Type: application/json' -d "$payload" "${ADM}/api/admin/config")"
  expect_code "${name} 必须 422" 422 "$code"
done

echo
echo "==================================================================="
echo "  断言总数 / 失败数 : ${CHECKS} / ${FAILS}"
echo "==================================================================="
if [ "$FAILS" -eq 0 ]; then
  echo " 业务链路验证通过：${CHECKS} 条断言"
  exit 0
fi
echo " 业务链路验证失败：${FAILS} 处（共 ${CHECKS} 条断言）"
exit 1
