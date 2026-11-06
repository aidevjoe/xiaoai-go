#!/bin/sh
# 把客户端推到小爱音箱（已刷 open-xiaoai 固件）并装成开机自启。
#
# 用法（三个变量都必须给，脚本不猜你的网络）：
#   DEV=192.168.1.50 PASS='音箱root密码' SERVER=ws://192.168.1.10:4399/ws \
#       sh deploy/scripts/install-client.sh
#
# 环境变量：
#   DEV     音箱 IP（必填）
#   PASS    音箱 root 密码（必填；刷过 open-xiaoai 固件的机器通常是 open-xiaoai）
#   SERVER  服务端地址，写进音箱的 server.txt，之后改它不用重新装（必填）
#   ID      设备 ID（默认 lx06）
#   BIN     本地二进制路径（默认 dist/xiaoai-client-armv7）
#
# 几个必须这么做的点（都是在这台机器上踩出来的）：
#   - 先传到 /tmp 再落到 /data：/data 只剩 4~5MB，放不下新旧两份；
#     而 /tmp 是 58MB 的 tmpfs（内存盘）
#   - 落地用 `cat > 目标` 原地覆盖，不做 rename：覆盖前已停掉进程，
#     不存在 ETXTBSY；原地覆盖也不额外占空间
#   - 停进程用 pkill -x：这台机器的 killall 匹配不上（实测杀不掉），
#     残留多实例会因同一个 device id 互相踢下线，表现成"每分钟掉线"
#   - 日志写 /tmp：/data 空间宝贵，日志涨起来会把固件空间吃光
set -e

DEV="${DEV:?请设置 DEV=音箱IP}"
PASS="${PASS:?请设置 PASS=音箱root密码}"
SERVER="${SERVER:?请设置 SERVER=ws://服务端地址:4399/ws}"
ID="${ID:-lx06}"
BIN="${BIN:-dist/xiaoai-client-armv7}"
DIR=/data/xiaoai
STAGE=/tmp/xiaoai-client.new

[ -f "$BIN" ] || { echo "找不到 $BIN，先跑 make client" >&2; exit 1; }

SSH_OPTS="-o HostKeyAlgorithms=+ssh-rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
export SSHPASS="$PASS"
run() { sshpass -e ssh $SSH_OPTS "root@$DEV" "$@"; }
say() { printf '\033[36m==>\033[0m %s\n' "$1"; }

NEW_KB=$(( $(wc -c <"$BIN") / 1024 ))
# busybox 的 stat 参数不通用，用 ls 取大小
OLD_KB=$(run "ls -l $DIR/xiaoai-client 2>/dev/null | awk '{print \$5}'" | tr -d '\r')
[ -z "$OLD_KB" ] && OLD_KB=0
OLD_KB=$(( OLD_KB / 1024 ))
AVAIL_KB=$(run "df -k /data | tail -1 | awk '{print \$4}'" | tr -d '\r')

say "空间检查：新 ${NEW_KB}KB / 旧 ${OLD_KB}KB / 可用 ${AVAIL_KB}KB"
# 原地覆盖只需要"新比旧大出来的那部分"额外空间
if [ "$AVAIL_KB" -lt $(( NEW_KB - OLD_KB + 256 )) ]; then
    echo "空间不足：覆盖安装还需 $(( NEW_KB - OLD_KB + 256 ))KB，可用 ${AVAIL_KB}KB" >&2
    exit 1
fi

say "上传到内存盘（不占 /data）"
sshpass -e scp $SSH_OPTS -O "$BIN" "root@$DEV:$STAGE"

say "停掉所有实例（多实例会互相踢下线）"
# 顺序很重要：
#  1. 先停 run.sh 守护循环——它会在客户端退出 3 秒后用【旧命令行】把客户端拉起来，
#     新装的二进制会带着上一版参数跑（踩过：-device 一直不生效）
#  2. 再杀客户端，TERM 杀不掉就 -9（旧版本会卡在 socket 读上）
#  3. 最后杀 arecord——它是客户端的子进程，客户端死了会变孤儿继续占着麦克风
# 模式写成 [r]un 而不是 run：pkill -f 会匹配到自己这条命令，否则自杀。
run "if [ -f /var/run/xiaoai.pid ]; then kill \$(cat /var/run/xiaoai.pid) 2>/dev/null; fi
pkill -f '[r]un[.]sh' 2>/dev/null; sleep 1
pkill -x xiaoai-client 2>/dev/null; sleep 1; pkill -9 -x xiaoai-client 2>/dev/null
pkill -x arecord 2>/dev/null; sleep 1
pgrep -x xiaoai-client >/dev/null && echo '⚠ 客户端仍在运行' || echo '(客户端已停)'"

say "原地覆盖到 $DIR/xiaoai-client"
run "mkdir -p $DIR && cat $STAGE > $DIR/xiaoai-client && chmod +x $DIR/xiaoai-client && rm -f $STAGE && ls -l $DIR/xiaoai-client"

say "写入服务端地址与守护脚本"
run "cat > $DIR/server.txt" <<EOF
$SERVER
EOF

# 探测采集设备。这台固件的坑：/etc/asound.conf 把 `pcm.noop` 定义成了
# `pcm.Capture`（dsnoop 共享 hw:0,3 PDM 麦克风）的别名，**不是空设备**；
# 而真正的 hw:0,1/hw:0,2 是恒为静音的 dummy 通道（能开、有字节、全 0）。
# 所以判据必须是"非零字节数"，不能只看文件大小，否则会选中一个哑巴设备。
say "探测麦克风设备"
CAP=$(run 'best=""; bestnz=-1
for d in noop mico_record hw:0,3 hw:0,0 hw:0,1; do
  rm -f /tmp/cap.raw
  arecord --quiet -t raw -D "$d" -f S32_LE -r 16000 -c 1 -d 1 /tmp/cap.raw >/dev/null 2>&1
  [ -s /tmp/cap.raw ] || continue
  nz=$(tr -d "\000" </tmp/cap.raw | wc -c)
  if [ "$nz" -gt "$bestnz" ]; then best="$d"; bestnz="$nz"; fi
  [ "$nz" -gt 500 ] && break
done
rm -f /tmp/cap.raw
echo "$best $bestnz"' | tr -d '\r')
CAP_DEV=$(echo "$CAP" | awk '{print $1}')
CAP_NZ=$(echo "$CAP" | awk '{print $2}')
if [ -n "$CAP_DEV" ]; then
    echo "    选中 $CAP_DEV（1 秒内非零字节 $CAP_NZ）"
    printf '%s\n' "$CAP_DEV" | run "cat > $DIR/capture.txt"
else
    echo "    ⚠ 没探到可用采集设备，退回 noop" >&2
    echo "noop" | run "cat > $DIR/capture.txt"
fi

run "cat > $DIR/run.sh" <<'EOF'
#!/bin/sh
# 客户端守护：退出后 3 秒重启。由 /data/init.sh 调用。
cd /data/xiaoai || exit 1
LOG=/tmp/xiaoai-client.log

# 已经在跑就不重复启动。
# 按进程名判断而不是 pidfile：多实例用同一个 device id 会互相踢，
# 表现成"每分钟掉线重连好几次"，排查很费劲。
if pgrep -x xiaoai-client >/dev/null 2>&1; then
    echo "[$(date '+%m-%d %H:%M:%S')] 已有实例在跑，跳过本次启动" >> "$LOG"
    exit 0
fi

# 日志超过 512KB 就轮转（内存盘也别无限涨）。
# 用 mv 而不是就地截断：客户端以 O_APPEND 写同一个文件，
# `: > $LOG` 会把它这次运行的日志全抹掉。
[ -f "$LOG" ] && [ "$(wc -c <$LOG)" -gt 524288 ] && mv "$LOG" "$LOG.prev"

while true; do
    SRV="$(cat /data/xiaoai/server.txt 2>/dev/null | tr -d '\r\n')"
    [ -z "$SRV" ] && SRV="ws://127.0.0.1:4399/ws"
    # 采集设备必须在这里给：客户端的 -device 默认 noop，而各固件的可用设备名不同。
    # 注意 noop 在这台 LX06 上是厂商给 PDM 麦克风起的别名（见 /etc/asound.conf），
    # 不是空设备——别想当然把它换掉，换成 hw:0,x 会变成恒静音。
    CAP="$(cat /data/xiaoai/capture.txt 2>/dev/null | tr -d '\r\n')"
    [ -z "$CAP" ] && CAP="noop"
    echo "[$(date '+%m-%d %H:%M:%S')] 启动客户端 → $SRV (采集设备 $CAP)" >> "$LOG"
    ./xiaoai-client -server "$SRV" -id __ID__ -device "$CAP" -wakewords /data/xiaoai/wakewords.txt >> "$LOG" 2>&1
    echo "[$(date '+%m-%d %H:%M:%S')] 客户端退出（code=$?），3 秒后重启" >> "$LOG"
    sleep 3
done
EOF

# 把设备 id 填进守护脚本
run "sed -i 's/__ID__/$ID/' $DIR/run.sh && chmod +x $DIR/run.sh"

run "cat > /data/init.sh" <<'EOF'
#!/bin/sh
# 开机自启入口，由固件的 /etc/rc.local 调用。
#
# 这里刻意不做网络探测：上游脚本用 ping 百度判断联网，而本机 ICMP 被拦，
# 结果开机脚本直接卡死。客户端自身"连不上就退避重试"，网络晚几秒就绪没关系。
[ -x /data/xiaoai/run.sh ] || exit 0
exec sh /data/xiaoai/run.sh
EOF
run "chmod +x /data/init.sh"

say "启动"
# 这台机器的 busybox 没有 setsid，也没有 nohup（`setsid: not found`）。
# 之前用 setsid 启动其实一直没生效，靠上一轮遗留的旧 run.sh 循环在撑，
# 于是"新二进制 + 旧参数"的诡异现象反复出现。改用 start-stop-daemon -b。
# 注意必须显式重定向 stdio：否则守护进程继承 ssh 的输出，会话里的后续输出会被吞。
run "mv /tmp/xiaoai-client.log /tmp/xiaoai-client.log.prev 2>/dev/null
start-stop-daemon -S -b -m -p /var/run/xiaoai.pid -x /bin/sh -- /data/xiaoai/run.sh >/dev/null 2>&1 </dev/null"
sleep 5

# 启动后必须确认进程真的起来了：极空间/音箱的 sshd 偶尔会在脚本中途断，
# 表现就是"停掉了旧的、新的没起来"，设备静默离线。
if ! run "pgrep -x xiaoai-client >/dev/null"; then
    say "客户端没起来，重试一次启动"
    run "start-stop-daemon -S -b -m -p /var/run/xiaoai.pid -x /bin/sh -- /data/xiaoai/run.sh >/dev/null 2>&1 </dev/null"
    sleep 4
    run "pgrep -x xiaoai-client >/dev/null" || { echo "启动失败，看设备日志: ssh root@$DEV 'tail -30 /tmp/xiaoai-client.log'" >&2; exit 1; }
fi

say "设备侧状态"
run "echo 实例: \$(pgrep -x xiaoai-client | tr '\n' ' '); echo arecord: \$(pgrep -x arecord | tr '\n' ' '); tail -8 /tmp/xiaoai-client.log; echo '--- wakewords ---'; cat $DIR/wakewords.txt 2>/dev/null || echo '(尚未下发)'"

say "完成。日志: ssh root@$DEV 'tail -f /tmp/xiaoai-client.log'"
