'use strict';
'require view';
'require poll';
'require ui';
'require smsrelay';

return view.extend({
	handleSave: null,
	handleSaveApply: null,
	handleReset: null,
	render: function() {
		var output = E('pre', { 'style': 'max-height:65vh;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere' });
		var level = E('select', { 'class': 'cbi-input-select' }, [
			E('option', { 'value': '' }, _('全部级别')), E('option', { 'value': 'info' }, 'INFO'),
			E('option', { 'value': 'warn' }, 'WARN'), E('option', { 'value': 'error' }, 'ERROR')
		]);
		var keyword = E('input', { 'class': 'cbi-input-text', 'placeholder': _('过滤关键词') });
		var auto = E('input', { 'type': 'checkbox', 'checked': '' });
		var follow = E('input', { 'type': 'checkbox', 'checked': '' });
		var entries = [], text = '';
		function display() {
			var q = keyword.value.toLowerCase();
			text = entries.filter(function(e) { return (!level.value || e.level === level.value) && (!q || e.msg.toLowerCase().indexOf(q) >= 0); })
				.map(function(e) { return e.ts + ' [' + e.level.toUpperCase() + '] ' + e.msg; }).join('\n');
			output.textContent = text || _('暂无匹配日志');
			if (follow.checked) output.scrollTop = output.scrollHeight;
		}
		function refresh() {
			return smsrelay.call('logs', 500, '').then(function(data) { entries = data.items || []; display(); }, function(err) {
				output.textContent = err.message;
			});
		}
		level.addEventListener('change', display);
		keyword.addEventListener('input', display);
		var root = E('div', {}, [
			E('h2', {}, _('短信网关 · 日志')),
			E('p', {}, _('最近 500 条运行日志，仅保存在内存中，进程重启后清空。启动与崩溃诊断可在系统日志中查看。')),
			E('div', { 'class': 'cbi-section', 'style': 'display:flex;gap:12px;flex-wrap:wrap;align-items:center' }, [
				level, keyword, E('label', {}, [ auto, ' ' + _('自动刷新') ]), E('label', {}, [ follow, ' ' + _('跟随末尾') ]),
				E('button', { 'class': 'btn', 'click': ui.createHandlerFn(this, refresh) }, _('刷新')),
				E('button', { 'class': 'btn', 'click': function() { return smsrelay.copy(text); } }, _('复制')),
				E('button', { 'class': 'btn', 'click': function() {
					var url = URL.createObjectURL(new Blob([ text ], { type: 'text/plain;charset=utf-8' }));
					var link = E('a', { 'href': url, 'download': 'smsrelay.log' });
					document.body.appendChild(link); link.click(); link.remove();
					window.setTimeout(function() { URL.revokeObjectURL(url); }, 1000);
				} }, _('下载'))
			]), output
		]);
		poll.add(function() { if (root.isConnected && auto.checked) return refresh(); }, 3);
		return refresh().then(function() { return root; });
	}
});
