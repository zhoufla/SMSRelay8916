#!/usr/bin/env bash
#
# 交叉编译 SMSRelay8916（默认目标：Linux / arm64，静态二进制）
#
# 用法：
#   ./build.sh                  # 编译 linux/arm64，输出 dist/smsrelay
#   ./build.sh --install-go     # 先装 Go（需要 sudo）再编译
#   ./build.sh --host           # 编译本机架构（在 x86 上调试用）
#   ./build.sh --no-proxy       # 强制直连，不走代理
#
set -euo pipefail

cd "$(dirname "$0")"

APP_NAME="smsrelay"
OUT_DIR="dist"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

# 局域网现成代理，用于访问 proxy.golang.org 等国外站点。
# 可通过环境变量覆盖：SMSRELAY_PROXY=http://other:port
DEFAULT_PROXY="${SMSRELAY_PROXY:-http://127.0.0.1:7890}"

TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-arm64}"
USE_PROXY="auto"

INSTALL_GO=0
while [ $# -gt 0 ]; do
    case "$1" in
        --install-go) INSTALL_GO=1 ;;
        --host)       TARGET_OS=""; TARGET_ARCH="" ;;
        --no-proxy)   USE_PROXY="off" ;;
        --proxy)      shift; DEFAULT_PROXY="$1"; USE_PROXY="on" ;;
        -h|--help)
            sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) echo "未知参数: $1" >&2; exit 2 ;;
    esac
    shift
done

log() { printf '\033[36m[build]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[build] %s\033[0m\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 1. Go 工具链
# ---------------------------------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
    if [ "$INSTALL_GO" = "1" ]; then
        log "安装 Go（apt）..."
        sudo apt-get update -qq
        sudo apt-get install -y golang-go ca-certificates
    else
        cat >&2 <<'EOF'
[build] 未找到 go 命令。

二选一：
  a) 本脚本代装（Ubuntu/Debian，需要 sudo）：
       ./build.sh --install-go

  b) 装官方最新版（更推荐，apt 的版本通常偏旧）：
       wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
       sudo rm -rf /usr/local/go
       sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
       echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile
       exec $SHELL -l
EOF
        exit 1
    fi
fi

GO_VER="$(go env GOVERSION | sed 's/^go//')"
log "Go 版本: $GO_VER  ($(go env GOROOT))"

# go.mod 要求 1.21+；modernc.org/sqlite 也需要较新的标准库
GO_MAJOR="$(echo "$GO_VER" | cut -d. -f1)"
GO_MINOR="$(echo "$GO_VER" | cut -d. -f2)"
if [ "$GO_MAJOR" -lt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -lt 21 ]; }; then
    die "Go 版本过低（$GO_VER），需要 1.21 及以上"
fi

# ---------------------------------------------------------------------------
# 2. 网络：决定是否走代理
# ---------------------------------------------------------------------------
probe() {
    # $1 = url，直接用 curl 探活，超时 6 秒
    curl -sS -o /dev/null -m 6 "$1" 2>/dev/null
}

if [ "$USE_PROXY" = "auto" ]; then
    log "探测模块代理可达性..."
    if probe "https://goproxy.cn/"; then
        log "goproxy.cn 直连可用，不走 HTTP 代理"
        USE_PROXY="off"
    elif probe "$DEFAULT_PROXY"; then
        log "直连失败，改用代理 $DEFAULT_PROXY"
        USE_PROXY="on"
    else
        log "两者都不通，仍尝试直连（可能失败）"
        USE_PROXY="off"
    fi
fi

if [ "$USE_PROXY" = "on" ]; then
    export HTTP_PROXY="$DEFAULT_PROXY"
    export HTTPS_PROXY="$DEFAULT_PROXY"
    export http_proxy="$DEFAULT_PROXY"
    export https_proxy="$DEFAULT_PROXY"
    # 局域网与回环不要走代理
    export NO_PROXY="localhost,127.0.0.1,192.168.0.0/16,10.0.0.0/8,172.16.0.0/12"
    export no_proxy="$NO_PROXY"
fi

# 国内代理优先，失败回落到官方
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"
export GOFLAGS="${GOFLAGS:-}"

log "GOPROXY=$GOPROXY"

# ---------------------------------------------------------------------------
# 3. 拉依赖
# ---------------------------------------------------------------------------
# 依赖版本不写死在 go.mod 里，交给 go get 选当前可用的最新版，
# 避免猜错版本号导致 tidy 阶段直接失败。
if [ ! -f go.sum ] || [ "$(wc -l < go.sum)" -lt 5 ]; then
    log "首次拉取依赖（godbus + modernc sqlite，约 30MB）..."
    go get github.com/godbus/dbus/v5@latest
    go get modernc.org/sqlite@latest
    go mod tidy
else
    log "依赖已就绪（需要更新版本时：rm -f go.sum && ./build.sh）"
fi

# ---------------------------------------------------------------------------
# 4. 编译
# ---------------------------------------------------------------------------
mkdir -p "$OUT_DIR"

# CGO_ENABLED=0 是关键：modernc.org/sqlite 是纯 Go 实现，
# 关掉 cgo 后不需要任何交叉 C 工具链，直接出静态二进制。
export CGO_ENABLED=0

BUILD_ENV=()
if [ -n "$TARGET_OS" ] && [ -n "$TARGET_ARCH" ]; then
    export GOOS="$TARGET_OS"
    export GOARCH="$TARGET_ARCH"
    BUILD_ENV+=("GOOS=$TARGET_OS" "GOARCH=$TARGET_ARCH")
    OUT="$OUT_DIR/${APP_NAME}-${TARGET_OS}-${TARGET_ARCH}"
else
    OUT="$OUT_DIR/${APP_NAME}"
fi

log "编译 -> $OUT"
go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o "$OUT" ./cmd/smsrelay

log "完成"
ls -lh "$OUT" | awk '{printf "  文件 %s  大小 %s\n", $9, $5}'
file "$OUT" 2>/dev/null | sed 's/^/  /' || true

# 版本自检（只有在同架构下才能直接跑）
if [ -z "$TARGET_ARCH" ] || [ "$TARGET_ARCH" = "$(go env GOHOSTARCH)" ]; then
    printf '  版本: '; "$OUT" -version || true
fi

cat <<EOF

下一步：
  部署到设备   ./deploy.sh 192.168.1.100
  或手工拷贝   scp $OUT root@192.168.1.100:/opt/smsrelay/smsrelay
EOF
