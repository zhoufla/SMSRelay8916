#!/usr/bin/env bash
#
# 隐私自检：确认仓库里没有真实手机号、内网地址、凭证、绝对路径。
#
# 用法：
#   ./scripts/privacy-check.sh            # 只查已跟踪的文件
#   ./scripts/privacy-check.sh --history  # 连同全部 git 历史一起查
#
# 在 .git/hooks/pre-commit 里加一行 `./scripts/privacy-check.sh` 就能自动拦截。
#
set -uo pipefail
cd "$(dirname "$0")/.."

# 允许出现的占位值（文档示例）
ALLOW='13800000000|13900000001|192\.168\.1\.100|192\.168\.1\.101|203\.0\.113\.10|10\.64\.0\.10|127\.0\.0\.1|0\.0\.0\.0|2409:x{4}|2409:y{4}|xxxx:xxxx|REPLACE_ME|access_token=\.\.\.|access_token=xxx'

FAIL=0
report() { printf '\033[31m[隐私]\033[0m %s\n' "$*"; FAIL=1; }

scan_text() {
    # $1 = 名称, 其余从 stdin 读入 "文件:行号:内容"
    local label="$1"
    local hits
    hits=$(grep -vE "$ALLOW" | grep -vE ':[0-9]+:\s*#' | head -20)
    if [ -n "$hits" ]; then
        report "$label"
        echo "$hits" | sed 's/^/         /'
    fi
}

collect_tracked() {
    git ls-files -z | xargs -0 grep -nIP --binary-files=without-match \
        '1[3-9][0-9]{9}|(?<![\d.])(?:10|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}|2409:[0-9a-f]{4}:|2001:db8[0-9a-f:]*[0-9a-f]{4}:|access_token=[A-Za-z0-9]{16,}|/home/[a-z]+/|Bearer [A-Za-z0-9._-]{20,}' \
        2>/dev/null
}

collect_history() {
    for c in $(git rev-list --all); do
        git grep -nIP --binary-files=without-match \
            '1[3-9][0-9]{9}|(?<![\d.])(?:10|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}|2409:[0-9a-f]{4}:|access_token=[A-Za-z0-9]{16,}|/home/[a-z]+/' \
            "$c" -- . 2>/dev/null | sed "s/^/$(git log -1 --format=%h "$c") /"
    done
}

echo "=== 扫描已跟踪文件 ==="
collect_tracked | scan_text "跟踪文件中发现疑似隐私数据"

if [ "${1:-}" = "--history" ]; then
    echo "=== 扫描全部历史 ==="
    collect_history | sed 's/^[0-9a-f]\{7\} //' | scan_text "历史提交中发现疑似隐私数据"
fi

# 提交作者邮箱（公开可见）
echo "=== 提交作者 ==="
git log --all --format='  %an <%ae>' | sort -u

if [ "$FAIL" -eq 0 ]; then
    printf '\033[32m[隐私]\033[0m 未发现真实个人信息 ✓\n'
fi
exit "$FAIL"
