'use strict';
'require view';
'require rpc';
'require ui';

/* procd 服务实例状态（gateway / keepalive 两个实例的 running） */
var callServiceList = rpc.declare({
	object: 'service',
	method: 'list',
	params: [ 'name' ]
});

var callInitAction = rpc.declare({
	object: 'rc',
	method: 'init',
	params: [ 'name', 'action' ]
});

/* iframe 直接嵌 SMSRelay 自带的前端（:8088）。
 * src 用 location.hostname 动态拼——从哪个地址进 LuCI，就嵌哪个主机的 8088，
 * USB 侧 / 局域网侧 / 以后 DDNS 域名都自动正确。
 * iframe 内部的 fetch/SSE 都是相对路径（对 8088 自身），不存在跨源问题。 */
return view.extend({
	load: function() {
		return callServiceList('smsrelay').catch(function() { return {}; });
	},

	isRunning: function(data) {
		var inst = (data && data.smsrelay && data.smsrelay.instances) || {};
		for (var k in inst)
			if (inst[k].running)
				return true;
		return false;
	},

	handleRestart: function(ev) {
		return callInitAction('smsrelay', 'restart')
			.then(function() {
				ui.addNotification(null, E('p', _('正在重启，几秒后刷新本页')), 'info');
				window.setTimeout(function() { window.location.reload(); }, 4000);
			})
			.catch(function(e) { ui.addNotification(null, E('p', _('重启失败: %s').format(e.message)), 'error'); });
	},

	render: function(data) {
		var running = this.isRunning(data);
		var url = 'http://%s:8088/'.format(window.location.hostname);

		var header = E('div', { 'style': 'display:flex;align-items:center;gap:12px;margin-bottom:10px' }, [
			E('span', {
				'class': 'label',
				'style': 'background:%s;color:#fff;padding:3px 10px;border-radius:4px'.format(running ? '#2eb872' : '#c44')
			}, running ? _('运行中') : _('未运行')),
			E('a', {
				'href': url,
				'target': '_blank',
				'style': 'color:#37c;font-size:13px'
			}, _('在新标签页打开 ↗')),
			E('button', {
				'class': 'btn cbi-button cbi-button-neutral',
				'click': ui.createHandlerFn(this, 'handleRestart'),
				'style': 'margin-left:auto'
			}, _('重启服务'))
		]);

		var frame = E('iframe', {
			'src': url,
			'id': 'smsrelay-frame',
			'style': 'width:100%;height:calc(100vh - 170px);min-height:480px;border:1px solid #ccc;border-radius:6px;display:block;background:#0f1115;'
		});

		var hint = E('div', { 'style': 'color:#888;font-size:12px;margin-top:6px' },
			_('页面由 smsrelay 服务（端口 8088）提供，嵌入显示在此。若长时间空白，请检查服务状态或直接用上面的链接打开。'));

		return E('div', {}, [ header, frame, hint ]);
	}
});
