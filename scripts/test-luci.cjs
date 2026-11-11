// 独立浏览器冒烟测试：使用实际页面模块与模拟 LuCI/RPC，不运行前端构建。
// PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-luci.cjs
// 截图是模拟数据预览，不是真机验证。
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');

async function main() {
	const root = path.join(__dirname, '../openwrt/files/www/luci-static/resources');
	const sources = { shared: fs.readFileSync(path.join(root, 'smsrelay.js'), 'utf8') };
	for (const name of ['config', 'status', 'messages', 'logs'])
		sources[name] = fs.readFileSync(path.join(root, 'view/smsrelay', name + '.js'), 'utf8');
	const browser = await chromium.launch({ headless: true });
	try {
		const page = await browser.newPage({ viewport: { width: 1080, height: 1000 } });
		const errors = [];
		page.on('pageerror', err => errors.push(err.message));
		await page.setContent(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><style>
		body{font:14px Arial,sans-serif;color:#444;background:#f5f5f5;margin:0}header{background:#233647;color:white;padding:18px 30px;font-size:20px}
		main{max-width:980px;margin:auto;padding:20px}nav{display:flex;gap:24px;margin-bottom:20px;border-bottom:1px solid #ddd;padding:12px 0}
		h2{font-size:22px}h3{font-size:16px;font-weight:normal;margin:0 0 12px}.cbi-section{background:white;border:1px solid #ddd;border-radius:7px;padding:16px;margin:16px 0}
		.table{width:100%;border-collapse:collapse}.td,.th{padding:9px 8px;text-align:left;vertical-align:top;border-bottom:1px solid #eee}.tr:last-child .td{border:0}
		.btn{padding:7px 12px;border:1px solid #ccc;background:white;border-radius:4px;cursor:pointer;margin:2px}.btn:disabled{opacity:.5}
		input,select,textarea{padding:7px;border:1px solid #ccc;border-radius:4px}textarea{display:block;width:95%;margin:14px 0}.hint{color:#777;font-size:12px}
		#modal{position:fixed;inset:15% 15%;padding:24px;background:white;box-shadow:0 0 0 100vmax #0007;overflow:auto;z-index:10}.right{text-align:right}
		pre{background:#fafafa;border:1px solid #ddd;padding:14px}.field{margin:12px 0;display:flex;gap:18px;align-items:center}.field label{width:160px}
		@media(max-width:600px){main{padding:10px}#modal{inset:8% 4%}.td,.th{padding:6px 3px}.cbi-section{overflow:auto}}
		</style><header>LuCI · 短信网关</header><main><nav>设置　　状态　　短信记录　　日志</nav>
		<p class="hint">离线预览 · 模拟设备数据（非真机，主题样式为测试替身）</p><div id="app"></div><div id="notifications"></div></main></html>`);
		await page.evaluate(sources => {
			window._ = text => text;
			String.prototype.format = function(...values) { let i = 0; return this.replace(/%[dst]/g, () => String(values[i++])); };
		// 与真实 LuCI dom.attr() 保持一致：函数注册为事件，其余值（含布尔 false）
		// 一律 setAttribute。布尔属性 disabled="false" 依然会禁用元素，视图必须用
		// null 跳过；之前这里用 property 赋值掩盖了这个差异，导致真机按钮失效。
		window.E = function(tag, attrs, children) {
			const el = document.createElement(tag);
			for (const [key, value] of Object.entries(attrs || {})) {
				if (value == null) continue;
				if (typeof value === 'function') el.addEventListener(key, value);
				else if (typeof value === 'object') el.setAttribute(key, JSON.stringify(value));
				else el.setAttribute(key, value);
			}
				const append = child => {
					if (Array.isArray(child)) child.forEach(append);
					else if (child instanceof Node) el.appendChild(child);
					else if (child != null) el.appendChild(document.createTextNode(String(child)));
				};
				append(children); return el;
			};
			window.mock = { polls: [], readonly: false, offline: false, sendCalls: 0, notifications: [],
				inbox: [{ id: 1, dir: 'in', peer: '10086', body: '验证码 123456\n<img src=x onerror="window.XSS=1">', code: '123456', ts: '2026-11-11 12:00:00', state: 'received', forwarded: 1 }],
				outbox: [{ id: 2, dir: 'out', peer: '10010', body: 'cxye', ts: '2026-11-11 11:00:00', state: 'unknown', forward_error: '发送结果待确认，请先核实' }]
			};
			const ui = {
				addNotification: (_, node) => { mock.notifications.push(node.textContent); },
				hideModal: () => { document.querySelector('#modal')?.remove(); document.body.classList.remove('modal-overlay-active'); },
				showModal: (title, nodes) => { ui.hideModal(); document.body.classList.add('modal-overlay-active'); document.body.appendChild(E('section', { id: 'modal' }, [ E('h3', {}, title), nodes ])); },
				createHandlerFn: (ctx, fn) => async function(...args) { this.disabled = true; try { return await fn.apply(ctx, args); } finally { this.disabled = false; } }
			};
			const rpc = { declare: options => async (...args) => {
				const p = Object.fromEntries(options.params.map((key, i) => [key, args[i]]));
				let data = {};
				switch (options.method) {
				case 'status':
					if (mock.offline) return { ok: false, error: '短信后台未运行' };
					data = { modem: { online: true, state: 'connected', access_tech: 'LTE', operator: '示例运营商', signal: 76, registration: 'home', ps_state: 'attached', iccid: 'test' },
						own_number: '未配置', counts: { inbox: 12, sent: 3, pending: 0, failed: 0 }, today: { inbox: 2, sent: 1 }, gateway: { version: '0.2.0-preview', uptime: 3600 }, last_inbound: '2026-11-11 12:00:00' }; break;
				case 'messages': { let items = p.dir === 'out' ? mock.outbox : mock.inbox; items = items.filter(m => (!p.q || m.body.includes(p.q) || m.peer.includes(p.q)) && (!p.has_code || m.code)); data = { items: items.slice(p.offset, p.offset + p.limit), total: items.length }; break; }
				case 'logs': data = { items: [ { ts: '12:00:00', level: 'info', msg: '短信已归档' }, { ts: '12:00:01', level: 'warn', msg: '转发失败 [URL]' }, { ts: '12:00:02', level: 'error', msg: '存储异常' } ] }; break;
				case 'ddns': data = { items: [] }; break;
				case 'send': mock.sendCalls++; mock.outbox.unshift({ id: 3, peer: p.number, body: p.text, state: 'queued', ts: '2026-11-11 12:01:00' }); data = { id: 1 }; break;
				case 'delete': mock.inbox = mock.inbox.filter(m => !p.ids.includes(m.id)); mock.outbox = mock.outbox.filter(m => !p.ids.includes(m.id)); data = { deleted: p.ids.length }; break;
				case 'test_channel': case 'sync_ddns': data = { operation: 'op-1' }; break;
				case 'operation': data = { done: true, message: '操作成功' }; break;
				}
				return { ok: true, data };
			} };
			const fs = {
				list: async path => path.includes('thermal') ? [ { name: 'thermal_zone0' } ] : [0,1,2,3].map(n => ({ name: 'cpu' + n })),
				read: async path => {
					if (path.endsWith('/type')) return 'pm8916_thermal';
					if (path.endsWith('/temp')) return '42000';
					if (path.endsWith('scaling_available_frequencies')) return '200000 400000 800000 998400 1209600 1401600';
					if (path.endsWith('scaling_available_governors')) return 'schedutil powersave performance';
					if (path.endsWith('scaling_governor')) return 'schedutil';
					if (path.endsWith('scaling_min_freq')) return '200000';
					if (path.endsWith('scaling_max_freq')) return '1401600';
					return '1209600';
				}
			};
			const uci = { load: async () => ({}), get: (_, __, field) => ({ name: '示例', type: 'A' })[field] };
			const poll = { add: fn => mock.polls.push(fn) };
			const network = { getNetworks: async () => [{ getName: () => 'modem', getIPAddrs: () => ['192.0.2.1/24'], getIP6Addrs: () => [] }] };
			const L = { hasViewPermission: () => !mock.readonly, Class: { extend: spec => spec } };
			const form = { NamedSection: 'named', GridSection: 'grid', Flag: 'flag', ListValue: 'select', Value: 'text', Button: 'button', DynamicList: 'list' };
			form.Map = class {
				constructor(name, title, description) { this.name = name; this.title = title; this.description = description; this.sections = []; }
				section(type, name, subtype, title) {
					const section = { fields: [], title: type === 'named' ? title : subtype, sid: type === 'named' ? name : name === 'channel' ? 'ch_demo' : 'd_demo' };
					section.option = (type, name, label) => { const field = { type, name, label, choices: [], value(v, title) { this.choices.push([v, title || v]); } }; section.fields.push(field); return field; };
					this.sections.push(section); return section;
				}
				async render() { return E('div', {}, [ E('h2', {}, this.title), E('p', {}, this.description), ...this.sections.map(s => E('section', { class: 'cbi-section' }, [ E('h3', {}, s.title || ''), ...s.fields.map(f => E('div', { class: 'field' }, [ E('label', {}, f.label), f.type === 'button' ? E('button', { class: 'btn', click: function(ev) { return f.onclick?.(ev, s.sid); } }, f.inputtitle || f.label) : f.type === 'select' ? E('select', {}, f.choices.map(c => E('option', { value: c[0] }, c[1]))) : E('input', { type: f.type === 'flag' ? 'checkbox' : f.password ? 'password' : 'text' }) ])) ])) ]); }
			};
			function load(source, smsrelay) { return new Function('view','fs','poll','network','smsrelay','ui','form','uci','rpc','L', source)({ extend: x => x }, fs, poll, network, smsrelay, ui, form, uci, rpc, L); }
			const shared = load(sources.shared);
			window.modules = Object.fromEntries(['config','status','messages','logs'].map(name => [name, load(sources[name], shared)]));
			window.show = async name => { mock.polls = []; const m = modules[name]; const data = m.load ? await m.load() : undefined; document.querySelector('#app').replaceChildren(await m.render(data)); };
		}, sources);
		await page.evaluate(() => show('status'));
		assert.equal(await page.getByText('暂无传感器', { exact: true }).count(), 2, 'PMIC must not replace CPU/baseband');
		assert.match(await page.locator('#app').innerText(), /CPU0 1.21 GHz/);
		await page.screenshot({ path: process.env.SMSRELAY_SCREENSHOT || '/tmp/opencode/smsrelay-status-preview.png', fullPage: true });
		await page.evaluate(() => { mock.offline = true; return show('status'); });
		assert.match(await page.locator('#app').innerText(), /短信后台未运行/);
		await page.evaluate(() => { mock.offline = false; return show('messages'); });
		assert.equal(await page.locator('#app img').count(), 0, 'SMS must render as text, not HTML');
		assert.equal(await page.evaluate(() => window.XSS), undefined);
		assert.equal(await page.getByRole('button', { name: '写短信', exact: true }).isDisabled(), false, 'writable session must allow composing');
		assert.equal(await page.getByRole('button', { name: '删除所选', exact: true }).isDisabled(), false, 'writable session must allow batch delete');
		assert.equal(await page.locator('#app input[data-id]').first().isDisabled(), false, 'row checkboxes must be selectable');
		await page.getByRole('button', { name: '写短信', exact: true }).click();
		await page.locator('#modal input').fill('10086');
		await page.locator('#modal textarea').fill('测试,\n短信');
		await page.getByRole('button', { name: '发送', exact: true }).click();
		await page.waitForFunction(() => !document.querySelector('#modal'));
		assert.equal(await page.evaluate(() => mock.sendCalls), 1);
		assert.match(await page.locator('#app').innerText(), /排队中/);
		await page.evaluate(() => { mock.readonly = true; return show('messages'); });
		assert.equal(await page.getByRole('button', { name: '写短信', exact: true }).isDisabled(), true);
		await page.evaluate(() => { mock.readonly = false; return show('logs'); });
		await page.getByPlaceholder('过滤关键词').fill('转发');
		assert.match(await page.locator('#app pre').innerText(), /转发失败/);
		assert.doesNotMatch(await page.locator('#app pre').innerText(), /短信已归档/);
		await page.evaluate(() => show('config'));
		assert.equal(await page.getByText('性能与指示灯', { exact: true }).count(), 1);
		assert.equal(await page.getByText('监听端口', { exact: true }).count(), 0);
		await page.evaluate(() => show('status'));
		await page.setViewportSize({ width: 390, height: 844 });
		assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'mobile page should not overflow');
		assert.deepEqual(errors, []);
		console.log('LuCI browser smoke tests passed (mock RPC/files/form/theme; not a device test)');
	} finally { await browser.close(); }
}

main().catch(err => { console.error(err); process.exitCode = 1; });
