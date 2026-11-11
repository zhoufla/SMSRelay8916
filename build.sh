#!/usr/bin/env bash
# ./build.sh --host：本机开发；STAGING_DIR=... ./build.sh：OpenWrt arm64。
set -euo pipefail
cd "$(dirname "$0")"
HOST_BUILD=0
for arg in "$@"; do
	case "$arg" in
		--host) HOST_BUILD=1 ;;
		--no-proxy) unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy ;;
		-h|--help) sed -n '2p' "$0"; exit 0 ;;
		*) echo "未知参数：$arg" >&2; exit 2 ;;
	esac
done
command -v go >/dev/null || { echo '请先安装 Go 1.26 或更新版本' >&2; exit 1; }
export CGO_ENABLED=1
if [ "$HOST_BUILD" = 1 ]; then
	unset GOOS GOARCH
	OUT=dist/smsrelay
	TAGS=()
else
	: "${STAGING_DIR:?OpenWrt 构建需要 STAGING_DIR；本机测试用 --host}"
	shopt -s nullglob
	TARGETS=("$STAGING_DIR"/target-*)
	COMPILERS=("$STAGING_DIR"/toolchain-*/bin/*-openwrt-linux-musl-gcc)
	[ "${#TARGETS[@]}" = 1 ] && [ "${#COMPILERS[@]}" = 1 ] || { echo '需要唯一的 target 与 OpenWrt musl gcc' >&2; exit 1; }
	[ -f "${TARGETS[0]}/usr/include/sqlite3.h" ] || { echo '请先编译目标 libsqlite3' >&2; exit 1; }
	export CC="${COMPILERS[0]}"
	export CGO_CFLAGS="-I${TARGETS[0]}/usr/include"
	export CGO_LDFLAGS="-L${TARGETS[0]}/usr/lib"
	export GOOS=linux GOARCH=arm64
	OUT=dist/smsrelay-linux-arm64
	TAGS=(-tags libsqlite3)
fi
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
mkdir -p dist
go build "${TAGS[@]}" -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o "$OUT" ./cmd/smsrelay
echo "已生成 $OUT"
