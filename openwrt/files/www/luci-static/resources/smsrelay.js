'use strict';
'require rpc';
'require ui';

var methods = {
	status: [], messages: [ 'dir', 'q', 'has_code', 'limit', 'offset' ],
	message: [ 'id' ], delete: [ 'ids' ], send: [ 'request_id', 'number', 'text' ],
	send_task: [ 'id' ], forward: [ 'id' ], logs: [ 'limit', 'level' ], cleanup: [],
	channels: [], test_channel: [ 'section' ], ddns: [], sync_ddns: [ 'section' ], operation: [ 'operation' ]
};
var calls = {};
Object.keys(methods).forEach(function(name) {
	calls[name] = rpc.declare({ object: 'smsrelay', method: name, params: methods[name] });
});

return L.Class.extend({
	call: function(name) {
		return calls[name].apply(null, Array.prototype.slice.call(arguments, 1)).then(function(reply) {
			if (!reply || !reply.ok) {
				var err = new Error((reply && reply.error) || _('短信后台不可用'));
				err.business = !!reply;
				throw err;
			}
			return reply.data || {};
		});
	},
	error: function(err) {
		ui.addNotification(null, E('p', {}, err.message || String(err)), 'error');
	},
	run: function() {
		var self = this;
		return this.call.apply(this, arguments).then(function(task) {
			var tries = 0;
			function check() {
				return self.call('operation', task.operation).then(function(result) {
					if (result.error) throw new Error(result.error);
					if (result.done) return result;
					if (++tries > 90) throw new Error(_('操作仍在执行，请稍后查看日志'));
					return new Promise(function(resolve) { window.setTimeout(resolve, 2000); }).then(check);
				});
			}
			return check();
		});
	},
	copy: function(text) {
		if (navigator.clipboard && window.isSecureContext)
			return navigator.clipboard.writeText(text).catch(this.error);
		ui.showModal(_('复制内容'), [
			E('textarea', { 'class': 'cbi-input-textarea', 'readonly': '', 'rows': 8 }, text),
			E('div', { 'class': 'right' }, E('button', { 'class': 'btn', 'click': ui.hideModal }, _('关闭')))
		]);
		return Promise.resolve();
	},
	state: function(state) {
		return ({ queued: _('排队中'), sending: _('发送中'), sent: _('已发送'), delivered: _('已送达'),
			failed: _('失败'), unknown: _('结果待确认'), received: _('已接收') })[state] || state || '—';
	}
});
