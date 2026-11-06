#!/bin/sh
# 小爱GO · 音箱端安装
#
# 推荐：直接在音箱上跑，一行命令，不用 clone 仓库、不用装 Go 和 sshpass
#
#   # 1) 先 SSH 进音箱（已刷 open-xiaoai 固件）
#   ssh -o HostKeyAlgorithms=+ssh-rsa root@音箱IP
#
#   # 2) 告诉它服务端在哪，然后跑这个脚本
#   curl -sSfL https://raw.githubusercontent.com/aidevjoe/xiaoai-go/main/deploy/scripts/install-client.sh \
#     | SERVER=ws://192.168.1.10:4399/ws sh
#
# 也可以在电脑上远程装（需要 sshpass：brew install hudochenkov/sshpass/sshpass）：
#
#   DEV=192.168.1.50 PASS='音箱root密码' SERVER=ws://192.168.1.10:4399/ws \
#     sh deploy/scripts/install-client.sh
#
# 环境变量：
#   SERVER   服务端地址（必填，ws://IP:4399/ws）。装好后写进 server.txt，改它不用重装
#   DEV      音箱 IP。给了就是"电脑远程装"；不给就当成"正跑在音箱上"
#   PASS     远程装的 SSH 密码（远程模式必填）
#   ID       设备 ID，默认取主机名（只保留字母数字和 . _ -）
#   BIN      用本地已有的二进制（不给就自动从 GitHub Release 下载）
#   VERSION  Release 版本，默认 latest
#   ASSET    下载哪个包，默认按 SERVER 自动选（wss:// → xiaoai-client-armv7-tls）
#
# 几个必须这么做的点（都是在这台机器上踩出来的）：
#   - 先传到 /tmp 再落到 /data：/data 只剩 4~5MB，放不下新旧两份；
#     而 /tmp 是 58MB 的 tmpfs（内存盘）
#   - 落地用 `cat > 目标` 原地覆盖，不做 rename：覆盖前已停掉进程，
#     不存在 ETXTBSY；原地覆盖也不额外占空间
#   - 停进程不用 killall（这台机器上匹配不上，实测杀不掉）：改成扫 /proc/*/comm，
#     残留多实例会因同一个 device id 互相踢下线，表现成"每分钟掉线"
#   - 日志写 /tmp：/data 空间宝贵，日志涨起来会把固件空间吃光
set -eu

SERVER="${SERVER:?请设置 SERVER=ws://服务端地址:4399/ws}"
DEV="${DEV:-}"
PASS="${PASS:-}"
ID="${ID:-$(hostname 2>/dev/null || echo lx06)}"
BIN="${BIN:-}"
VERSION="${VERSION:-latest}"
REPO="${REPO:-aidevjoe/xiaoai-go}"

# 默认构建不带 TLS（省 2MB）；服务端走 wss:// 时自动换成 -tls 那个
case "$SERVER" in
    wss://*) ASSET="${ASSET:-xiaoai-client-armv7-tls}" ;;
    *)       ASSET="${ASSET:-xiaoai-client-armv7}" ;;
esac

DIR=/data/xiaoai
STAGE=/tmp/xiaoai-client.new
DL=/tmp/xiaoai-client.dl

say() { printf '\033[36m==>\033[0m %s\n' "$1"; }
die() { printf '\033[31m%s\033[0m\n' "$1" >&2; exit 1; }

# ---------------------------------------------------------------- 连接方式
# 一个 run() 抽象掉"在本地执行"和"通过 ssh 在音箱上执行"两种模式：
# 下面所有动作（停进程、探麦克风、写守护脚本）两边完全一样。
# run 一律把 stdin 接到 /dev/null：这个脚本经常被 `curl ... | sh` 跑，
# 子进程要是读了 stdin，后面的脚本内容就被吃掉了。
# 需要往音箱写内容的场合走 put_stream（自带单独管道），**不能**用 run，
# 否则 /dev/null 会把管道顶掉，写出 0 字节文件（踩过：server.txt 是空的）。
if [ -n "$DEV" ]; then
    command -v sshpass >/dev/null 2>&1 || die "远程模式需要 sshpass（brew install hudochenkov/sshpass/sshpass）"
    SSH_OPTS="-o HostKeyAlgorithms=+ssh-rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8"
    export SSHPASS="$PASS"
    run() { sshpass -e ssh $SSH_OPTS "root@$DEV" "$1" </dev/null; }
    put_stream() { sshpass -e ssh $SSH_OPTS "root@$DEV" "cat > $1"; }
    say "远程模式：$DEV"
else
    [ -d /data ] || die "没找到 /data：请确认这是在音箱上执行（想在电脑上装就设 DEV=音箱IP）"
    run() { sh -c "$1" </dev/null; }
    put_stream() { sh -c "cat > $1"; }
    say "本地模式：$(uname -m)"
fi

# 把本地文件 / 一行文本写到音箱上（走管道，省掉 scp 及其兼容性问题）
put_file() { cat "$1" | put_stream "$2"; }
put_text() { printf '%s\n' "$1" | put_stream "$2"; }

# 列出 xiaoai-client 的 PID。刻意不用 pgrep -x：不同 busybox 版本对 -x 的解释不一样，
# 实测 1.36 上 `pgrep -x xiaoai-client` 明明进程在也返回 1（直接扫 /proc 才靠谱）。
PROC_SCAN='for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = xiaoai-client ] && echo "${d#/proc/}"; done'
running() { run "test -n \"\$($PROC_SCAN)\""; }

# 生成脚本内容用的临时文件（生成在"跑脚本的这台机器"上：电脑或音箱本身）
TMPD="${TMPDIR:-/tmp}"
TMP_RUN="$TMPD/xiaoai-run.sh.$$"
TMP_INIT="$TMPD/xiaoai-init.sh.$$"
trap 'rm -f "$TMP_RUN" "$TMP_INIT"' EXIT

# ---------------------------------------------------------------- 1. 准备二进制
if [ -z "$BIN" ]; then
    BIN="$DL"
    if [ "$VERSION" = "latest" ]; then
        URL="https://github.com/$REPO/releases/latest/download/$ASSET"
    else
        URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
    fi
    say "下载客户端：$URL"
    if command -v curl >/dev/null 2>&1; then
        curl -fL --progress-bar --retry 3 --connect-timeout 20 -o "$BIN" "$URL" \
            || die "下载失败。可手动下载后用 BIN=/path/to/xiaoai-client-armv7 重跑"
    else
        wget -O "$BIN" "$URL" || die "下载失败（没有 curl，wget 也没成功）"
    fi
fi
[ -s "$BIN" ] || die "二进制不存在或为空: $BIN"
NEW_KB=$(( $(wc -c <"$BIN") / 1024 ))
say "待安装：$(( NEW_KB / 1024 ))MB"

# ---------------------------------------------------------------- 2. 空间检查
# /data 只剩 4~5MB 是常态，先看够不够；否则传到一半失败会把旧版本也搞坏
OLD_KB=$(run "ls -l $DIR/xiaoai-client 2>/dev/null | awk '{print \$5}'" | tr -d '\r')
[ -z "$OLD_KB" ] && OLD_KB=0
OLD_KB=$(( OLD_KB / 1024 ))
AVAIL_KB=$(run "df -k /data | tail -1 | awk '{print \$4}'" | tr -d '\r')
say "空间：新 ${NEW_KB}KB / 旧 ${OLD_KB}KB / 可用 ${AVAIL_KB}KB"
if [ "$AVAIL_KB" -lt $(( NEW_KB - OLD_KB + 256 )) ]; then
    die "空间不足：覆盖安装还需 $(( NEW_KB - OLD_KB + 256 ))KB，可用 ${AVAIL_KB}KB"
fi

# ---------------------------------------------------------------- 3. 上传
# 先放 /tmp（58MB tmpfs）再原地落到 /data：直接往 /data 写要同时容纳新旧两份
say "上传到内存盘"
[ "$BIN" = "$STAGE" ] || put_file "$BIN" "$STAGE"

# ---------------------------------------------------------------- 4. 停掉旧实例
# 顺序很重要：
#  1. 先停 run.sh 守护循环——它会在客户端退出 3 秒后用【旧命令行】把客户端拉起来，
#     新装的二进制会带着上一版参数跑（踩过：-device 一直不生效）
#  2. 再杀客户端，TERM 杀不掉就 -9（旧版本会卡在 socket 读上）
#  3. 最后杀 arecord——它是客户端的子进程，客户端死了会变孤儿继续占着麦克风
# 模式写成 [r]un 而不是 run：pkill -f 会匹配到自己这条命令，否则自杀。
say "停掉旧实例"
run "if [ -f /var/run/xiaoai.pid ]; then kill \$(cat /var/run/xiaoai.pid) 2>/dev/null; fi
pkill -f '[r]un[.]sh' 2>/dev/null; sleep 1
for p in \$($PROC_SCAN); do kill \$p 2>/dev/null; done; sleep 1
for p in \$($PROC_SCAN); do kill -9 \$p 2>/dev/null; done
pkill -x arecord 2>/dev/null; sleep 1
if [ -n \"\$($PROC_SCAN)\" ]; then echo '⚠ 客户端仍在运行'; else echo '(已停)'; fi"

# ---------------------------------------------------------------- 5. 落地
say "安装到 $DIR/xiaoai-client"
run "mkdir -p $DIR && cat $STAGE > $DIR/xiaoai-client && chmod +x $DIR/xiaoai-client && rm -f $STAGE"
run "ls -l $DIR/xiaoai-client"

# ---------------------------------------------------------------- 6. 服务端地址
say "写入服务端地址"
put_text "$SERVER" "$DIR/server.txt"

# ---------------------------------------------------------------- 7. 探测麦克风
# 这台固件的坑：/etc/asound.conf 把 `pcm.noop` 定义成了 `pcm.Capture`
# （dsnoop 共享 hw:0,3 PDM 麦克风）的别名，**不是空设备**；
# 而真正的 hw:0,1/hw:0,2 是恒为静音的 dummy 通道（能开、有字节、全 0）。
# 所以判据必须是"非零字节数"，不能只看文件大小，否则会选中一个哑巴设备。
say "探测麦克风设备"
CAP=$(run 'best=""; bestnz=0
for d in noop mico_record hw:0,3 hw:0,0 hw:0,1; do
  rm -f /tmp/cap.raw
  arecord --quiet -t raw -D "$d" -f S32_LE -r 16000 -c 1 -d 1 /tmp/cap.raw >/dev/null 2>&1
  [ -s /tmp/cap.raw ] || continue
  nz=$(tr -d "\000" </tmp/cap.raw | wc -c)
  if [ "$nz" -gt "$bestnz" ]; then best="$d"; bestnz="$nz"; fi
  [ "$nz" -gt 500 ] && break
done
rm -f /tmp/cap.raw
if [ -n "$best" ]; then echo "$best $bestnz"; else echo "none 0"; fi' | tr -d '\r')
CAP_DEV=$(echo "$CAP" | awk '{print $1}')
CAP_NZ=$(echo "$CAP" | awk '{print $2}')
case "$CAP_DEV" in
    ""|none)
        echo "    ⚠ 没探到可用采集设备，退回 noop" >&2
        CAP_DEV=noop ;;
    *)
        echo "    选中 $CAP_DEV（1 秒内非零字节 $CAP_NZ）" ;;
esac
put_text "$CAP_DEV" "$DIR/capture.txt"

# ---------------------------------------------------------------- 8. 守护脚本
# 已经在跑就不重复启动：多实例用同一个 device id 会互相踢下线，
# 表现成"每分钟掉线重连好几次"，排查很费劲。
# 日志轮转用 mv 而不是 `: > $LOG`：客户端以 O_APPEND 写同一个文件，
# 就地截断会把它这次运行的日志全抹掉。
# 先在本地生成，再整份推过去：不依赖远端 sed 的 -i 方言，也不会和内联引号打架
cat > "$TMP_RUN" <<'EOF'
#!/bin/sh
# 客户端守护：退出后 3 秒重启。由 /data/init.sh 调用。
cd /data/xiaoai || exit 1
LOG=/tmp/xiaoai-client.log

# 已经在跑就不重启：多实例用同一个 device id 会互相踢下线，表现成"每分钟掉线"。
# 不用 pgrep -x 是因为不同 busybox 版本行为不一致，直接看 /proc 最稳。
running() {
    for d in /proc/[0-9]*; do
        [ "$(cat "$d/comm" 2>/dev/null)" = xiaoai-client ] && return 0
    done
    return 1
}

if running; then
    echo "[$(date '+%m-%d %H:%M:%S')] 已有实例在跑，跳过本次启动" >> "$LOG"
    exit 0
fi

[ -f "$LOG" ] && [ "$(wc -c <$LOG)" -gt 524288 ] && mv "$LOG" "$LOG.prev"

while true; do
    SRV="$(cat /data/xiaoai/server.txt 2>/dev/null | tr -d '\r\n')"
    [ -z "$SRV" ] && SRV="ws://127.0.0.1:4399/ws"
    # 采集设备必须在这里给：客户端 -device 默认 noop，而各固件的可用设备名不同。
    CAP="$(cat /data/xiaoai/capture.txt 2>/dev/null | tr -d '\r\n')"
    [ -z "$CAP" ] && CAP="noop"
    echo "[$(date '+%m-%d %H:%M:%S')] 启动客户端 → $SRV (采集设备 $CAP)" >> "$LOG"
    ./xiaoai-client -server "$SRV" -id '__ID__' -device "$CAP" -wakewords /data/xiaoai/wakewords.txt >> "$LOG" 2>&1
    echo "[$(date '+%m-%d %H:%M:%S')] 客户端退出（code=$?），3 秒后重启" >> "$LOG"
    sleep 3
done
EOF
# 设备 ID 会写进 run.sh 的命令行，只留安全字符；过滤空了就退回 lx06
ID_SAFE=$(printf '%s' "$ID" | tr -cd 'A-Za-z0-9._-')
[ -n "$ID_SAFE" ] || ID_SAFE=lx06
[ "$ID_SAFE" = "$ID" ] || echo "    ⚠ 设备 ID 只保留字母数字和 . _ -：$ID → $ID_SAFE"
sed "s|__ID__|$ID_SAFE|g" "$TMP_RUN" > "$TMP_RUN.new" && mv "$TMP_RUN.new" "$TMP_RUN"
put_file "$TMP_RUN" "$DIR/run.sh"
run "chmod +x $DIR/run.sh"

# 开机自启
cat > "$TMP_INIT" <<'EOF'
#!/bin/sh
[ -x /data/xiaoai/run.sh ] || exit 0
exec sh /data/xiaoai/run.sh
EOF
put_file "$TMP_INIT" "/data/init.sh"
run "chmod +x /data/init.sh"

# ---------------------------------------------------------------- 9. 启动
# busybox 没有 setsid 也没有 nohup（`setsid: not found`），用 start-stop-daemon -b。
# 必须显式重定向 stdio：否则守护进程继承 ssh 的输出，会话里的后续输出会被吞。
say "启动"
run "mv /tmp/xiaoai-client.log /tmp/xiaoai-client.log.prev 2>/dev/null || true
start-stop-daemon -S -b -m -p /var/run/xiaoai.pid -x /bin/sh -- /data/xiaoai/run.sh >/dev/null 2>&1 </dev/null"
sleep 5

# 启动后必须确认进程真的起来了：音箱的 sshd 偶尔会在脚本中途断，
# 表现就是"停掉了旧的、新的没起来"，设备静默离线。
if ! running; then
    say "没起来，重试一次"
    run "start-stop-daemon -S -b -m -p /var/run/xiaoai.pid -x /bin/sh -- /data/xiaoai/run.sh >/dev/null 2>&1 </dev/null"
    sleep 4
    running || die "启动失败，看日志：tail -30 /tmp/xiaoai-client.log"
fi

say "设备状态"
run "echo 客户端: \$(echo \$($PROC_SCAN | tr '\n' ' ')); echo 录音: \$(pgrep -x arecord | tr '\n' ' '); echo '--- 最近日志 ---'; tail -8 /tmp/xiaoai-client.log"

say "完成。看日志：tail -f /tmp/xiaoai-client.log"
