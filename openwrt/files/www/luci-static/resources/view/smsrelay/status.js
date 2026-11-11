'use strict';
'require view';
'require fs';
'require poll';
'require network';
'require smsrelay';

var CPU = '/sys/devices/system/cpu/';
var POLICY = CPU + 'cpufreq/policy0/';

function read(path) {
	return fs.read(path).then(function(v) { return { value: v.trim() }; }, function() { return { error: true }; });
}
function freq(v) {
	var n = parseInt(v, 10);
	return isNaN(n) ? '—' : n >= 1000000 ? (n / 1000000).toFixed(2) + ' GHz' : Math.round(n / 1000) + ' MHz';
}
function row(label, value) {
	return E('tr', { 'class': 'tr' }, [ E('td', { 'class': 'td', 'style': 'width:140px' }, label), E('td', { 'class': 'td' }, value) ]);
}
function table(title, rows) {
	return E('div', { 'class': 'cbi-section' }, [ E('h3', {}, title), E('table', { 'class': 'table' }, rows) ]);
}

return view.extend({
	handleSave: null,
	handleSaveApply: null,
	handleReset: null,
	load: function() {
		return Promise.all([
			fs.list(CPU).catch(function() { return []; }),
			fs.list('/sys/class/thermal').catch(function() { return []; })
		]);
	},
	hardware: function(data) {
		var cpus = data[0].filter(function(e) { return /^cpu[0-9]+$/.test(e.name); }).sort(function(a, b) {
			return parseInt(a.name.slice(3), 10) - parseInt(b.name.slice(3), 10);
		});
		var zones = data[1].filter(function(e) { return /^thermal_zone[0-9]+$/.test(e.name); });
		return Promise.all([
			Promise.all(cpus.map(function(c) {
				return Promise.all([ read(CPU + c.name + '/online'), read(CPU + c.name + '/cpufreq/scaling_cur_freq') ]).then(function(v) {
					return c.name.toUpperCase() + ' ' + (v[0].value === '0' ? _('离线') : freq(v[1].value));
				});
			})),
			Promise.all(zones.map(function(z) {
				var path = '/sys/class/thermal/' + z.name + '/';
				return Promise.all([ read(path + 'type'), read(path + 'temp') ]);
			})),
			read(POLICY + 'scaling_governor'), read(POLICY + 'scaling_min_freq'), read(POLICY + 'scaling_max_freq')
		]).then(function(v) {
			function temperature(re) {
				var matches = v[1].filter(function(z) { return re.test(z[0].value || ''); });
				var values = matches.map(function(z) { return parseFloat(z[1].value) / 1000; }).filter(function(n) { return isFinite(n) && n > -100 && n < 200; });
				if (!values.length)
					return matches.length || v[1].some(function(z) { return z[0].error; }) ? _('读取失败') : _('暂无传感器');
				var n = Math.max.apply(null, values);
				return E('span', { 'style': n >= 70 ? 'color:#d33' : '' }, n.toFixed(1) + ' °C');
			}
			var lines = [];
			for (var i = 0; i < v[0].length; i += 2)
				lines.push(E('div', {}, v[0].slice(i, i + 2).join(' · ')));
			return table(_('系统监控'), [
				row(_('CPU 频率'), E('div', { 'style': 'font-family:monospace' }, lines.length ? lines : [ _('读取失败') ])),
				row(_('CPU 温度'), temperature(/cpu/i)),
				row(_('基带温度'), temperature(/modem|baseband|mss|q6/i)),
				row(_('调频策略'), (v[2].value || '—') + ' · ' + _('范围') + ' ' +
					(v[3].error || v[4].error ? _('读取失败') : (parseInt(v[3].value, 10) / 1000) + '–' + (parseInt(v[4].value, 10) / 1000) + ' MHz'))
			]);
		});
	},
	service: function() {
		var self = this;
		return Promise.all([
			smsrelay.call('status').catch(function(err) { return { error: err.message }; }),
			network.getNetworks().catch(function() { return []; }),
			smsrelay.call('ddns').catch(function() { return { items: [] }; })
		]).then(function(data) {
			var s = data[0], m = s.modem || {}, c = s.counts || {}, today = s.today || {};
			self.statusPending = m.state === '正在读取';
			var addresses = [];
			data[1].forEach(function(n) {
				if (n.getName() === 'modem' || n.getName() === 'wwan')
					addresses = addresses.concat(n.getIPAddrs(), n.getIP6Addrs());
			});
			return E('div', {}, [
				table(_('模组与网络'), [
					row(_('模组状态'), m.state || '—'), row(_('运营商'), m.operator || '—'),
					row(_('网络制式'), m.access_tech || '—'), row(_('信号质量'), m.signal != null ? m.signal + '%' : '—'),
					row(_('注册状态'), m.registration || '—'), row(_('数据连接'), m.ps_state || '—'),
					row(_('SIM 状态'), m.online ? (m.iccid && m.iccid !== '--' ? _('已识别') : _('未识别或读取中')) : _('模组未就绪')),
					row(_('本机号码'), s.own_number || '—'), row(_('接口 IP'), addresses.join(' · ') || '—')
				]),
				table(_('短信服务'), [
					row(_('服务状态'), s.error || _('运行中')), row(_('版本'), (s.gateway || {}).version || '—'),
					row(_('运行时间'), s.gateway ? '%t'.format(s.gateway.uptime) : '—'),
					row(_('收件 / 发件'), (c.inbox || 0) + ' / ' + (c.sent || 0)),
					row(_('今日收件 / 发件'), (today.inbox || 0) + ' / ' + (today.sent || 0)),
					row(_('待转发 / 失败'), (c.pending || 0) + ' / ' + (c.failed || 0)),
					row(_('最近收信'), s.last_inbound || '—'),
					row(_('数据库'), s.error ? _('后台不可用') : s.database_error || _('正常'))
				]),
				table(_('DDNS'), data[2].items.length ? data[2].items.map(function(r) {
					return row(r.name + ' (' + r.type + ')', (r.last_ip || '—') + ' · ' + (r.last_error || r.reach_note || r.last_update || _('尚未同步')));
				}) : [ row(_('记录'), _('无启用记录或后台不可用')) ])
			]);
		});
	},
	render: function(data) {
		var hardware = E('div'), service = E('div'), tick = 0;
		var root = E('div', {}, [ E('h2', {}, _('短信网关 · 状态')), hardware, service ]);
		var update = function() {
			var jobs = [ this.hardware(data).then(function(node) { hardware.replaceChildren(node); }) ];
			if (tick++ % 6 === 0 || this.statusPending)
				jobs.push(this.service().then(function(node) { service.replaceChildren(node); }));
			return Promise.all(jobs);
		}.bind(this);
		poll.add(function() { if (root.isConnected) return update(); }, 5);
		return update().then(function() { return root; });
	}
});
