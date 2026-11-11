# 由 sysupgrade 在生成备份清单后调用；VACUUM INTO 提供一致性在线快照。
smsrelay_backup() {
	local list="$1" db was_running=0 pid i=0
	db="$(uci -q get smsrelay.main.db_path)"
	[ -n "$db" ] || db=/etc/smsrelay/sms.db
	[ -f "$db" ] || return 0
	if [ "${CONF_BACKUP_LIST:-0}" != 1 ]; then
		# 真正刷写时先停收信，避免快照后又归档并删除模组副本而未进入备份。
		# 单纯 sysupgrade -b 保持在线，备份语义是调用瞬间的一致性快照。
		if [ -z "$CONF_BACKUP" ]; then
			/etc/init.d/smsrelay running >/dev/null 2>&1 && was_running=1
			pid="$(ubus call service list '{"name":"smsrelay"}' 2>/dev/null | jsonfilter -e '@.smsrelay.instances.gateway.pid' 2>/dev/null)"
			/etc/init.d/smsrelay stop
			case "$pid" in
				''|*[!0-9]*) ;;
				*)
					while kill -0 "$pid" 2>/dev/null; do
						[ "$i" -lt 10 ] || { echo 'SMSRelay: 后台未及时退出，停止升级备份' >&2; exit 1; }
						sleep 1
						i=$((i + 1))
					done
					;;
			esac
		fi
		/usr/bin/smsrelay backup || {
			[ "$was_running" = 1 ] && /etc/init.d/smsrelay start
			echo "SMSRelay: 短信快照失败，停止备份以避免静默丢失历史记录" >&2
			exit 1
		}
	fi
	# 排除活动库及 WAL/SHM，只打包快照。awk 使用精确字符串比较，不把路径当正则。
	awk -v db="$db" '$0 != db && $0 != db "-wal" && $0 != db "-shm" && $0 != "/etc/smsrelay/upgrade.db"' "$list" > "$list.smsrelay"
	cat "$list.smsrelay" > "$list"
	rm -f "$list.smsrelay"
	echo /etc/smsrelay/upgrade.db >> "$list"
}

[ -n "$sysupgrade_init_conffiles" ] && append sysupgrade_init_conffiles smsrelay_backup
