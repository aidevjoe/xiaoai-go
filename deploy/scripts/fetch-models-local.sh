#!/bin/sh
# 本机拉取最小模型集（用于验证引擎能加载）
#
# 用法：  sh deploy/scripts/fetch-models-local.sh [目标目录]
# 默认：  ./models
#
# 说明：
#   VAD  2.3MB  —— 必需，判断有没有人说话
#   ASR  229MB  —— SenseVoice，中文识别
#   TTS  约 60MB —— VITS 中文，合成声音
# 已有文件会跳过，可重复执行。
set -e

DEST="${1:-./models}"
mkdir -p "$DEST"

say() { printf '\033[36m==>\033[0m %s\n' "$1"; }

fetch() {
    url="$1"
    out="$2"
    if [ -f "$out" ]; then
        say "已存在，跳过: $out"
        return
    fi
    say "下载: $(basename "$out")"
    curl -fL --retry 3 --connect-timeout 20 -o "$out.part" "$url"
    mv "$out.part" "$out"
}

# 1) Silero VAD
if [ ! -f "$DEST/silero_vad.onnx" ]; then
    fetch "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx" "$DEST/silero_vad.onnx"
fi

# 2) SenseVoice（中文识别）
if [ ! -d "$DEST/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17" ]; then
    say "下载 SenseVoice（约 229MB）"
    curl -fL --retry 3 --connect-timeout 20 -o "$DEST/sensevoice.tar.bz2" \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17.tar.bz2"
    tar xjf "$DEST/sensevoice.tar.bz2" -C "$DEST"
    rm -f "$DEST/sensevoice.tar.bz2"
fi

# 3) VITS 中文（合成）
if [ ! -d "$DEST/vits-zh-aishell3" ]; then
    say "下载 VITS 中文（约 60MB）"
    curl -fL --retry 3 --connect-timeout 20 -o "$DEST/vits.tar.bz2" \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-icefall-zh-aishell3.tar.bz2"
    tar xjf "$DEST/vits.tar.bz2" -C "$DEST"
    rm -f "$DEST/vits.tar.bz2"
fi

say "完成，当前模型目录："
ls -lh "$DEST" | head -20
