'use strict';
'require view';
'require ui';
'require poll';
'require smsrelay';

return view.extend({
	handleSave: null,
	handleSaveApply: null,
	handleReset: null,
	render: function() {
		var dir = 'in', offset = 0, limit = 30, selected = {}, items = [], pending = false;
		var writable = L.hasViewPermission();
		var search = E('input', { 'class': 'cbi-input-text', 'placeholder': _('搜索号码或正文') });
		var code = E('input', { 'type': 'checkbox' });
		var body = E('div'), page = E('span'), error = E('p', { 'style': 'color:#d33' });
		var prev = E('button', { 'class': 'btn', 'click': function() { offset = Math.max(0, offset - limit); return refresh(); } }, _('上一页'));
		var next = E('button', { 'class': 'btn', 'click': function() { offset += limit; return refresh(); } }, _('下一页'));
		function requestID() {
			var bytes = new Uint8Array(16); window.crypto.getRandomValues(bytes);
			return Array.from(bytes).map(function(n) { return ('0' + n.toString(16)).slice(-2); }).join('');
		}
		function compose(number, text) {
			var rid = requestID(), numberInput = E('input', { 'class': 'cbi-input-text', 'value': number || '', 'placeholder': _('接收号码') });
			var textInput = E('textarea', { 'class': 'cbi-input-textarea', 'rows': 6, 'maxlength': 1000 }, text || '');
			var estimate = E('p'), submit;
			function count() {
				var value = textInput.value, chars = Array.from(value).length;
				// 与后台一致的保守分段规则；最终结果由后台确定。
				var gsm = '@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !"#¤%&\'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà';
				var isGsm = Array.from(value).every(function(c) { return gsm.indexOf(c) >= 0; });
				var max = isGsm ? 160 : 70, units = isGsm ? chars : value.length;
				var parts = units <= max ? (units ? 1 : 0) : Math.ceil(units / (max - 8));
				estimate.textContent = _('%d 字，预计 %d 条；长短信分段发送，最终以后台处理为准。').format(chars, parts);
			}
			textInput.addEventListener('input', count); count();
			submit = E('button', { 'class': 'btn cbi-button-action', 'click': function() {
				if (!numberInput.value.trim() || !textInput.value.trim()) return;
				submit.disabled = true; numberInput.disabled = true; textInput.disabled = true;
				return smsrelay.call('send', rid, numberInput.value, textInput.value).then(function(data) {
					ui.hideModal(); dir = 'out'; offset = 0; box.value = dir;
					ui.addNotification(null, E('p', {}, _('已加入发送队列，任务 #%d。请在发件箱查看结果。').format(data.id)), 'info');
					return refresh();
				}).catch(function(err) {
					// 保留原请求标识及内容，网络超时后再次提交不会产生重复任务。
					submit.disabled = false;
					if (err.business) { numberInput.disabled = false; textInput.disabled = false; }
					smsrelay.error(err);
				});
			} }, _('发送'));
			ui.showModal(_('写短信'), [ numberInput, textInput, estimate,
				E('p', {}, _('发送可能产生运营商费用。结果待确认的短信请先核实，不要直接重发。')),
				E('div', { 'class': 'right' }, [ E('button', { 'class': 'btn', 'click': ui.hideModal }, _('关闭')), ' ', submit ])
			]);
		}
		function detail(m) {
			ui.showModal(_('短信详情'), [
				E('p', {}, m.peer + ' · ' + m.ts + ' · ' + smsrelay.state(m.state)),
				E('pre', { 'style': 'white-space:pre-wrap;overflow-wrap:anywhere' }, m.body),
				E('p', {}, m.forward_error || ''),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', 'click': function() { return smsrelay.copy(m.body); } }, _('复制正文')), ' ',
					E('button', { 'class': 'btn', 'click': ui.hideModal }, _('关闭'))
				])
			]);
		}
		function remove(ids) {
			ui.showModal(_('删除短信'), [ E('p', {}, _('确认删除所选 %d 条记录？排队中或发送中的记录不会删除。').format(ids.length)),
				E('div', { 'class': 'right' }, [ E('button', { 'class': 'btn', 'click': ui.hideModal }, _('取消')), ' ',
					E('button', { 'class': 'btn cbi-button-negative', 'click': ui.createHandlerFn(this, function() {
						return smsrelay.call('delete', ids).then(function() { ui.hideModal(); return refresh(); }).catch(smsrelay.error);
					}) }, _('删除')) ]) ]);
		}
		function refresh() {
			if (pending) return Promise.resolve();
			pending = true;
			return smsrelay.call('messages', dir, search.value, dir === 'in' && code.checked, limit, offset).then(function(data) {
				error.textContent = ''; selected = {}; items = data.items || [];
				var rows = [ E('tr', { 'class': 'tr table-titles' }, [
					E('th', { 'class': 'th' }, E('input', { 'type': 'checkbox', 'disabled': !writable, 'change': function(ev) {
						body.querySelectorAll('input[data-id]').forEach(function(el) { el.checked = ev.target.checked; selected[el.dataset.id] = el.checked; });
					} })),
					E('th', { 'class': 'th' }, _('时间')), E('th', { 'class': 'th' }, _('号码')),
					E('th', { 'class': 'th' }, _('正文')), E('th', { 'class': 'th' }, _('状态')), E('th', { 'class': 'th' }, _('操作'))
				]) ];
				items.forEach(function(m) {
					var actions = [ E('button', { 'class': 'btn', 'click': function() { detail(m); } }, _('详情')) ];
					if (m.code) actions.push(E('button', { 'class': 'btn', 'click': function() { return smsrelay.copy(m.code); } }, _('复制验证码')));
					if (writable && dir === 'in' && m.forwarded !== 1) actions.push(E('button', { 'class': 'btn', 'click': ui.createHandlerFn(this, function() {
						return smsrelay.call('forward', m.id).then(refresh).catch(smsrelay.error);
					}) }, _('重试转发')));
					if (writable && dir === 'out' && m.state === 'failed') actions.push(E('button', { 'class': 'btn', 'click': function() { compose(m.peer, m.body); } }, _('重新编辑发送')));
					if (writable) actions.push(E('button', { 'class': 'btn cbi-button-negative', 'click': function() { remove([ m.id ]); } }, _('删除')));
					rows.push(E('tr', { 'class': 'tr' }, [
						E('td', { 'class': 'td' }, E('input', { 'type': 'checkbox', 'data-id': m.id, 'disabled': !writable, 'change': function(ev) { selected[m.id] = ev.target.checked; } })),
						E('td', { 'class': 'td' }, m.ts), E('td', { 'class': 'td' }, m.peer),
						E('td', { 'class': 'td', 'style': 'white-space:pre-wrap;overflow-wrap:anywhere;max-width:32em' }, m.body),
						E('td', { 'class': 'td' }, dir === 'in' ? [ _('待转发'), _('已处理'), _('转发失败') ][m.forwarded] || '—' : smsrelay.state(m.state)),
						E('td', { 'class': 'td', 'style': 'white-space:normal' }, actions)
					]));
				});
				if (!items.length) rows.push(E('tr', { 'class': 'tr' }, E('td', { 'class': 'td', 'colspan': 6 }, _('暂无记录'))));
				body.replaceChildren(E('table', { 'class': 'table' }, rows));
				page.textContent = _('共 %d 条 · 第 %d 页').format(data.total || 0, Math.floor(offset / limit) + 1);
				prev.disabled = offset === 0; next.disabled = offset + limit >= data.total;
			}).catch(function(err) { error.textContent = err.message; }).finally(function() { pending = false; });
		}
		var box = E('select', { 'class': 'cbi-input-select', 'change': function(ev) { dir = ev.target.value; offset = 0; return refresh(); } }, [
			E('option', { 'value': 'in' }, _('收件箱')), E('option', { 'value': 'out' }, _('发件箱'))
		]);
		var root = E('div', {}, [ E('h2', {}, _('短信网关 · 短信记录')),
			E('div', { 'class': 'cbi-section', 'style': 'display:flex;gap:10px;align-items:center;flex-wrap:wrap' }, [
				box, search, E('label', {}, [ code, ' ' + _('仅验证码') ]),
				E('button', { 'class': 'btn', 'click': function() { offset = 0; return refresh(); } }, _('查询 / 刷新')),
				E('button', { 'class': 'btn cbi-button-action', 'disabled': !writable, 'click': function() { compose(); } }, _('写短信')),
				E('button', { 'class': 'btn cbi-button-negative', 'disabled': !writable, 'click': function() {
					var ids = Object.keys(selected).filter(function(id) { return selected[id]; }).map(Number);
					if (ids.length) remove(ids);
				} }, _('删除所选'))
			]), error, body, E('div', { 'class': 'cbi-section', 'style': 'display:flex;gap:12px;align-items:center' }, [ prev, page, next ])
		]);
		poll.add(function() {
			if (root.isConnected && !document.body.classList.contains('modal-overlay-active') && !Object.keys(selected).some(function(id) { return selected[id]; })) return refresh();
		}, 10);
		return refresh().then(function() { return root; });
	}
});
