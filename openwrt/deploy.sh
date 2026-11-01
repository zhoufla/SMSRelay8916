#!/bin/sh
# SMSRelay8916 → OpenWrt 设备部署脚本（在构建机上跑）
#
# 用法: ./openwrt/deploy.sh [设备IP] [SSH密码]
#   默认 192.168.68.1 / 123456
#
# 做的事：
#   1. 交叉编译 arm64 静态二进制（CGO_ENABLED=0，musl 直接跑）
#   2. scp 二进制 + keepalive + init + LuCI 菜单到设备
#   3. 远端启用并启动服务
set -e

HOST=${1:-192.168.68.1}
PASS=${2:-123456}
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
