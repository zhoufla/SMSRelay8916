.PHONY: all build host deploy clean tidy vet fmt privacy

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

all: build

# 交叉编译 linux/arm64（部署到设备用）
build:
	VERSION=$(VERSION) ./build.sh

# 编译本机架构（在 x86 上跑起来看逻辑）
host:
	VERSION=$(VERSION) ./build.sh --host

# 部署：必须显式给出设备地址，脚本里不内置任何主机
deploy:
	@[ -n "$(HOST)" ] || { echo "用法：make deploy HOST=192.168.1.100" >&2; exit 1; }
	./deploy.sh $(HOST)

# 隐私自检：确认没有真实的号码 / 内网地址 / 凭证（提交前先跑）
privacy:
	./scripts/privacy-check.sh
	./scripts/privacy-check.sh --history

tidy:
	go mod tidy

vet:
	CGO_ENABLED=0 go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf dist
