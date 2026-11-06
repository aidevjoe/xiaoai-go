#!/bin/sh
# 快速迭代：把服务端二进制编译出来，直接替换进正在跑的容器。
#
# 为什么不用 docker build：镜像里 COPY . . 会让 UI 产物一变就整层重来，
# 一次 1~4 分钟。这里只跑 go build，然后用 docker cp + restart 换二进制，10~30 秒。
#
# 前提：一个常驻编译容器（有 gcc），脚本会自动建：
#   docker run -d --name xa-build -v $PWD:/src -w /src golang:1.24-bookworm sleep infinity
#
# 用法：
#   APP=xiaoai-server ADMIN_PORT=8080 sh deploy/scripts/dev-build.sh   # 编译 + 替换 + 重启
#   sh deploy/scripts/dev-build.sh --no-restart                        # 只编译
#
# 注意：只适用于服务端。音箱端 client 是纯 Go，用 make client 交叉编译即可。
set -e

BUILDER="${BUILDER:-xa-build}"
# APP 是正在运行的应用容器名（要在它里面换二进制）。不给就只编译。
APP="${APP:-}"
OUT=dist/xa-server-linux

# 1) 确保编译器容器在
if ! docker inspect "$BUILDER" >/dev/null 2>&1; then
    echo "==> 创建编译容器 $BUILDER（首次会装 gcc，约 30 秒）"
    docker run -d --name "$BUILDER" -v "$PWD":/src -w /src golang:1.24-bookworm sleep infinity >/dev/null
    docker exec "$BUILDER" sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends gcc libc6-dev >/dev/null 2>&1'
elif ! docker exec "$BUILDER" sh -c 'command -v gcc' >/dev/null 2>&1; then
    docker exec "$BUILDER" sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends gcc libc6-dev >/dev/null 2>&1'
fi

# 2) 编译（cgo + sherpa 静态链接库在 go module 里）
echo "==> 编译服务端（linux/$(docker exec "$BUILDER" go env GOARCH)）"
T0=$(date +%s)
docker exec "$BUILDER" sh -c "CGO_ENABLED=1 go build -tags sherpa -trimpath -ldflags '-s -w' -o $OUT ./cmd/server"
echo "==> 编译完成，用时 $(( $(date +%s) - T0 ))s，$(ls -lh "$OUT" | awk '{print $5}')"

if [ "$1" = "--no-restart" ]; then
    exit 0
fi

# 3) 换二进制并重启（restart 只重跑 entrypoint，1~3 秒）
if [ -z "$APP" ]; then
    echo "==> 没指定 APP，只编译不替换（设 APP=容器名 可自动替换并重启）"
    exit 0
fi
if ! docker inspect "$APP" >/dev/null 2>&1; then
    echo "找不到应用容器 $APP" >&2
    exit 1
fi
echo "==> 替换 $APP 里的二进制并重启"
docker cp "$OUT" "$APP":/usr/local/bin/xiaoai-server
docker restart "$APP" >/dev/null

# 4) 等它就绪
i=0
while [ $i -lt 30 ]; do
    if curl -s -m 1 --noproxy 127.0.0.1 "http://127.0.0.1:${ADMIN_PORT:-8080}/api/health" | grep -q '"ok":true'; then
        echo "==> 已就绪：$(curl -s --noproxy 127.0.0.1 "http://127.0.0.1:${ADMIN_PORT:-8080}/api/health" | head -c 120)"
        exit 0
    fi
    i=$((i + 1))
    sleep 1
done
echo "==> 30 秒内没起来，看日志：docker logs $APP --tail 40" >&2
exit 1
