#!/bin/sh
# SMSRelay8916 → OpenWrt 设备部署脚本（在构建机上跑）
#
# 用法: ./openwrt/deploy.sh <设备IP> [SSH密码]
#   SSH 密码也可用环境变量 SMSRELAY_SSH_PASS 传入（避免出现在 shell 历史）
#
# 做的事：
#   1. 交叉编译 arm64 静态二进制（CGO_ENABLED=0，musl 直接跑）
#   2. scp 二进制 + keepalive + init + UCI + LuCI 菜单到设备
#   3. 远端启用并启动服务
#
# 注意：正式部署建议走 OpenWrt 包（openwrt/Makefile → ipk），本脚本是
# 不重刷固件的快速迭代通道。UCI 配置只在设备上还不存在时推送，
# 不覆盖已改过的配置；init.d 每次启动从 UCI 渲染 config.json。
set -e

HOST=${1:?"usage: deploy.sh <device-ip> [ssh-password]"}
PASS=${2:-${SMSRELAY_SSH_PASS:-}}
[ -n "$PASS" ] || { echo "need ssh password (arg2 or SMSRELAY_SSH_PASS env)"; exit 1; }
DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$DIR/.." && pwd)

SSH="sshpass -p $PASS ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
SCP="sshpass -p $PASS scp -O -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o PreferredAuthentications=password -o PubkeyAuthentication=no"

VERSION=$(cd "$ROOT" && git describe --tags --always 2>/dev/null || echo dev)

echo "== 编译 arm64 =="
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags "-s -w -X main.Version=openwrt-$VERSION" \
    -o "$DIR/files/usr/bin/smsrelay" ./cmd/smsrelay
ls -lh "$DIR/files/usr/bin/smsrelay"

echo "== 上传 =="
$SCP "$DIR/files/usr/bin/smsrelay" root@$HOST:/usr/bin/smsrelay
$SCP "$DIR/files/usr/bin/smsrelay-keepalive" root@$HOST:/usr/bin/smsrelay-keepalive
$SCP "$DIR/files/etc/init.d/smsrelay" root@$HOST:/etc/init.d/smsrelay
# UCI 启动配置：只在设备上还不存在时推送，避免覆盖已改过的配置
$SSH root@$HOST "[ -f /etc/config/smsrelay ]" || \
	$SCP "$DIR/files/etc/config/smsrelay" root@$HOST:/etc/config/smsrelay
$SCP "$DIR/files/usr/share/luci/menu.d/luci-app-smsrelay.json" root@$HOST:/usr/share/luci/menu.d/
$SCP "$DIR/files/usr/share/rpcd/acl.d/luci-app-smsrelay.json" root@$HOST:/usr/share/rpcd/acl.d/
$SSH root@$HOST "mkdir -p /www/luci-static/resources/view/smsrelay"
$SCP "$DIR/files/www/luci-static/resources/view/smsrelay/index.js" root@$HOST:/www/luci-static/resources/view/smsrelay/

echo "== 启用 =="
$SSH root@$HOST "chmod +x /usr/bin/smsrelay /usr/bin/smsrelay-keepalive /etc/init.d/smsrelay
sh -n /etc/init.d/smsrelay && echo 'init 语法 OK'
rm -rf /tmp/luci-indexcache /tmp/luci-modulecache 2>/dev/null
/etc/init.d/smsrelay enable
/etc/init.d/smsrelay restart
/etc/init.d/rpcd restart 2>/dev/null
sleep 3
pgrep -f /usr/bin/smsrelay >/dev/null && echo 'smsrelay 运行中' || echo '!! smsrelay 未启动'
pgrep -f smsrelay-keepalive >/dev/null && echo 'keepalive 运行中' || echo '!! keepalive 未启动'
curl -s -o /dev/null -w 'web http_code=%{http_code}\n' http://127.0.0.1:8088/ || true"

echo "== 完成：LuCI → 服务 → 短信网关 =="
