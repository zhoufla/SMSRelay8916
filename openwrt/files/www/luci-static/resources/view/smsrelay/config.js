'use strict';
'require view';
'require form';
'require uci';
'require fs';
'require ui';
'require smsrelay';

/*
 * smsrelay 配置页（原生 LuCI 表单）
 *
 * 只做一件事：编辑 /etc/config/smsrelay。
 * 表单直接绑定 uci 的 smsrelay 配置，保存后由 rpcd 写盘并 commit，
 * procd 的 reload trigger 会重启服务 —— 不需要任何自定义 RPC。
 *
 * 段名即主键：channel/ddns 段由 LuCI 自动生成（cfg0xxxx），
 * 程序用它映射成稳定的 channel_id，投递历史不会因为改名而错位。
 */

return view.extend({
	load: function() {
		return Promise.all([
			uci.load('smsrelay'), uci.load('jz02'),
			fs.read('/sys/devices/system/cpu/cpufreq/policy0/scaling_available_frequencies').catch(function() { return ''; }),
			fs.read('/sys/devices/system/cpu/cpufreq/policy0/scaling_available_governors').catch(function() { return ''; }),
			fs.read('/sys/class/leds/green:wlan/brightness').then(function() { return true; }).catch(function() { return false; })
		]);
	},

	render: function(data) {
		var m = new form.Map('smsrelay', _('短信网关'),
			_('短信历史保存在 SQLite，收发及转发不依赖浏览器。短信设置保存并应用后会重启后台；发送中请等待完成再修改。性能与指示灯设置独立应用。'));

		/* ---------------- 基本设置 ---------------- */
		var s = m.section(form.NamedSection, 'main', 'smsrelay', _('基本设置'));
		s.anonymous = true;

		var o = s.option(form.Flag, 'enabled', _('启用服务'));
		o.rmempty = false;

		o = s.option(form.Flag, 'keepalive', _('LTE 保活'),
			_('周期 ICMP，避免空闲态收不到寻呼导致短信延迟'));
		o.default = '1';
		// 勾选值等于 default 时 LuCI 默认会 remove 掉该 option（rmempty）；
		// 另外未显式配置时 cfgvalue 也回落到 default，与提交值相等会被判成
		//「无变化」而跳过写入。两处都要关掉，勾选状态才会真正落盘。
		o.rmempty = false;
		o.forcewrite = true;

		o = s.option(form.Value, 'own_number', _('本机号码'),
			_('留空则跟随 SIM 卡；很多卡不写号码，转发内容里会缺「收信号码」'));
		o.rmempty = true;

		o = s.option(form.Value, 'retain_days', _('短信保留天数'),
			_('0 表示不自动清理'));
		o.datatype = 'uinteger';
		o.placeholder = '30';

		o = s.option(form.Value, 'guard_interval', _('兜底扫描间隔（秒）'),
			_('漏掉 D-Bus 信号时，最坏多久能补收'));
		o.datatype = 'min(1)';
		o.placeholder = '60';

		o = s.option(form.Value, 'modem_index', _('Modem 编号'));
		o.placeholder = '0';
		o.datatype = 'uinteger';

		o = s.option(form.Value, 'bands', _('模组频段'),
			_('可选。留空 = 不改动模组默认频段。竖线或逗号分隔 mmcli 频段名，' +
			  '例如 eutran-1|eutran-3|eutran-8|eutran-39|eutran-40。' +
			  '模组默认只开 eutran-1/3/5，到了移动 band 8/39/40 覆盖的区域会' +
			  '「手机有信号、模组扫不到」，此时把频段放开即可；建议直接填全部支持的频段。'));
		o.placeholder = 'egsm|dcs|pcs|g850|utran-1|utran-2|utran-3|utran-5|utran-8|' +
			'eutran-1|eutran-2|eutran-3|eutran-5|eutran-7|eutran-8|eutran-20|eutran-39|eutran-40';
		o.rmempty = true;

		o = s.option(form.Value, 'operator', _('锁定运营商（PLMN）'),
			_('可选。留空 = 自动选网。填本网 MCCMNC：移动 46000、联通 46001、电信 46011。' +
			  '模组卡在别家网络（如 CHN-CT）或一直 searching 时填上，' +
			  '保活脚本会在基带就绪后强制注册回本网；已在本网则不做任何动作。'));
		o.placeholder = '46000';
		o.datatype = 'uinteger';
		o.rmempty = true;

		o = s.option(form.Value, 'db_path', _('归档库路径'),
			_('默认 /etc/smsrelay/sms.db。升级保留配置时创建一致性备份；不要放 /tmp 或 /var。更改路径不会自动搬迁旧库，请先停服务迁移。'));
		o.placeholder = '/etc/smsrelay/sms.db';
		o.validate = function(sid, value) {
			return /^\/[A-Za-z0-9_./-]+$/.test(value) && value.indexOf('//') < 0 && !/\/\.{1,2}(\/|$)/.test(value) && value !== '/etc/smsrelay/upgrade.db' && !/^\/(tmp|var|proc|sys|dev)(\/|$)/.test(value)
				? true : _('请输入持久存储上的绝对路径，不允许空格、上级目录或临时目录');
		};
		o = s.option(form.Button, '_cleanup', _('清理过期短信'));
		o.inputtitle = _('立即清理');
		o.onclick = function() {
			ui.showModal(_('清理过期短信'), [ E('p', {}, _('按已保存的保留天数清理，0 表示不清理。确认继续？')),
				E('div', { 'class': 'right' }, [ E('button', { 'class': 'btn', 'click': ui.hideModal }, _('取消')), ' ',
					E('button', { 'class': 'btn cbi-button-negative', 'click': ui.createHandlerFn(this, function() {
						return smsrelay.call('cleanup').then(function(r) {
							ui.hideModal(); ui.addNotification(null, E('p', {}, _('已删除 %d 条记录').format(r.deleted)), 'info');
						}).catch(smsrelay.error);
					}) }, _('确认')) ]) ]);
		};

		/* ---------------- 转发通道 ---------------- */
		var cs = m.section(form.GridSection, 'channel', _('转发通道'),
			_('全部条件留空表示接收所有短信；多个条件之间是「且」的关系。'));
		cs.addremove = true;
		cs.anonymous = true;
		cs.sortable = true;
		cs.sectiontitle = function(sid) {
			return uci.get('smsrelay', sid, 'name') || _('未命名通道');
		};

		o = cs.option(form.Flag, 'enabled', _('启用'));
		o.default = '1';
		o.editable = true;

		o = cs.option(form.Value, 'name', _('名称'));
		o.rmempty = false;

		o = cs.option(form.ListValue, 'type', _('类型'));
		o.value('webhook', 'Webhook');
		o.value('dingtalk', _('钉钉机器人'));
		o.default = 'webhook';

		o = cs.option(form.Value, 'url', _('地址'));
		o.rmempty = false;
		o.placeholder = 'https://example.com/hook';

		o = cs.option(form.Value, 'secret', _('加签密钥'),
			_('仅钉钉「加签」模式需要，SEC 开头'));
		o.password = true;
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.Value, 'prefix', _('正文前缀'));
		o.placeholder = '【短信】';
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.ListValue, 'method', _('请求方法'));
		o.value('POST');
		o.value('PUT');
		o.default = 'POST';
		o.modalonly = true;

		o = cs.option(form.DynamicList, 'header', _('请求头'),
			_('每行一个，格式 K: V'));
		o.placeholder = 'Content-Type: application/json';
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.DynamicList, 'body_line', _('请求体模板'),
			_('每行一段，程序用换行拼回去；%s 是短信正文'));
		o.placeholder = '{"text":"%s"}';
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.Value, 'match_peer', _('号码前缀'),
			_('逗号分隔，任一命中即可'));
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.Value, 'match_keyword', _('命中关键词'),
			_('逗号分隔，任一命中即可'));
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.Value, 'match_skip_keyword', _('排除关键词'),
			_('任一命中则跳过本通道，优先级高于命中关键词'));
		o.modalonly = true;
		o.rmempty = true;

		o = cs.option(form.Flag, 'match_code_only', _('仅验证码短信'));
		o.modalonly = true;
		o = cs.option(form.Button, '_test', _('连通性测试'));
		o.inputtitle = _('测试已保存配置');
		o.onclick = function(ev, sid) {
			return smsrelay.run('test_channel', sid).then(function() {
				ui.addNotification(null, E('p', {}, _('测试成功')), 'info');
			}).catch(smsrelay.error);
		};

		/* ---------------- DDNS ---------------- */
		var ds = m.section(form.GridSection, 'ddns', _('DDNS'),
			_('同步到 Cloudflare。记录类型 A/AAAA 各自一条。'));
		ds.addremove = true;
		ds.anonymous = true;
		ds.sortable = true;
		ds.sectiontitle = function(sid) {
			var name = uci.get('smsrelay', sid, 'name');
			var type = uci.get('smsrelay', sid, 'type');
			return (name || _('未命名记录')) + (type ? ' · ' + type : '');
		};

		o = ds.option(form.Flag, 'enabled', _('启用'));
		o.default = '1';
		o.editable = true;

		o = ds.option(form.Value, 'name', _('域名'));
		o.rmempty = false;
		o.placeholder = 'home.example.com';

		o = ds.option(form.ListValue, 'type', _('记录类型'));
		o.value('A', 'A');
		o.value('AAAA', 'AAAA');
		o.default = 'A';

		o = ds.option(form.Value, 'source', _('地址来源'),
			_('iface:wwan0 取本机网卡，external 走公网查询'));
		o.placeholder = 'external';
		o.rmempty = false;

		o = ds.option(form.Value, 'api_token', _('Cloudflare Token'));
		o.password = true;
		o.rmempty = false;
		o.modalonly = true;

		o = ds.option(form.Value, 'zone_id', _('Zone ID'),
			_('留空则按域名自动查询'));
		o.modalonly = true;
		o.rmempty = true;

		o = ds.option(form.Flag, 'proxied', _('走 Cloudflare 代理'));
		o.modalonly = true;

		o = ds.option(form.Value, 'ttl', _('TTL'),
			_('1 表示自动'));
		o.datatype = 'uinteger';
		o.placeholder = '1';
		o.modalonly = true;

		o = ds.option(form.Button, '_sync', _('手动同步'));
		o.inputtitle = _('同步已保存配置');
		o.onclick = function(ev, sid) {
			return smsrelay.run('sync_ddns', sid).then(function(r) {
				ui.addNotification(null, E('p', {}, r.message || _('同步完成')), 'info');
			}).catch(smsrelay.error);
		};

		var maps = [ m ];
		if (data[2].trim() || data[4]) {
			var perf = new form.Map('jz02', _('性能与指示灯'), _('设置独立保存在 /etc/config/jz02；修改不会重启短信服务。'));
			var ps = perf.section(form.NamedSection, 'system', 'jz02');
			ps.anonymous = true;
			if (data[2].trim()) {
				o = ps.option(form.ListValue, 'cpu_governor', _('调频策略'));
				data[3].trim().split(/\s+/).filter(Boolean).forEach(function(g) { o.value(g); });
				o.rmempty = false;
				o = ps.option(form.ListValue, 'cpu_max_freq', _('最高频率'));
				data[2].trim().split(/\s+/).filter(Boolean).forEach(function(f) { o.value(f, (parseInt(f, 10) / 1000) + ' MHz'); });
				o.rmempty = false;
			}
			if (data[4]) {
				o = ps.option(form.Flag, 'led_wifi', _('绿灯显示 Wi-Fi 活动'));
				o.default = '0'; o.rmempty = false;
			}
			maps.push(perf);
		}
		return Promise.all(maps.map(function(map) { return map.render(); })).then(function(nodes) { return E('div', {}, nodes); });
	}
});
