#!/bin/sh
# 每日兜底：清空 modem 内可能残留的孤儿短信分片。
#
# 背景：长短信分片若在组装完成前被删掉，剩余分片会留在 SIM 里变成孤儿，
# 之后短信中心复用相同分片序号时会撞车，导致整条短信丢失。
# 正常流程（正文稳定 6 秒才删除）已极少产生孤儿，这里每天兜底清一次，
# 保证 73 个槽位不会被垃圾占满。
#
# 必须停掉 ModemManager 才能独占 AT 口，否则会打断它读短信。
# 全程纯 shell（stty + dd），不依赖 python3。
set -u

AT=/dev/wwan0at1
log() { echo "[smsrelay-maint] $*"; }

systemctl stop smsrelay 2>/dev/null
systemctl stop ModemManager 2>/dev/null
sleep 3

stty -F "$AT" 115200 raw -echo -echoe -echok -echoctl -echoke 2>/dev/null || true
exec 3<>"$AT"
at() {
    printf '%s\r' "$1" >&3
    timeout 3 dd bs=1 count=512 <&3 2>/dev/null | tr -d '\r'
}

before=$(at 'AT+CPMS?')
log "before: $(printf '%s' "$before" | grep CPMS | tr -d '\n')"
out=$(at 'AT+CMGD=1,4')
case "$out" in
    *OK*) log "清空成功" ;;
    *)    log "清空可能失败: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
after=$(at 'AT+CPMS?')
log "after: $(printf '%s' "$after" | grep CPMS | tr -d '\n')"
exec 3<&-

systemctl start ModemManager
i=0
while [ $i -lt 30 ]; do
    mmcli -L 2>/dev/null | grep -q Modem && break
    i=$((i+1)); sleep 1
done
log "ModemManager 就绪（${i}s）"
systemctl start smsrelay
log "done"
