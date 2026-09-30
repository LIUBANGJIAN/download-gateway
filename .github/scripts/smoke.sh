#!/usr/bin/env bash
# =============================================================================
# download-gateway · 容器冒烟验证
# =============================================================================
#
# 为什么需要这个脚本
# ------------------
# 在它出现之前，CI 只**构建**镜像、从不**运行**镜像。于是：
#   · Dockerfile 写错了 ENTRYPOINT → 没人知道；
#   · 迁移脚本没打进镜像 → 没人知道；
#   · 端口没监听 / 路由没挂上 → 没人知道。
# 流水线照样全绿，镜像推上 Docker Hub 却是坏的 —— 这就是「假绿」。
#
# 本脚本把镜子照到镜像本身：**把刚构建出的镜像真跑起来**，
# 挨个打四个面（健康检查 / aria2 面 / BitComet 面 / 管理面），
# 任何一条不符预期就让流水线红灯。
#
# 设计上的三条自律
# ----------------
# 1. **不用 `set -e`**：要跑完全部断言再一次性汇总，而不是第一条失败就退出
#    （那样看不到「到底坏了几处」）。失败数非零时脚本末尾统一 exit 1。
# 2. **不用 `|| true` 掩盖**：只在「探测是否已就绪」这种**预期会失败**的轮询里用，
#    断言一律显式比对，绝不吞错。
# 3. **打印实际状态码与响应片段**：失败时能一眼看出「哪个接口、返回了什么」，
#    而不是只给一句「断言失败」。
#
# 环境变量
# --------
#   IMAGE  必填。要验证的镜像引用（**必须是本次刚构建的那个**，不能是 latest，
#          否则验的是上一版镜像 —— 那种"绿"毫无意义）。
#
# 本脚本自己会在容器里注入两个**确定性**凭据（管理口令 / 对外公共令牌），
# 因此验的是「门禁真的开着」而不是「默认放行」：
#   · 对外口不带凭据 → 必须被拒；带正确凭据 → 必须放行（两边都验）。
#   · 管理口错误口令 → 必须 401。
# =============================================================================

set -uo pipefail

IMAGE="${IMAGE:?必须通过环境变量传入 IMAGE（要验证的镜像引用）}"

CONTAINER="gw-smoke"
PUB_PORT=16800
ADM_PORT=18080
ADMIN_PW="ci-smoke-pass-9f2c"
# ⚠️ 对外口的共享令牌**必须显式设上**，否则鉴权门禁整体处于"放行"状态，
# 下面那条「未认证必须 401」的断言就**永远不可能成立**（期望 401、实际 200 ⇒ 假红）。
# 判定表在 `ingress/envelope.rs` 的 `AuthPolicy::decide`：
#   · 未配置 public_token ⇒ 一律 Allow（无论带不带 Bearer）
#   · 配置了 public_token  ⇒ 带对才算 Allow，否则 Deny
# 设上它，才能同时验证**拒绝路径**（不带凭据必须被拒）与**放行路径**（带对凭据必须通）——
# 只验其中一边的鉴权测试是没意义的：只验拒绝，分不清"门禁在工作"还是"接口根本没实现"；
# 只验放行，则完全测不出门禁是否存在。
PUBLIC_TOKEN="ci-smoke-public-token-2f7a"
PUB="http://127.0.0.1:${PUB_PORT}"
ADM="http://127.0.0.1:${ADM_PORT}"
COOKIE_JAR="$(mktemp)"

FAILS=0
pass() { printf '  [OK]   %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAILS=$((FAILS + 1)); }

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -f "$COOKIE_JAR"
}
trap cleanup EXIT
cleanup

# --- 断言辅助：比对 HTTP 状态码，并在失败时打印响应片段 ---------------------
# 用法：expect <描述> <期望码> <实际码> <响应体>
expect() {
  local desc="$1" want="$2" got="$3" body="${4:-}"
  if [ "$got" = "$want" ]; then
    pass "$desc（HTTP $got）"
  else
    fail "$desc — 期望 HTTP $want，实际 HTTP $got；响应：$(printf '%s' "$body" | head -c 200)"
  fi
}

# 用法：expect_contains <描述> <响应体> <期望子串>
expect_contains() {
  local desc="$1" body="$2" needle="$3"
  if printf '%s' "$body" | grep -qF -- "$needle"; then
    pass "$desc（含「$needle」）"
  else
    fail "$desc — 响应中未找到「$needle」；实际：$(printf '%s' "$body" | head -c 200)"
  fi
}

# 用法：expect_absent <描述> <响应体> <不应出现的子串>
expect_absent() {
  local desc="$1" body="$2" needle="$3"
  if printf '%s' "$body" | grep -qF -- "$needle"; then
    fail "$desc — 响应中**不应**出现「$needle」；实际：$(printf '%s' "$body" | head -c 200)"
  else
    pass "$desc（未出现「$needle」）"
  fi
}

echo "==================================================================="
echo " 冒烟目标镜像：${IMAGE}"
echo "==================================================================="

echo
echo "── 1. 启动容器 ─────────────────────────────────────────────────"
echo "  对外口 → ${PUB}   管理口 → ${ADM}"
docker run -d --name "$CONTAINER" \
  -p "${PUB_PORT}:6800" \
  -p "${ADM_PORT}:8080" \
  -e DISPATCH_ADMIN_PASSWORD="$ADMIN_PW" \
  -e DISPATCH_ADMIN_COOKIE_SECURE=never \
  -e DISPATCH_PUBLIC_TOKEN="$PUBLIC_TOKEN" \
  -e RUST_LOG=info \
  "$IMAGE" >/dev/null

echo
echo "── 2. 等待就绪（最多 30 秒）────────────────────────────────────"
READY=0
for i in $(seq 1 30); do
  # 这里用 `|| true` 是**正确的**：容器还没起来时 curl 必然失败，属于预期路径。
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${ADM}/healthz" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then
    READY=1
    echo "  第 ${i} 次探测成功"
    break
  fi
  sleep 1
done
if [ "$READY" != "1" ]; then
  echo "::error title=容器未就绪::30 秒内 ${ADM}/healthz 未返回 200。以下为容器日志尾部："
  docker logs "$CONTAINER" 2>&1 | tail -60
  exit 1
fi

echo
echo "── 3. 健康检查（两个端口都要有）────────────────────────────────"
for port in "$PUB" "$ADM"; do
  body="$(curl -s --max-time 5 -w '\n%{http_code}' "${port}/healthz")"
  code="$(printf '%s' "$body" | tail -1)"
  payload="$(printf '%s' "$body" | sed '$d')"
  expect "GET ${port}/healthz" 200 "$code" "$payload"
  expect_contains "健康检查返回体含 status" "$payload" '"status"'
done

echo
echo "── 4. 对外口 · aria2 原生协议面 ────────────────────────────────"
# 4a. **拒绝路径**：不带 token 必须被拒。
# aria2 的鉴权失败按协议惯例仍是 HTTP 200，错误在 JSON-RPC 的 `error` 对象里 ——
# 所以这里要断的是**响应体里的 code=-1**，而不是 HTTP 状态码。
# （这一条是踩出来的：最初写成「期望 HTTP 401」，而 JSON-RPC 从来不这么回。）
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"smoke-0","method":"aria2.getVersion","params":[]}' "${PUB}/jsonrpc")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${PUB}/jsonrpc (aria2.getVersion, 无 token)" 200 "$code" "$payload"
expect_contains "aria2 未带 token 被拒（code=-1）" "$payload" '"code":-1'
expect_contains "aria2 未带 token 的提示" "$payload" 'Unauthorized'
expect_absent "aria2 未带 token 时不得返回 result" "$payload" '"result"'

# 4b. **放行路径**：按 aria2 官方手册，token 放在 params[0] 的 `"token:<值>"`。
ARIA_BODY="{\"jsonrpc\":\"2.0\",\"id\":\"smoke-1\",\"method\":\"aria2.getVersion\",\"params\":[\"token:${PUBLIC_TOKEN}\"]}"
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d "$ARIA_BODY" "${PUB}/jsonrpc")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${PUB}/jsonrpc (aria2.getVersion, 带正确 token)" 200 "$code" "$payload"
expect_contains "aria2 带正确 token 后返回 result" "$payload" '"result"'
expect_absent "带正确 token 后不应再出现鉴权错误" "$payload" 'Unauthorized'

# 4c. token 用错也得被拒 —— 否则 4a/4b 可能只是"恰好没走到校验"。
resp="$(curl -s --max-time 10 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"smoke-2","method":"aria2.getVersion","params":["token:wrong-token"]}' "${PUB}/jsonrpc")"
expect_contains "aria2 错误 token 被拒（code=-1）" "$resp" '"code":-1'

echo
echo "── 5. 对外口 · BitComet 原生协议面 ─────────────────────────────"
# 5a. **拒绝路径**：不带 Authorization → 必须 401 + INVALID_TOKEN。
# 断言 401 而不是 200：这**正是**在验证 BitComet 认证门禁是活的。
# 如果这里返回 200，说明鉴权被绕过了 —— 那比「接口不通」严重得多。
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{}' "${PUB}/api/config/about/get")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${PUB}/api/config/about/get（无 Bearer）" 401 "$code" "$payload"
expect_contains "BitComet 未认证错误码" "$payload" "INVALID_TOKEN"

# 5b. **放行路径**：带正确 Bearer → 200 且 error_code=OK。
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${PUBLIC_TOKEN}" -d '{}' "${PUB}/api/config/about/get")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${PUB}/api/config/about/get（带正确 Bearer）" 200 "$code" "$payload"
expect_contains "BitComet 带正确 Bearer 后放行" "$payload" '"error_code":"OK"'
expect_contains "BitComet 响应自称 proxy 平台" "$payload" '"platform":"proxy"'

# 5c. 错误 Bearer 也必须被拒。
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer definitely-wrong' -d '{}' "${PUB}/api/config/about/get")"
code="$(printf '%s' "$resp" | tail -1)"
expect "POST ${PUB}/api/config/about/get（错误 Bearer）" 401 "$code" "$(printf '%s' "$resp" | sed '$d')"

# 5d. 握手第一步不需要凭据（这是三段式握手的入口，缺了它真客户端连不进来）。
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -X POST "${PUB}/api/webui/ip_verify")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${PUB}/api/webui/ip_verify（握手第一步，无需凭据）" 200 "$code" "$payload"
expect_contains "握手第一步返回 OK" "$payload" '"error_code":"OK"'

echo
echo "── 6. 管理口 · 登录 ────────────────────────────────────────────"
resp="$(curl -s --max-time 10 -c "$COOKIE_JAR" -w '\n%{http_code}' \
  -H 'Content-Type: application/json' \
  -d "{\"password\":\"${ADMIN_PW}\"}" "${ADM}/api/admin/login")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${ADM}/api/admin/login（注入的确定性口令）" 200 "$code" "$payload"
if grep -qi 'gw_admin_sid' "$COOKIE_JAR" 2>/dev/null || grep -q 'sid' "$COOKIE_JAR" 2>/dev/null; then
  pass "登录下发了会话 Cookie"
else
  fail "登录未下发会话 Cookie；cookie jar 内容：$(head -c 200 "$COOKIE_JAR" 2>/dev/null)"
fi

# 错误口令必须被拒（证明口令校验真的在跑，而不是"配了什么都能进"）
resp="$(curl -s --max-time 10 -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{"password":"definitely-not-the-password"}' "${ADM}/api/admin/login")"
code="$(printf '%s' "$resp" | tail -1)"
expect "POST ${ADM}/api/admin/login（错误口令）" 401 "$code" "$(printf '%s' "$resp" | sed '$d')"

echo
echo "── 7. 管理口 · 页面与静态资源 ──────────────────────────────────"
# 前端三件套（index.html / app.css / app.js）是用 `include_str!` **编进二进制**的。
# 这件事的失败模式很隐蔽：文件没打进镜像，接口照样全绿，只有人打开浏览器才发现是白页。
# 所以这里逐个把它们 GET 一遍 —— 这是唯一能证明"它们真在镜像里"的办法。
for asset in / /login /tasks /nodes /settings /app.css /app.js; do
  resp="$(curl -s --max-time 10 -w '\n%{http_code}' "${ADM}${asset}")"
  code="$(printf '%s' "$resp" | tail -1)"
  expect "GET ${ADM}${asset}" 200 "$code" "$(printf '%s' "$resp" | sed '$d' | head -c 200)"
done

# 首页必须含四个导航入口 + 那条"调度未启用"横幅。
# 导航只剩三个 = 前端没更新到位（比如节点/设置页根本没做出来）。
home="$(curl -s --max-time 10 "${ADM}/")"
for view in overview nodes tasks settings; do
  expect_contains "管理台导航含 data-view=${view}" "$home" "data-view=\"${view}\""
done
expect_contains "首页横幅明说调度未启用" "$home" "仅受理任务"
# 导航里**不得**出现「新增任务」按钮（产品原则，见 README「职责边界」）。
# ⚠️ 这里刻意**不**断言「不能出现『添加任务』四个字」：任务页的说明文字里
#    恰好有一句「管理台**不提供**添加任务」—— 按字面禁词写，会把**正确的文案**判成违规。
#    （这是本地实测踩到的：断言写成禁词匹配时，它红在了自己身上。）
#    改判**按钮 id** 与**页面路径**：这两个才是"能不能提交任务"的真正载体。
expect_absent "管理台不得有「新增任务」按钮" "$home" 'id="task-new"'
expect_absent "管理台不得引用添加任务页" "$home" "/tasks/new"

echo
echo "── 8. 管理口 · 只读接口 ────────────────────────────────────────"
for path in summary nodes config; do
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -w '\n%{http_code}' "${ADM}/api/admin/${path}")"
  code="$(printf '%s' "$resp" | tail -1)"
  payload="$(printf '%s' "$resp" | sed '$d')"
  expect "GET ${ADM}/api/admin/${path}" 200 "$code" "$payload"
done

resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" "${ADM}/api/admin/nodes")"
expect_contains "节点列表返回 items 数组" "$resp" '"items"'
expect_absent "节点列表不得泄露密文" "$resp" 'pass_enc'

resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" "${ADM}/api/admin/config")"
expect_contains "设置接口返回策略表" "$resp" '"policies"'
expect_contains "设置接口返回运行环境快照" "$resp" '"env"'

echo
echo "── 9. 验收 · 管理台不得能提交任务 ──────────────────────────────"
# A-01：`POST /api/admin/tasks` 必须**已不存在**。
# axum 对「路径存在但方法不匹配」返回 405，对「路径不存在」返回 404；两者都算通过，
# 关键是**不能是 200**（那就说明"添加任务"又回来了）。
resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X POST -w '\n%{http_code}' \
  -H 'Content-Type: application/json' -d '{"url":"http://203.0.113.9/x.torrent"}' \
  "${ADM}/api/admin/tasks")"
code="$(printf '%s' "$resp" | tail -1)"
if [ "$code" = "404" ] || [ "$code" = "405" ]; then
  pass "POST ${ADM}/api/admin/tasks 已被移除（HTTP $code）"
else
  fail "POST ${ADM}/api/admin/tasks 仍可用（HTTP $code）—— 管理台不该能提交任务！"
fi

# A-02：前端「添加任务」页也必须消失。
resp="$(curl -s --max-time 10 -w '\n%{http_code}' "${ADM}/tasks/new")"
code="$(printf '%s' "$resp" | tail -1)"
expect "GET ${ADM}/tasks/new（添加任务页已删除）" 404 "$code" "$(printf '%s' "$resp" | sed '$d')"

echo
echo "── 10. 端到端 · 节点增删改 + 密码回看 ───────────────────────────"
# 用 RFC 5737 文档保留地址做基址（永远不可能是真实主机），
# 因此这一步同时验证了「探测失败路径」：节点应显示为离线且带失败原因。
NODE_JSON='{"alias":"冒烟节点","base_url":"http://203.0.113.77:9085","user":"smoke","password":"node-pw-8a1f","max_rate_kbps":512,"role":"series","tags":"ci"}'
resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -w '\n%{http_code}' \
  -H 'Content-Type: application/json' -d "$NODE_JSON" "${ADM}/api/admin/nodes")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "POST ${ADM}/api/admin/nodes（新增）" 200 "$code" "$payload"
expect_absent "新增响应不得回显密文" "$payload" 'pass_enc'
NODE_ID="$(printf '%s' "$payload" | jq -r '.data.node.node_id // empty' 2>/dev/null)"

if [ -z "$NODE_ID" ]; then
  fail "无法从新增响应中取到 node_id，后续节点用例跳过；响应：$(printf '%s' "$payload" | head -c 200)"
else
  pass "取到 node_id=${NODE_ID}"

  # 别名唯一约束 → 409
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -w '\n%{http_code}' \
    -H 'Content-Type: application/json' -d "$NODE_JSON" "${ADM}/api/admin/nodes")"
  code="$(printf '%s' "$resp" | tail -1)"
  expect "POST ${ADM}/api/admin/nodes（别名重复）" 409 "$code" "$(printf '%s' "$resp" | sed '$d')"

  # 限速换算：界面 512 KB/s → 后端 524288 字节/秒
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" "${ADM}/api/admin/nodes")"
  expect_contains "限速按 KB/s 折算存储（512 KB/s → 524288 B/s）" "$resp" '"max_rate_bytes":524288'
  expect_contains "限速回读为 KB/s" "$resp" '"max_rate_kbps":512'
  expect_contains "探测失败路径执行（离线）" "$resp" '"online":false'

  # 密码回看
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X POST -w '\n%{http_code}' \
    -H 'Content-Type: application/json' -d '{}' "${ADM}/api/admin/nodes/${NODE_ID}/secret")"
  code="$(printf '%s' "$resp" | tail -1)"
  payload="$(printf '%s' "$resp" | sed '$d')"
  expect "POST ${ADM}/api/admin/nodes/{id}/secret（密码回看）" 200 "$code" "$payload"
  expect_contains "回看的密码与写入一致" "$payload" '"password":"node-pw-8a1f"'

  # 编辑时密码留空 = 不修改（最容易写错的一条）
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X PUT -w '\n%{http_code}' \
    -H 'Content-Type: application/json' -d '{"alias":"冒烟节点改","password":""}' \
    "${ADM}/api/admin/nodes/${NODE_ID}")"
  code="$(printf '%s' "$resp" | tail -1)"
  expect "PUT ${ADM}/api/admin/nodes/{id}（改名且密码留空）" 200 "$code" "$(printf '%s' "$resp" | sed '$d')"
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X POST -H 'Content-Type: application/json' \
    -d '{}' "${ADM}/api/admin/nodes/${NODE_ID}/secret")"
  expect_contains "密码留空后原密码未被清掉" "$resp" '"password":"node-pw-8a1f"'

  # 启停开关
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X POST -w '\n%{http_code}' \
    -H 'Content-Type: application/json' -d '{"enabled":false}' \
    "${ADM}/api/admin/nodes/${NODE_ID}/enabled")"
  code="$(printf '%s' "$resp" | tail -1)"
  payload="$(printf '%s' "$resp" | sed '$d')"
  expect "POST ${ADM}/api/admin/nodes/{id}/enabled（关闭）" 200 "$code" "$payload"
  expect_contains "关闭后 enabled=false" "$payload" '"enabled":false'

  # 删除
  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X DELETE -w '\n%{http_code}' \
    "${ADM}/api/admin/nodes/${NODE_ID}")"
  code="$(printf '%s' "$resp" | tail -1)"
  expect "DELETE ${ADM}/api/admin/nodes/{id}" 200 "$code" "$(printf '%s' "$resp" | sed '$d')"

  resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" "${ADM}/api/admin/nodes")"
  expect_absent "删除后列表不再包含该节点" "$resp" '"冒烟节点改"'
fi

echo
echo "── 11. 端到端 · 策略保存与校验 ─────────────────────────────────"
resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X PUT -w '\n%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"policies":[{"key":"least_tasks","enabled":false,"priority":300}]}' \
  "${ADM}/api/admin/config")"
code="$(printf '%s' "$resp" | tail -1)"
payload="$(printf '%s' "$resp" | sed '$d')"
expect "PUT ${ADM}/api/admin/config（保存策略）" 200 "$code" "$payload"
expect_contains "保存后回读到新优先级" "$payload" '"priority":300'

# 越界优先级必须 422（不许静默夹取）
resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X PUT -w '\n%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"policies":[{"key":"least_tasks","priority":99999}]}' \
  "${ADM}/api/admin/config")"
code="$(printf '%s' "$resp" | tail -1)"
expect "PUT ${ADM}/api/admin/config（优先级越界）" 422 "$code" "$(printf '%s' "$resp" | sed '$d')"

# 未知策略键必须 422（不许静默忽略）
resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X PUT -w '\n%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"policies":[{"key":"no_such_policy","enabled":true}]}' \
  "${ADM}/api/admin/config")"
code="$(printf '%s' "$resp" | tail -1)"
expect "PUT ${ADM}/api/admin/config（未知策略键）" 422 "$code" "$(printf '%s' "$resp" | sed '$d')"

# 容量安全阀不允许关闭
resp="$(curl -s --max-time 10 -b "$COOKIE_JAR" -X PUT -w '\n%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"policies":[{"key":"max_concurrent","enabled":false}]}' \
  "${ADM}/api/admin/config")"
code="$(printf '%s' "$resp" | tail -1)"
expect "PUT ${ADM}/api/admin/config（关闭安全阀）" 422 "$code" "$(printf '%s' "$resp" | sed '$d')"

echo
echo "── 12. 容器日志（尾部，供排障）─────────────────────────────────"
docker logs "$CONTAINER" 2>&1 | tail -30

echo
echo "==================================================================="
if [ "$FAILS" -eq 0 ]; then
  echo " 冒烟全部通过"
  {
    echo "### 容器冒烟 · 全部通过"
    echo ""
    echo "镜像 \`${IMAGE}\` 已真实启动并逐面验证："
    echo ""
    echo "- 健康检查（对外口 + 管理口）"
    echo "- aria2 原生协议面：无 token 拒绝 / 错 token 拒绝 / 正确 token 放行"
    echo "- BitComet 原生协议面：无 Bearer 401 INVALID_TOKEN / 错 Bearer 401 / 正确 Bearer 放行 / 握手第一步免凭据"
    echo "- 管理口：登录（错误口令必须 401）/ 只读接口"
    echo "- 节点：增删改 + 启停开关 + 密码回看 + 限速 KB/s↔B/s 折算 + 探测离线路径"
    echo "- 策略：保存与回读 + 越界优先级 / 未知键 / 关闭安全阀 均须 422"
    echo "- **管理台不得能提交任务**（\`POST /api/admin/tasks\` 与 \`GET /tasks/new\` 都必须不存在）"
  } >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
  exit 0
else
  echo " 冒烟失败 ${FAILS} 处"
  echo "::error title=容器冒烟失败::共 ${FAILS} 条断言未通过。请查看上方 [FAIL] 行（含实际 HTTP 状态码与响应片段）。"
  exit 1
fi
