#!/bin/sh
# LTE 保活 + 断网自愈。
#
# 保活：周期发一个 ICMP 包，让 LTE 别停在 ECM-IDLE（本机 2015 年 MPSS 空闲态
# 收不到网络寻呼，短信会延迟几分钟）。代价约 0.5MB/天。
#
# 自愈：连续 FAIL_LIMIT 次 ping 失败（或接口没有 IPv4）→ 重拨 LTE。
# 连接由 NetworkManager 的 "modem" profile 管理，重拨统一走 nmcli；
# 全程不碰 /dev/wwan0atN（ModemManager 在用，抢了会话短信就丢）。
# 两次恢复尝试之间至少间隔 RECOVER_COOLDOWN 秒，失败重试不打爆日志；
# 恢复/降级都打 WARN 前缀日志，journalctl -u smsrelay-keepalive 可查。
IFACE=wwan0
TARGET=223.5.5.5
INTERVAL=10
FAIL_LIMIT=3
RECOVER_COOLDOWN=300

fails=0
last_recover=0

log() { echo "keepalive: $*" >&2; }

recover() {
    now=$(date +%s)
    if [ $((now - last_recover)) -lt "$RECOVER_COOLDOWN" ]; then
        return 1
    fi
    last_recover=$now
    if ip -4 addr show "$IFACE" 2>/dev/null | grep -q inet; then
        # 有地址但不通 = 会话僵死，强制断开重拨
        log "WARN 连续 $fails 次 ping 不通，强制重拨 LTE"
        nmcli device disconnect wwan0qmi0 >/dev/null 2>&1
        sleep 2
    else
        log "WARN $IFACE 无 IPv4（连续 $fails 次），重拨 LTE"
    fi
    nmcli con up modem >/dev/null 2>&1 || log "WARN nmcli con up modem 失败"
    sleep 15
    if ping -I "$IFACE" -c 1 -W 5 -q "$TARGET" >/dev/null 2>&1; then
        log "LTE 已恢复（重拨成功）"
        return 0
    fi
    log "WARN 重拨后仍不通，$RECOVER_COOLDOWN 秒后自动再试"
    return 1
}

while true; do
    if ip -4 addr show "$IFACE" 2>/dev/null | grep -q inet; then
        if ping -I "$IFACE" -c 1 -W 3 -q "$TARGET" >/dev/null 2>&1; then
            if [ "$fails" -ge "$FAIL_LIMIT" ]; then
                log "LTE 恢复正常（此前连续失败 $fails 次）"
            fi
            fails=0
        else
            fails=$((fails + 1))
            [ "$fails" -ge "$FAIL_LIMIT" ] && recover
        fi
    else
        fails=$((fails + 1))
        [ "$fails" -ge "$FAIL_LIMIT" ] && recover
    fi
    sleep "$INTERVAL"
done
