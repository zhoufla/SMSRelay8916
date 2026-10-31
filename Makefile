.PHONY: all build host deploy clean tidy vet fmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

all: build

# 交叉编译 linux/arm64（部署到设备用）
build:
	VERSION=$(VERSION) ./build.sh

# 编译本机架构（在 x86 上跑起来看逻辑）
host:
	VERSION=$(VERSION) ./build.sh --host

deploy:
	./deploy.sh

tidy:
	go mod tidy

vet:
	CGO_ENABLED=0 go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf dist
