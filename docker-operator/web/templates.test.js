// templates.test.js -- wiring tests for templates.js, the Templates page's
// event handlers. No jsdom in this project, so this drives templates.js
// against a hand-rolled fake container/window: just enough querySelector /
// addEventListener surface for the handlers under test, with the real
// render.js producing the markup.
//
// Run with: node --test web/templates.test.js

const test = require('node:test');
const assert = require('node:assert/strict');

// fakeEl is a minimal element: listeners keyed by event name, attributes,
// and a closest() that returns whatever ancestor the test wires in.
function fakeEl(attrs, ancestor) {
	const listeners = {};
	return {
		attrs: attrs || {},
		hidden: true,
		innerHTML: '',
		getAttribute(name) { return this.attrs[name]; },
		addEventListener(ev, fn) { listeners[ev] = fn; },
		click() { return listeners.click && listeners.click({ preventDefault() {} }); },
		closest() { return ancestor; },
		insertAdjacentHTML(_pos, html) { this.innerHTML += html; },
	};
}

// setup installs a fresh fake window (real Render, stubbed fetch/confirm/
// navigation hooks), loads templates.js against it, and returns the pieces
// a test asserts on. els maps a selector to the element(s) the fake
// container returns for it.
function setup(opts) {
	const calls = { fetches: [], confirms: [] };
	const responses = opts.responses || {};
	const els = opts.els || {};
	global.window = {
		Render: require('./render.js'),
		OperatorAuth: {
			fetch(url, options) {
				calls.fetches.push({ url, method: (options && options.method) || 'GET' });
				const body = responses[url] !== undefined ? responses[url] : {};
				return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(body)) });
			},
		},
		OperatorConfirm: {
			show(message, o) {
				calls.confirms.push({ message, opts: o });
				return Promise.resolve(opts.confirmAnswer);
			},
		},
	};
	const container = {
		innerHTML: '',
		querySelector(sel) { const v = els[sel]; return Array.isArray(v) ? v[0] : (v || null); },
		querySelectorAll(sel) { const v = els[sel]; return v ? (Array.isArray(v) ? v : [v]) : []; },
	};
	delete require.cache[require.resolve('./templates.js')];
	require('./templates.js');
	global.window.renderTemplatesPage(container);
	return { calls, container };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

test('templates.js: Delete asks for confirmation and sends nothing when cancelled', async () => {
	const row = fakeEl({ 'data-template-id': 'tpl_1' });
	const del = fakeEl({}, row);
	const { calls } = setup({
		responses: { '/api/templates': { templates: [{ id: 'tpl_1', name: 'nightly' }] } },
		els: { '.templates-page__delete': [del] },
		confirmAnswer: false,
	});
	await tick();
	del.click();
	await tick();

	assert.equal(calls.confirms.length, 1);
	const c = calls.confirms[0];
	assert.equal(c.opts.title, 'Delete this template?');
	assert.equal(c.opts.confirmLabel, 'Delete');
	assert.equal(c.opts.danger, true);
	assert.match(c.message, /cannot be undone/);
	assert.equal(calls.fetches.filter((f) => f.method === 'DELETE').length, 0);
});

test('templates.js: Delete sends DELETE once confirmed', async () => {
	const row = fakeEl({ 'data-template-id': 'tpl_1' });
	const del = fakeEl({}, row);
	const { calls } = setup({
		responses: { '/api/templates': { templates: [{ id: 'tpl_1', name: 'nightly' }] } },
		els: { '.templates-page__delete': [del] },
		confirmAnswer: true,
	});
	await tick();
	del.click();
	await tick();

	const deletes = calls.fetches.filter((f) => f.method === 'DELETE');
	assert.deepEqual(deletes.map((f) => f.url), ['/api/templates/tpl_1']);
});
