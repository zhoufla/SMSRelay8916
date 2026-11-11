#!/usr/bin/env bash
# OpenWrt 主线部署统一使用 SDK/构建树工具链，Debian 部署见 debian 分支。
set -euo pipefail
export SMSRELAY_SSH_PASS="${SMSRELAY_SSH_PASS:-${DEVICE_PASS:-}}"
exec "$(dirname "$0")/openwrt/deploy.sh" "$@"
