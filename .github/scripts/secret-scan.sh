#!/usr/bin/env bash
# 隐私脱敏闸门（CI 与本机共用同一份，避免正则漂移）。
#
# 用法：
#   bash .github/scripts/secret-scan.sh          # 扫已跟踪文件
#   bash .github/scripts/secret-scan.sh --staged # 只扫暂存区（做 pre-commit 用）
#
# 为什么要在 push 前跑：这套规则会在 `git grep` 命中时直接 exit 1，
# 而它在 CI 里是**第一个 job**，失败得极快（实测 7 秒），
# 看起来像"编译挂了"，实际是内容被拦。本地先跑一次能省一轮往返。
set -uo pipefail

# 只放**强特征**：真实 token 格式、真实私网 IP、私钥块、AWS Key、
# 以及「用户:口令」形态的弱口令。
#
# 刻意**不放**「明文提及敏感文件名」（如 xxx.token.txt）这类启发式：
# ① 文件名不是凭据，价值极低；
# ② 它是**自指**的 —— 任何解释这条规则的注释、任何列出待忽略文件的
#    .gitignore 都会命中它，导致闸门永远红灯（实测如此，见 D-8）。
# 文件级策略交给 .gitignore（第一道防线）。
#
# 注意：本脚本自身含这些正则，因此**必须**写成转义态（`192\.168\.`），
# 直接写字面 IP 会让它自己命中自己。
PAT='ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|ghs_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|dckr_pat_[A-Za-z0-9_-]{16,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|192\.168\.[0-9]+\.[0-9]+|10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+|(admin|root|user|guest):[A-Za-z0-9@._!-]{1,16}'

mode="${1:-worktree}"
if [ "$mode" = "--staged" ]; then
  hits="$(git grep --cached -nIE "$PAT" -- . )"
else
  hits="$(git grep -nIE "$PAT" -- . )"
fi

if [ -n "$hits" ]; then
  echo "$hits"
  cat <<'TIP'

::error::命中敏感信息，拒绝构建与推送

清洗方式（两条替换，通常就够）：
  1. 私网 IP     → RFC 5737 文档保留地址
                   192.0.2.0/24 (TEST-NET-1) / 198.51.100.0/24 / 203.0.113.0/24
                   实测记录里确实需要保留"哪台机器"的，用代号（NAS-A / NAS-B）。
  2. 「用户名 + 口令」形态（如 admin 与 admin 用冒号连起来）→ 换成占位符
                   <user>:<pass>，或换成不命中黑名单前缀的用户名
                   （闸门只认 admin / root / user / guest 四个词做冒号前缀）。
                   注意：**这条说明自己也不能写字面量**，否则闸门会拦它自己。
TIP
  exit 1
fi

echo "脱敏闸门通过：未发现凭据 / 内网 IP / 弱口令"
