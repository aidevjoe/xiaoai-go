#!/bin/sh
# 把服务端部署到一台远程 Linux 主机（需要 SSH + docker）。
#
# 用途：本地不想装 cgo 交叉编译链时，把源码同步过去、在目标机上原生构建镜像。
# 适合群晖/极空间/QNAP 这类 NAS，也适合任意一台 x86/arm 的 Linux 服务器。
#
# 用法（本脚本用你的 SSH key，需能免密登录）：
#   NAS_HOST=192.168.1.10 sh deploy/scripts/deploy-server.sh
#
# 环境变量：
#   NAS_HOST    目标主机（必填）
#   NAS_PORT    SSH 端口（默认 22；极空间是 10000）
#   NAS_DIR     部署目录（必填。极空间可以是 /tmp/zfsv3/*/*/data/Docker/xiaoai-go）
#   ADMIN_PORT  管理面在宿主机的端口（默认 8080）
#   CONTAINER   容器名（默认 xiaoai-server）
#   IMAGE       镜像名（默认 xiaoai-server:local）
#
# 为什么在 NAS 上编译而不是在 Mac 上交叉编译：
#   sherpa-onnx 是 C++ 库，服务端必须 cgo 编译；在 Apple Silicon 上用
#   Rosetta 模拟 amd64 跑 go build，Go 编译器会自己段错误（实测
#   `crypto/internal/sysrand: signal: segmentation fault`）。
#   NAS 是 x86_64 原生，容器里直接编译最稳。
set -e

NAS_HOST="${NAS_HOST:?请设置 NAS_HOST=目标主机地址}"
NAS_PORT="${NAS_PORT:-22}"
ADMIN_PORT="${ADMIN_PORT:-8080}"
CONTAINER="${CONTAINER:-xiaoai-server}"
IMAGE="${IMAGE:-xiaoai-server:local}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

say() { printf '\033[36m==>\033[0m %s\n' "$1"; }
die() { printf '\033[31m%s\033[0m\n' "$1" >&2; exit 1; }

# 连接复用：极空间 sshd 在短时间内接多个连接会掉，复用一条最稳
SSH_CTL=/tmp/xiaoai-nas-ssh.sock
nas() {
    ssh -p "$NAS_PORT" -o BatchMode=yes -o ControlMaster=auto -o ControlPath="$SSH_CTL" \
        -o ControlPersist=600 "root@$NAS_HOST" "$@"
}

# ---- 0) 部署目录 ----
if [ -z "$NAS_DIR" ]; then
    # 极空间：数据卷固定在 /tmp/zfsv3/<盘>/<id>/data，自动探一下省得手打
    NAS_DIR=$(nas 'ls -d /tmp/zfsv3/*/*/data/Docker 2>/dev/null | head -1')
    [ -n "$NAS_DIR" ] && NAS_DIR="$NAS_DIR/xiaoai-go"
fi
[ -n "$NAS_DIR" ] || die "请设置 NAS_DIR=目标机上的部署目录（例如 /volume1/docker/xiaoai-go）"
say "NAS: $NAS_HOST:$NAS_PORT  目录: $NAS_DIR"

say "检查 NAS 环境"
nas 'uname -m; docker --version'
ARCH=$(nas 'uname -m' | tr -d '\r')
[ "$ARCH" = "x86_64" ] || say "注意：NAS 架构是 $ARCH，镜像会按它原生编译"

# ---- 1) 上传源码 ----
# 只打包编译真正需要的路径，不用 --exclude：
# `--exclude=dist` 会把 internal/server/web/dist（前端产物，go:embed 引用）一起排掉，
# 构建时报 `pattern all:dist: no matching files found`。白名单最省心。
DIST=internal/server/web/dist
if [ ! -f "$ROOT/$DIST/index.html" ]; then
    say "前端产物缺失，先构建 UI"
    ( cd "$ROOT/ui" && npm run build )
fi
# 防御：产物里必须有 index.html，否则 go:embed 一定失败
[ -f "$ROOT/$DIST/index.html" ] || die "前端产物缺失: $DIST/index.html（先跑 cd ui && npm run build）"

say "打包并上传源码"
tar czf - -C "$ROOT" go.mod go.sum cmd internal deploy \
    | nas "rm -rf $NAS_DIR/src && mkdir -p $NAS_DIR/src && tar xzf - -C $NAS_DIR/src && echo \"    源码 $(du -sh $NAS_DIR/src | cut -f1)\" && ls $NAS_DIR/src"

# ---- 2) 上传模型 ----
# 只传运行时真正会用到的：VAD / ASR / TTS(aishell3) / KWS。
# fanchen 与 melo 是当初选型时试过的，一个太慢一个模型文件是坏的，不传。
say "同步模型（约 480MB，已存在的不重复传）"
MODELS="silero_vad.onnx
sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17
vits-icefall-zh-aishell3
sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"
[ -d "$ROOT/models" ] || die "本地没有 models/ 目录，先跑 sh deploy/scripts/fetch-models-local.sh"

# 本地缺哪个就在本地拉哪个，避免"传上去才发现少了模型"
for m in $MODELS; do
    [ -e "$ROOT/models/$m" ] || die "本地缺模型: models/$m（先跑 fetch-models-local.sh）"
done

# NAS 上已有的不重传（480MB，重跑一次部署不该再等一遍）
EXIST=$(nas "ls $NAS_DIR/models 2>/dev/null" | tr -d '\r')
TO_SEND=""
for m in $MODELS; do
    if printf '%s\n' "$EXIST" | grep -qx "$m"; then
        say "    已存在，跳过: $m"
    else
        TO_SEND="$TO_SEND $m"
    fi
done
if [ -n "$TO_SEND" ]; then
    say "传输:$TO_SEND"
    tar czf - -C "$ROOT/models" $TO_SEND \
        | nas "mkdir -p $NAS_DIR/models && tar xzf - -C $NAS_DIR/models"
else
    say "模型已齐，无需传输"
fi
nas "ls -1 $NAS_DIR/models"

# ---- 3) 上传配置（含 LLM 与 HA 的凭据，只放在自己的 NAS 上）----
if [ -f "$ROOT/.data/settings.json" ]; then
    say "同步 settings.json（若 NAS 上已有则不覆盖）"
    cat "$ROOT/.data/settings.json" | nas "mkdir -p $NAS_DIR/data
if [ -f $NAS_DIR/data/settings.json ]; then cat > /dev/null; echo '    已有配置，保留不动';
else cat > $NAS_DIR/data/settings.json; echo '    已写入配置'; fi"
else
    say "本地没有 .data/settings.json，NAS 上会用默认配置"
fi

# ---- 4) 在 NAS 上编译镜像 ----
# 注意不能用 `docker build ... | tail`：管道会把 docker 的退出码换成 tail 的 0，
# 构建失败了脚本还继续往下走，最后 `docker run` 去 Docker Hub 找一个不存在的镜像
# （报 TLS handshake timeout，很容易误判成网络问题）。
say "在 NAS 上构建镜像（首次要拉 golang 基础镜像与依赖，约 3~8 分钟）"
if nas "cd $NAS_DIR/src && docker build -f deploy/Dockerfile.server -t $IMAGE . > /tmp/xiaoai-build.log 2>&1"; then
    nas "tail -6 /tmp/xiaoai-build.log"
else
    nas "tail -40 /tmp/xiaoai-build.log" >&2
    die "镜像构建失败（完整日志在 NAS:/tmp/xiaoai-build.log）"
fi
nas "docker image inspect $IMAGE >/dev/null 2>&1" || die "镜像 $IMAGE 不存在，构建可能没真正成功"

# ---- 5) 起容器 ----
say "启动容器"
nas "docker rm -f $CONTAINER >/dev/null 2>&1 || true
docker run -d --name $CONTAINER --restart unless-stopped \
  -p 4399:4399 -p $ADMIN_PORT:8080 \
  -v $NAS_DIR/models:/models:ro \
  -v $NAS_DIR/data:/data \
  -e TZ=Asia/Shanghai \
  $IMAGE \
  -device-addr=:4399 -admin-addr=:8080 -models=/models -settings=/data/settings.json"

# ---- 6) 等就绪 ----
say "等待服务就绪"
i=0
while [ $i -lt 60 ]; do
    if nas "curl -fsS -m 2 http://127.0.0.1:$ADMIN_PORT/api/health" 2>/dev/null | grep -q '"ok":true'; then
        say "已就绪"
        nas "curl -s http://127.0.0.1:$ADMIN_PORT/api/health; echo; docker logs --tail 12 $CONTAINER"
        break
    fi
    i=$((i + 1))
    sleep 2
done
[ $i -lt 60 ] || { nas "docker logs --tail 40 $CONTAINER"; die "启动超时"; }

# ---- 7) 下一步 ----
cat <<EOF

服务端已就绪，管理后台: http://$NAS_HOST:$ADMIN_PORT/

把音箱指到这台服务端（在本地执行）：
  make client
  DEV=音箱IP PASS='音箱root密码' SERVER=ws://$NAS_HOST:4399/ws sh deploy/scripts/install-client.sh

之后改服务端地址不用重刷固件：改 $NAS_DIR/data/ 下的配置即可。
EOF
