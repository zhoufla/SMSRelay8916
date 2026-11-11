#!/bin/sh
# SMSRelay8916 → OpenWrt 设备部署脚本（在构建机上跑）
#
# 用法: STAGING_DIR=/path/to/staging_dir ./openwrt/deploy.sh <设备IP> [SSH密码]
#   SSH 密码也可用环境变量 SMSRELAY_SSH_PASS 传入（避免出现在 shell 历史）
#
# 做的事：
#   1. 用 OpenWrt 工具链交叉编译 arm64（CGO + 系统 libsqlite3）
#   2. scp 二进制、服务、RPC、UCI、原生 LuCI 到设备
#   3. 远端启用并启动服务
#
# 注意：
#   - 正式发布请走 OpenWrt 包（openwrt/Makefile → ipk）。本脚本只是
#     不重刷固件的快速迭代通道，需要用构建树/SDK 的 STAGING_DIR 编译：
#     sqlite 走 CGO，宿主机自带的 gcc 编不出能在 musl 上跑的二进制。
#   - 设备上必须有 libsqlite3（ipk 会作为依赖装上；手工部署时脚本会先检查）。
#   - UCI 配置只在设备上还不存在时推送，绝不覆盖已改过的值。
set -e

HOST=${1:?"usage: STAGING_DIR=... deploy.sh <device-ip> [ssh-password]"}
PASS=${2:-${SMSRELAY_SSH_PASS:-}}
[ -n "$PASS" ] || { echo "need ssh password (arg2 or SMSRELAY_SSH_PASS env)"; exit 1; }
DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$DIR/.." && pwd)

SSH="sshpass -p $PASS ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
SCP="sshpass -p $PASS scp -O -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o PreferredAuthentications=password -o PubkeyAuthentication=no"

[ -n "${STAGING_DIR:-}" ] || {
	echo "!! 未设置 STAGING_DIR"
	echo "   sqlite 通过 CGO 链接系统 libsqlite3，必须用 OpenWrt 工具链编译。"
	echo "   在构建树根目录执行： STAGING_DIR=\$PWD/staging_dir ./openwrt/deploy.sh <ip> <pass>"
	echo "   或者直接出 ipk： cp -a openwrt package/smsrelay && make package/smsrelay/compile"
	exit 1
}
TARGET_DIR=$(echo "$STAGING_DIR"/target-*)
CC=$(ls "$STAGING_DIR"/toolchain-*/bin/*-openwrt-linux-musl-gcc 2>/dev/null | head -n 1)
[ -n "$CC" ] || { echo "!! 在 $STAGING_DIR 下找不到 *-openwrt-linux-musl-gcc"; exit 1; }
[ -f "$TARGET_DIR/usr/include/sqlite3.h" ] || {
	echo "!! $TARGET_DIR/usr/include/sqlite3.h 不存在：先在构建树里 make package/libs/sqlite3/compile"
	exit 1
}

VERSION=$(cd "$ROOT" && git describe --tags --always 2>/dev/null || echo dev)

echo "== 编译 arm64（CGO + libsqlite3）=="
cd "$ROOT"
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
	CC="$CC" \
	CGO_CFLAGS="-I$TARGET_DIR/usr/include" \
	CGO_LDFLAGS="-L$TARGET_DIR/usr/lib" \
	go build -tags libsqlite3 -trimpath \
	-ldflags "-s -w -X main.Version=openwrt-$VERSION" \
	-o "$DIR/files/usr/bin/smsrelay" ./cmd/smsrelay
ls -lh "$DIR/files/usr/bin/smsrelay"

echo "== 检查设备依赖 =="
$SSH root@$HOST "ls /usr/lib/libsqlite3.so* >/dev/null 2>&1" || {
	echo "!! 设备上没有 libsqlite3，先装包：apk add libsqlite3（或 opkg install libsqlite3）"
	exit 1
}
$SSH root@$HOST "command -v ubus >/dev/null && test -d /usr/share/rpcd/acl.d && test -f /www/luci-static/resources/view.js" || {
	echo '!! 设备必须先安装 luci-base、rpcd、rpcd-mod-file'
	exit 1
}

echo "== 上传 =="
$SSH root@$HOST "/etc/init.d/smsrelay stop 2>/dev/null || true"
$SCP "$DIR/files/usr/bin/smsrelay" root@$HOST:/usr/bin/smsrelay
$SCP "$DIR/files/usr/bin/smsrelay-keepalive" root@$HOST:/usr/bin/smsrelay-keepalive
$SCP "$DIR/files/usr/bin/sms" root@$HOST:/usr/bin/sms
$SCP "$DIR/files/etc/init.d/smsrelay" root@$HOST:/etc/init.d/smsrelay
for service in jz02-power jz02-led; do
	$SCP "$DIR/files/etc/init.d/$service" root@$HOST:/etc/init.d/
done
# UCI 配置：只在设备上还不存在时推送，不覆盖通道与凭据。
$SSH root@$HOST "[ -f /etc/config/smsrelay ]" || \
	$SCP "$DIR/files/etc/config/smsrelay" root@$HOST:/etc/config/smsrelay
$SSH root@$HOST "[ -f /etc/config/jz02 ]" || \
	$SCP "$DIR/files/etc/config/jz02" root@$HOST:/etc/config/jz02
$SCP "$DIR/files/usr/share/luci/menu.d/luci-app-smsrelay.json" root@$HOST:/usr/share/luci/menu.d/
$SCP "$DIR/files/usr/share/rpcd/acl.d/luci-app-smsrelay.json" root@$HOST:/usr/share/rpcd/acl.d/
$SSH root@$HOST "mkdir -p /www/luci-static/resources/view/smsrelay /usr/libexec/rpcd /lib/upgrade /etc/uci-defaults"
$SCP "$DIR/files/www/luci-static/resources/view/smsrelay/"*.js root@$HOST:/www/luci-static/resources/view/smsrelay/
$SCP "$DIR/files/www/luci-static/resources/smsrelay.js" root@$HOST:/www/luci-static/resources/
$SCP "$DIR/files/usr/libexec/rpcd/smsrelay" root@$HOST:/usr/libexec/rpcd/smsrelay
$SCP "$DIR/files/lib/upgrade/smsrelay.sh" root@$HOST:/lib/upgrade/
$SCP "$DIR/files/etc/uci-defaults/90-smsrelay" root@$HOST:/etc/uci-defaults/

echo "== 启用 =="
$SSH root@$HOST "chmod +x /usr/bin/sms /usr/bin/smsrelay /usr/bin/smsrelay-keepalive /etc/init.d/smsrelay /etc/init.d/jz02-power /etc/init.d/jz02-led /usr/libexec/rpcd/smsrelay
sh -n /etc/init.d/smsrelay && echo 'init 语法 OK'
sh /etc/uci-defaults/90-smsrelay && rm -f /etc/uci-defaults/90-smsrelay
/etc/init.d/smsrelay enable
/etc/init.d/smsrelay restart
/etc/init.d/jz02-power restart
/etc/init.d/jz02-led restart
/etc/init.d/rpcd restart 2>/dev/null
sleep 3
pgrep -f /usr/bin/smsrelay >/dev/null && echo 'smsrelay 运行中' || echo '!! smsrelay 未启动'
pgrep -f smsrelay-keepalive >/dev/null && echo 'keepalive 运行中' || echo '!! keepalive 未启动'
ubus call smsrelay status '{}'"

echo "== 完成：LuCI → 服务 → 短信网关 =="
