#!/bin/sh
# 轻量保活：周期性发一个 ICMP 包，让 LTE 连接别长时间停在 ECM-IDLE。
#
# 本机 MSM8916 模组（2015 年 MPSS）空闲态收不到网络寻呼，短信中心只能反复
# 重试，直到设备因其它原因重新附着才补投 —— 表现为"短信要等好几分钟"。
# 代价约 0.5MB/天。若换固件/换机型，可先停掉本服务观察。
IFACE=wwan0
TARGET=223.5.5.5
INTERVAL=10

while true; do
    if ip -4 addr show "$IFACE" 2>/dev/null | grep -q inet; then
        ping -I "$IFACE" -c 1 -W 2 -q "$TARGET" >/dev/null 2>&1 || true
    fi
    sleep "$INTERVAL"
done
