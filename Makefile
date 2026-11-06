# 小爱GO 构建入口
#
# 两个二进制：
#   client  音箱端，纯 Go 静态编译到 armv7（CGO 必须关闭）
#   server  服务端，需要 cgo（sherpa-onnx 是 C++ 库），正式构建走容器
#
# 客户端必须用 Go 1.23 编译，不能用 1.24+（本机的 1.27 也不行）。
# 原因：Go 1.24 起标准库加密包改成 crypto/internal/fips140 模块实现，
# 这个模块在音箱的 Linux 4.9（aarch64 内核跑 32 位用户态）上启动即段错误
# ——连 `xiaoai-client -h` 都跑不到，run.sh 只能每 3 秒重启一次。
# 实测：同一段 `import "crypto/sha1"` 的小程序，1.23 编出来正常，1.27 编出来 rc=139。
# 客户端要 SHA-1 做 WebSocket 握手，所以一定会链进加密包。

GO        ?= go
DIST      ?= dist
LDFLAGS   ?= -s -w -buildid= -X main.buildTime=$(shell date +%Y%m%d-%H%M%S)
IMAGE     ?= xiaoai-go:local
CLIENT_TOOLCHAIN ?= go1.23.8

.PHONY: all client client-tls server fake ui test bench lint fmt size \
        docker docker-run dev-run dev-build clean help

all: client server ## 编译全部

client: ## 交叉编译音箱端（armv7，约 3MB）
	@mkdir -p $(DIST)
	GOTOOLCHAIN=$(CLIENT_TOOLCHAIN) CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS)" -o $(DIST)/xiaoai-client-armv7 ./cmd/client
	@ls -lh $(DIST)/xiaoai-client-armv7

client-tls: ## 同上，但带 TLS（服务端走 wss:// 时才需要，多约 2MB）
	@mkdir -p $(DIST)
	GOTOOLCHAIN=$(CLIENT_TOOLCHAIN) CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -tags tls \
		-ldflags "$(LDFLAGS)" -o $(DIST)/xiaoai-client-armv7-tls ./cmd/client
	@ls -lh $(DIST)/xiaoai-client-armv7-tls

server: ## 编译服务端（本机无 cgo 引擎，仅用于开发）
	@mkdir -p $(DIST)
	$(GO) build -o $(DIST)/xiaoai-server ./cmd/server

fake: ## 编译模拟音箱（本机联调用）
	@mkdir -p $(DIST)
	$(GO) build -o $(DIST)/xiaoai-fake ./cmd/fakeclient

ui: ## 重建前端（产物落在 internal/server/web/dist，需一并提交）
	cd ui && npm ci && npm run build

test: ## 跑单元测试
	$(GO) test ./... -count=1

bench: ## 协议层基准
	$(GO) test ./internal/proto/ -run '^$$' -bench . -benchtime 200000x

lint: ## go vet + gofmt 检查
	$(GO) vet ./...
	@out=$$(gofmt -l cmd internal); \
	if [ -n "$$out" ]; then echo "以下文件未格式化，请跑 make fmt："; echo "$$out"; exit 1; fi
	@echo "lint ok"

fmt: ## 格式化 Go 代码
	gofmt -w cmd internal

size: ## 看客户端体积都由什么组成
	@echo "== 依赖包数 =="
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) list -deps ./cmd/client | wc -l
	@echo "== 二进制 =="
	@ls -lh $(DIST)/xiaoai-client-armv7 2>/dev/null || echo "先跑 make client"

docker: ui ## 本地构建服务端镜像（含前端）
	docker build -f deploy/Dockerfile.server -t $(IMAGE) .

docker-run: ## 跑一个不加载模型的容器（验证接口与后台）
	docker run --rm -p 4399:4399 -p 8080:8080 $(IMAGE) -no-engine

dev-run: ## 本机跑服务端（-no-engine，不需要模型）
	@mkdir -p .data
	$(GO) run ./cmd/server -no-engine -device-addr :24399 -admin-addr :28080 \
		-settings .data/settings.json -wake-words 小黑

dev-build: ## 用常驻编译容器出 linux 二进制，并替换进运行中的容器
	sh deploy/scripts/dev-build.sh

clean: ## 清掉本地编译产物
	rm -rf $(DIST)

help: ## 列出所有目标
	@grep -E '^[a-z][a-z-]*:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  \033[36m%-13s\033[0m %s\n", $$1, $$2}'
