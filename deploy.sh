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
# 切换：停旧版，启新版
# ---------------------------------------------------------------------------
if [ "$KEEP_OLD" = "0" ]; then
    log "停止旧版 sms-gw（避免抢占 8088）..."
    rsh 'systemctl disable --now sms-gw 2>/dev/null || true; systemctl is-active sms-gw 2>/dev/null || echo "  sms-gw: 已停止"'
fi

log "启动 smsrelay..."
rsh 'systemctl restart smsrelay; sleep 4; systemctl is-active smsrelay'

echo
log "状态自检"
rsh bash -s <<'REMOTE'
echo "--- 服务 ---"
systemctl is-active smsrelay
echo "--- 内存 ---"
P=$(systemctl show -p MainPID --value smsrelay)
ps -o rss=,pcpu= -p "$P" 2>/dev/null | awk '{printf "  RSS=%.1fMB CPU=%s%%\n", $1/1024, $2}'
echo "--- HTTP ---"
curl -s -m 10 http://127.0.0.1:8088/api/ping || echo "  接口未响应"
echo
curl -s -m 15 http://127.0.0.1:8088/api/status | python3 -c '
import sys, json
d = json.load(sys.stdin)
m = d["modem"]
print("  modem  :", m["state"], "/", m["access_tech"], "/", m["registration"], "/ signal", m["signal"])
print("  sim    :", m["imsi"], m["iccid"])
print("  本机号码:", d.get("own_number"))
print("  归档   :", d["counts"])
print("  今日   :", d["today"])
print("  版本   :", d["gateway"]["version"])
' 2>/dev/null || echo "  状态接口解析失败"
echo "--- 日志 ---"
journalctl -u smsrelay -n 8 --no-pager | tail -8
REMOTE

echo
log "完成。Web UI: http://$HOST:8088"
cat <<'EOF'

回退到 Python 版：
  systemctl disable --now smsrelay
  systemctl enable --now sms-gw
EOF
