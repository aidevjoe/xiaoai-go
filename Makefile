# 小爱GO 构建入口
#
# 两个二进制：
#   client  音箱端，纯 Go 静态编译到 armv7（CGO 必须关闭）
#   server  服务端，需要 cgo（sherpa-onnx 是 C++ 库），正式构建走容器

GO        ?= go
DIST      ?= dist
LDFLAGS   ?= -s -w -buildid= -X main.buildTime=$(shell date +%Y%m%d-%H%M%S)
IMAGE     ?= xiaoai-go:local

.PHONY: all client client-tls server fake ui test bench lint fmt size \
        docker docker-run dev-run dev-build clean help

all: client server

## ---------------------------------------------------------------- 客户端
##
## 必须用 Go 1.23 编译，不能用 1.24+（本机的 1.27 也不行）。
##
## 原因：Go 1.24 起标准库加密包改成 crypto/internal/fips140 模块实现，
## 这个模块在音箱的 Linux 4.9（aarch64 内核跑 32 位用户态）上启动即段错误
## ——连 `xiaoai-client -h` 都跑不到，run.sh 只能每 3 秒重启一次。
## 实测：同一段 `import "crypto/sha1"` 的小程序，1.23 编出来正常，1.27 编出来 rc=139。
## 客户端要 SHA-1 做 WebSocket 握手，所以一定会链进加密包。
CLIENT_TOOLCHAIN ?= go1.23.8

client:
	@mkdir -p $(DIST)
	GOTOOLCHAIN=$(CLIENT_TOOLCHAIN) CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS)" -o $(DIST)/xiaoai-client-armv7 ./cmd/client
	@ls -lh $(DIST)/xiaoai-client-armv7

## 带 TLS 的客户端：只有服务端暴露到公网、必须走 wss:// 时才用。
## 代价是 crypto/tls 与 crypto/x509，二进制大约多 2MB。
client-tls:
	@mkdir -p $(DIST)
	GOTOOLCHAIN=$(CLIENT_TOOLCHAIN) CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -tags tls \
		-ldflags "$(LDFLAGS)" -o $(DIST)/xiaoai-client-armv7-tls ./cmd/client
	@ls -lh $(DIST)/xiaoai-client-armv7-tls

## ---------------------------------------------------------------- 服务端
##
## 本机（macOS）只能编出不含引擎的版本；真正要跑引擎必须在 linux 上用 cgo 编。
server:
	@mkdir -p $(DIST)
	$(GO) build -o $(DIST)/xiaoai-server ./cmd/server

## 模拟音箱，本机联调用
fake:
	@mkdir -p $(DIST)
	$(GO) build -o $(DIST)/xiaoai-fake ./cmd/fakeclient

## ---------------------------------------------------------------- 前端
## 产物落在 internal/server/web/dist（入库，go:embed 需要它）
ui:
	cd ui && npm ci && npm run build

## ---------------------------------------------------------------- 质量
test:
	$(GO) test ./... -count=1

bench:
	$(GO) test ./internal/proto/ -run '^$$' -bench . -benchtime 200000x

lint:
	$(GO) vet ./...
	@out=$$(gofmt -l cmd internal); \
	if [ -n "$$out" ]; then echo "未格式化："; echo "$$out"; exit 1; fi

fmt:
	gofmt -w cmd internal

## 体积报告：看客户端里到底是谁在占地方
size:
	@echo "== 依赖包数 =="
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) list -deps ./cmd/client | wc -l
	@echo "== 二进制 =="
	@ls -lh $(DIST)/xiaoai-client-armv7 2>/dev/null || echo "先跑 make client"

## ---------------------------------------------------------------- Docker
docker: ui
	docker build -f deploy/Dockerfile.server -t $(IMAGE) .

## 本地跑一个（不挂模型，只验证接口与后台）
docker-run:
	docker run --rm -p 4399:4399 -p 8080:8080 $(IMAGE) -no-engine

## ---------------------------------------------------------------- 本机开发
## 引擎是 cgo + linux，本机跑不了；-no-engine 足够验证接口、后台与对话逻辑
dev-run:
	@mkdir -p .data
	$(GO) run ./cmd/server -no-engine -device-addr :24399 -admin-addr :28080 \
		-settings .data/settings.json -wake-words 小黑

## 快速迭代：用常驻编译容器出 linux 二进制，直接 docker cp 进正在跑的容器
dev-build:
	sh deploy/scripts/dev-build.sh

clean:
	rm -rf $(DIST)

help:
	@grep -E '^## ' Makefile | sed 's/^## //'
