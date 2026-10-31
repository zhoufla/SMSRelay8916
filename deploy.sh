#!/usr/bin/env bash
#
# 把编译好的二进制部署到设备。
#
# 用法：
#   ./deploy.sh <设备地址>            # 例如 ./deploy.sh 192.168.1.100
#   DEVICE_PASS=xxx ./deploy.sh <地址>  # 指定密码（非交互）
#   ./deploy.sh <地址> --keep-old     # 不停止旧版 sms-gw 服务（默认会停，避免抢 8088）
#
# 其余可用环境变量：DEVICE_USER（默认 root）
#
set -euo pipefail

cd "$(dirname "$0")"

# 目标设备地址必须由调用方给出，脚本里不写死任何主机
HOST="${1:-}"
[ "${HOST#-}" != "$HOST" ] && HOST=""
USER_NAME="${DEVICE_USER:-root}"
PASS="${DEVICE_PASS:-}"
BIN="dist/smsrelay-linux-arm64"
REMOTE_DIR="/opt/smsrelay"
CONF_DIR="/etc/smsrelay"

KEEP_OLD=0
for a in "$@"; do
    [ "$a" = "--keep-old" ] && KEEP_OLD=1
done

log()  { printf '\033[36m[deploy]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[deploy] %s\033[0m\n' "$*"; }
die()  { printf '\033[31m[deploy] %s\033[0m\n' "$*" >&2; exit 1; }

[ -n "$HOST" ] || die "缺少设备地址。用法：./deploy.sh <设备地址>（例如 ./deploy.sh 192.168.1.100）"
[ -f "$BIN" ] || die "找不到 $BIN，先跑 ./build.sh"

# ---------------------------------------------------------------------------
# SSH 通道
# ---------------------------------------------------------------------------
SSH_BIN="ssh"
SCP_BIN="scp"
if [ -n "$PASS" ] && command -v sshpass >/dev/null 2>&1; then
    SSH_BIN="sshpass -p $PASS ssh"
    SCP_BIN="sshpass -p $PASS scp"
elif [ -z "$PASS" ]; then
    warn "未设置 DEVICE_PASS，将使用密钥或交互式输入密码"
fi

SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10"

rsh()  { $SSH_BIN $SSH_OPTS "$USER_NAME@$HOST" "$@"; }
rcp()  { $SCP_BIN $SSH_OPTS "$@"; }

log "目标 $USER_NAME@$HOST"

# ---------------------------------------------------------------------------
# 预检
# ---------------------------------------------------------------------------
ARCH_REMOTE="$(rsh 'uname -m' | tr -d '\r')"
log "设备架构: $ARCH_REMOTE"
case "$ARCH_REMOTE" in
    aarch64|arm64) ;;
    *) die "二进制是 arm64 的，设备是 $ARCH_REMOTE，请用 TARGET_ARCH=$ARCH_REMOTE ./build.sh 重新编译" ;;
esac

if ! rsh 'command -v mmcli >/dev/null 2>&1'; then
    die "设备上没有 mmcli（ModemManager 未安装）"
fi

# ---------------------------------------------------------------------------
# 上传
# ---------------------------------------------------------------------------
log "上传二进制..."
rcp "$BIN" "$USER_NAME@$HOST:/tmp/smsrelay.new"

log "安装并配置..."
rsh bash -s <<REMOTE
set -e
mkdir -p "$REMOTE_DIR" "$CONF_DIR" /var/lib/smsrelay

install -m 0755 /tmp/smsrelay.new "$REMOTE_DIR/smsrelay"
rm -f /tmp/smsrelay.new

# 配置：优先复用旧 Python 版的钉钉 token 等设置，
# 因为两个版本的配置字段名完全一致。
if [ ! -f "$CONF_DIR/config.json" ]; then
    if [ -f /etc/sms-gw/config.json ]; then
        cp /etc/sms-gw/config.json "$CONF_DIR/config.json"
        echo "  配置: 已从 /etc/sms-gw/config.json 复制"
    else
        cat > "$CONF_DIR/config.json" <<'CONF'
{
  "listen_host": "0.0.0.0",
  "listen_port": 8088,
  "api_token": "",
  "db_path": "/var/lib/smsrelay/sms.db",
  "guard_interval": 60,
  "modem_index": "0",
  "web_dir": "/opt/smsrelay/web",
  "own_number": "",
  "dingtalk": { "enabled": false, "url": "", "prefix": "【短信】", "max_per_min": 15 }
}
CONF
        echo "  配置: 已生成默认配置 $CONF_DIR/config.json（钉钉未启用）"
    fi
else
    echo "  配置: 已存在，保持不动"
fi
chmod 600 "$CONF_DIR/config.json"

# Web 目录（可选）：没有就用二进制内置页面
mkdir -p "$REMOTE_DIR/web"

echo "  二进制版本: \$("$REMOTE_DIR/smsrelay" -version 2>/dev/null || echo '?')"
REMOTE

# ---------------------------------------------------------------------------
# systemd
# ---------------------------------------------------------------------------
log "安装 systemd 单元..."
rcp deploy/smsrelay.service "$USER_NAME@$HOST:/tmp/smsrelay.service"

rsh bash -s <<REMOTE
set -e
install -m 0644 /tmp/smsrelay.service /etc/systemd/system/smsrelay.service
rm -f /tmp/smsrelay.service
systemctl daemon-reload
systemctl enable smsrelay >/dev/null 2>&1
REMOTE

# ---------------------------------------------------------------------------
# 辅助脚本与定时器：每日清理、LTE 保活、命令行工具
# 这些东西以前只存在于设备上，换台机器就没了，所以随部署一起安装。
# ---------------------------------------------------------------------------
log "安装辅助脚本与定时器..."
for f in smsrelay-maint.sh smsrelay-keepalive.sh sms; do
    rcp "scripts/$f" "$USER_NAME@$HOST:/tmp/$f"
done
for u in smsrelay-maint.service smsrelay-maint.timer smsrelay-keepalive.service; do
    rcp "deploy/$u" "$USER_NAME@$HOST:/tmp/$u"
done

rsh bash -s <<REMOTE
set -e
install -m 0755 /tmp/smsrelay-maint.sh     /usr/local/bin/smsrelay-maint.sh
install -m 0755 /tmp/smsrelay-keepalive.sh /usr/local/bin/smsrelay-keepalive.sh
install -m 0755 /tmp/sms                          /usr/local/bin/sms
install -m 0644 /tmp/smsrelay-maint.service     /etc/systemd/system/smsrelay-maint.service
install -m 0644 /tmp/smsrelay-maint.timer       /etc/systemd/system/smsrelay-maint.timer
install -m 0644 /tmp/smsrelay-keepalive.service /etc/systemd/system/smsrelay-keepalive.service
rm -f /tmp/smsrelay-maint.sh /tmp/smsrelay-keepalive.sh /tmp/sms \
      /tmp/smsrelay-maint.service /tmp/smsrelay-maint.timer /tmp/smsrelay-keepalive.service

# 装上去之前先验语法：装一个跑不起来的清理脚本，比不装更糟
sh -n /usr/local/bin/smsrelay-maint.sh
sh -n /usr/local/bin/smsrelay-keepalive.sh
sh -n /usr/local/bin/sms
echo "  脚本语法: OK"

systemctl daemon-reload
systemctl enable smsrelay-maint.timer >/dev/null 2>&1 || true
systemctl enable smsrelay-keepalive.service >/dev/null 2>&1 || true

if command -v jq >/dev/null 2>&1; then
    echo "  jq: 已安装，sms 命令可用"
else
    echo "  ⚠ 缺少 jq，sms 命令不可用：apt-get install jq"
fi
REMOTE

# ---------------------------------------------------------------------------
# 切换：停旧版，启新版
# ---------------------------------------------------------------------------
if [ "$KEEP_OLD" = "0" ]; then
    log "停止旧版 sms-gw（避免抢占 8088）..."
    rsh 'systemctl disable --now sms-gw 2>/dev/null || true; systemctl is-active sms-gw 2>/dev/null || echo "  sms-gw: 已停止"'
fi

log "启动 smsrelay 与辅助服务..."
rsh 'systemctl restart smsrelay; systemctl restart smsrelay-keepalive; systemctl start smsrelay-maint.timer; sleep 4
     systemctl is-active smsrelay smsrelay-keepalive smsrelay-maint.timer'

echo
log "状态自检"
rsh bash -s <<'REMOTE'
echo "--- 服务 ---"
systemctl is-active smsrelay
echo "--- 辅助 ---"
for u in smsrelay-keepalive smsrelay-maint.timer; do
    printf "  %-24s %s\n" "$u" "$(systemctl is-active $u 2>/dev/null)"
done
systemctl list-timers smsrelay-maint.timer --no-pager 2>/dev/null | awk 'NR==2{print "  下次清理: "$1" "$2" "$3}'
echo "--- 内存 ---"
P=$(systemctl show -p MainPID --value smsrelay)
ps -o rss=,pcpu= -p "$P" 2>/dev/null | awk '{printf "  RSS=%.1fMB CPU=%s%%\n", $1/1024, $2}'
echo "--- HTTP ---"
curl -s -m 10 http://127.0.0.1:8088/api/ping || echo "  接口未响应"
echo
if command -v jq >/dev/null 2>&1; then
    curl -s -m 15 http://127.0.0.1:8088/api/status | jq -r '
      "  modem  : \(.modem.state) / \(.modem.access_tech) / \(.modem.registration) / signal \(.modem.signal)",
      "  sim    : \(.modem.imsi) \(.modem.iccid)",
      "  本机号码: \(.own_number // "-")",
      "  归档   : 收 \(.counts.inbox) / 发 \(.counts.sent) / 总 \(.counts.total)",
      "  今日   : 收 \(.today.inbox // 0) / 发 \(.today.sent // 0)",
      "  版本   : \(.gateway.version)"
    '
else
    echo "  （设备上没有 jq，跳过状态解析；apt-get install jq 可启用）"
fi
echo "--- 日志 ---"
journalctl -u smsrelay -n 8 --no-pager | tail -8
REMOTE

echo
log "完成。Web UI: http://$HOST:8088"
cat <<'EOF'

回退到上一个二进制：
  systemctl stop smsrelay
  cp /opt/smsrelay/smsrelay.bak /opt/smsrelay/smsrelay
  systemctl start smsrelay
EOF
