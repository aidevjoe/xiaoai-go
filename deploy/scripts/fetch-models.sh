#!/bin/sh
# 小爱GO · 模型下载
#
# 一行命令搞定：
#
#   curl -sSfL https://raw.githubusercontent.com/aidevjoe/xiaoai-go/main/deploy/scripts/fetch-models.sh \
#     | sh -s -- ~/xiaoai/models
#
# 或者先下载再执行（想看看脚本内容的话）：
#
#   curl -sSfL -o fetch-models.sh https://raw.githubusercontent.com/aidevjoe/xiaoai-go/main/deploy/scripts/fetch-models.sh
#   sh fetch-models.sh ~/xiaoai/models
#
# 可调的环境变量：
#   MODELS_DIR   目标目录（也可以用第一个参数传）
#   PROXY        GitHub 加速前缀，例如 https://ghproxy.net/（会拼在下载地址前面）
#   SKIP_TTS=1   不下载本地 TTS（只用云端豆包/Edge 时不需要，省 208MB）
#   SKIP_KWS=1   不下载唤醒词模型（用不到本地唤醒时）
#
# 已有文件会跳过，可以反复执行；下载先写 .part 再改名，中断不会留下半个文件。
set -eu

DEST="${1:-${MODELS_DIR:-./models}}"
PROXY="${PROXY:-}"
SKIP_TTS="${SKIP_TTS:-0}"
SKIP_KWS="${SKIP_KWS:-0}"

say() { printf '\033[36m==>\033[0m %s\n' "$1"; }
warn() { printf '\033[33m!\033[0m %s\n' "$1" >&2; }

mkdir -p "$DEST"
DEST=$(cd "$DEST" && pwd)
say "模型目录: $DEST"

# ---- 下载器：优先 curl，没有就用 wget（busybox 环境常见） ----
if command -v curl >/dev/null 2>&1; then
    DL() {
        # $1=url $2=输出文件
        curl -fL --progress-bar --retry 3 --retry-delay 2 --connect-timeout 20 -C - -o "$2" "$1"
    }
elif command -v wget >/dev/null 2>&1; then
    DL() {
        wget -O "$2" "$1"
    }
else
    warn "既没有 curl 也没有 wget，装一个再来"
    exit 1
fi

# 给 GitHub 地址套上加速前缀（国内直连 releases 常常几十 KB/s）
url() {
    case "$1" in
        http*) printf '%s%s' "$PROXY" "$1" ;;
        *) printf '%s' "$1" ;;
    esac
}

# 单个文件
fetch_file() {
    out="$DEST/$2"
    if [ -s "$out" ]; then
        say "跳过（已存在）: $2"
        return 0
    fi
    say "下载: $2"
    DL "$(url "$1")" "$out.part"
    mv "$out.part" "$out"
}

# 压缩包：先解到临时目录，成功才改名，避免"半个模型目录"让引擎加载失败
fetch_tar() {
    name="$2"
    if [ -d "$DEST/$name" ] && [ -n "$(ls -A "$DEST/$name" 2>/dev/null)" ]; then
        say "跳过（已存在）: $name"
        return 0
    fi
    say "下载: $name"
    tmp="$DEST/.tmp-$name"
    rm -rf "$tmp"
    mkdir -p "$tmp"
    DL "$(url "$1")" "$tmp/pkg.tar.bz2"
    tar xjf "$tmp/pkg.tar.bz2" -C "$tmp"
    rm -f "$tmp/pkg.tar.bz2"
    # 包内顶层目录名一般就是 $name；不是的话取唯一的那个目录
    if [ -d "$tmp/$name" ]; then
        src="$tmp/$name"
    else
        src=$(find "$tmp" -mindepth 1 -maxdepth 1 -type d | head -1)
    fi
    [ -n "$src" ] || { warn "解包后没找到目录: $name"; exit 1; }
    rm -rf "$DEST/$name"
    mv "$src" "$DEST/$name"
    rm -rf "$tmp"
}

BASE="https://github.com/k2-fsa/sherpa-onnx/releases/download"

say "① VAD（2MB，判断有没有人在说话）"
fetch_file "$BASE/asr-models/silero_vad.onnx" silero_vad.onnx

say "② 识别模型 SenseVoice（229MB，中/英/日/韩/粤）"
fetch_tar "$BASE/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17.tar.bz2" \
    sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17

if [ "$SKIP_KWS" != "1" ]; then
    say "③ 唤醒词模型（13MB，本地 KWS）"
    fetch_tar "$BASE/kws-models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01.tar.bz2" \
        sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01
fi

if [ "$SKIP_TTS" != "1" ]; then
    say "④ 本地合成模型 VITS（下载 60MB，解开 208MB；不用云端 TTS 时才需要）"
    fetch_tar "$BASE/tts-models/vits-icefall-zh-aishell3.tar.bz2" vits-icefall-zh-aishell3
fi

# ---- 自检：缺文件的服务端启动时会报"零件未加载"，提前说清楚 ----
say "校验"
miss=0
for f in silero_vad.onnx; do
    [ -s "$DEST/$f" ] || { warn "缺少 $f"; miss=1; }
done
[ -s "$DEST/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17/model.int8.onnx" ] \
    || { warn "识别模型不完整（缺 model.int8.onnx）"; miss=1; }
if [ "$SKIP_TTS" != "1" ]; then
    [ -s "$DEST/vits-icefall-zh-aishell3/model.onnx" ] \
        || { warn "合成模型不完整（缺 model.onnx）"; miss=1; }
fi
[ "$miss" = "0" ] || { warn "上面这些缺了服务端会少一个能力，可重跑本脚本补齐"; }

echo
say "完成。磁盘占用："
du -sh "$DEST" 2>/dev/null || true
ls -1 "$DEST"
echo
echo "下一步：把这个目录挂给容器 —— -v $DEST:/models:ro"
