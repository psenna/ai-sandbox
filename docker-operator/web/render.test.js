// render.test.js -- issue #75's "DOM/component-level test": exercises
// render.js's pure HTML-string functions directly with Node's built-in test
// runner. No jsdom, no headless browser, no npm dependency at all -- this
// project has no build pipeline, and render.js is deliberately written with
// zero document/window access so it needs none of that to test.
//
// Run with: node --test web/render.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const Render = require('./render.js');

test('renderAgentList: empty state', () => {
	const html = Render.renderAgentList([], null);
	assert.match(html, /No agents yet/);
	assert.match(html, /agent-list__empty/);
});

test('renderAgentList: null/undefined agents also renders the empty state', () => {
	assert.match(Render.renderAgentList(null, null), /agent-list__empty/);
	assert.match(Render.renderAgentList(undefined, null), /agent-list__empty/);
});

test('renderAgentList: renders one <li> per agent, in API order', () => {
	const agents = [
		{ id: 'agt_a', name: 'Alpha', status: 'running' },
		{ id: 'agt_b', name: 'Bravo', status: 'stopped' },
	];
	const html = Render.renderAgentList(agents, null);
	const aIndex = html.indexOf('agt_a');
	const bIndex = html.indexOf('agt_b');
	assert.ok(aIndex >= 0 && bIndex >= 0 && aIndex < bIndex, 'expected agt_a before agt_b, got: ' + html);
});

test('renderAgentList: marks exactly the selected agent', () => {
	const agents = [
		{ id: 'agt_a', name: 'Alpha', status: 'running' },
		{ id: 'agt_b', name: 'Bravo', status: 'running' },
	];
	const html = Render.renderAgentList(agents, 'agt_b');
	const items = html.split('</li>').filter((s) => s.trim() !== '');
	assert.equal(items.length, 2);
	assert.doesNotMatch(items[0], /agent-item--selected/);
	assert.match(items[1], /agent-item--selected/);
});

test('renderAgentList: an agent name containing HTML is escaped, never rendered raw', () => {
	const html = Render.renderAgentList([{ id: 'agt_x', name: '<script>evil()</script>', status: 'running' }], null);
	assert.doesNotMatch(html, /<script>evil\(\)<\/script>/);
	assert.match(html, /&lt;script&gt;evil\(\)&lt;\/script&gt;/);
});

test('renderAgentList: an agent id containing HTML is escaped in the data attribute', () => {
	const html = Render.renderAgentList([{ id: '"><img src=x>', name: 'x', status: 'running' }], null);
	assert.doesNotMatch(html, /<img src=x>/);
});

test('renderAgentListItem: unnamed agent shows a placeholder label', () => {
	const html = Render.renderAgentListItem({ id: 'agt_c', name: '', status: 'creating' });
	assert.match(html, /\(unnamed\)/);
	assert.match(html, /status-creating/);
	assert.match(html, /data-agent-id="agt_c"/);
});

test('renderAgentListItem: selected agent gets the selected class, others do not', () => {
	const selected = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running' }, 'agt_a');
	const notSelected = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running' }, 'agt_b');
	assert.match(selected, /agent-item--selected/);
	assert.doesNotMatch(notSelected, /agent-item--selected/);
});

test('statusLabel: every known store.Status constant maps to a distinct label/class', () => {
	const known = ['creating', 'running', 'stopped', 'error', 'deleting'];
	const seen = new Set();
	for (const status of known) {
		const label = Render.statusLabel(status);
		assert.notEqual(label.cls, 'status-unknown', status + ' should not map to unknown');
		assert.ok(!seen.has(label.cls), 'duplicate css class for ' + status);
		seen.add(label.cls);
	}
});

test('statusLabel: an unrecognised status degrades to Unknown instead of throwing', () => {
	assert.equal(Render.statusLabel('some-future-status').cls, 'status-unknown');
	assert.equal(Render.statusLabel('').cls, 'status-unknown');
	assert.equal(Render.statusLabel(undefined).text, 'Unknown');
});

test('renderCapacity: pluralises correctly and handles zero agents', () => {
	assert.equal(Render.renderCapacity([], 1), '0 of 1 agent');
	assert.equal(Render.renderCapacity([{}, {}], 5), '2 of 5 agents');
	assert.equal(Render.renderCapacity(null, 5), '0 of 5 agents');
});

test('escapeHTML: escapes all five HTML-significant characters', () => {
	assert.equal(Render.escapeHTML(`<>&"'`), '&lt;&gt;&amp;&quot;&#39;');
});

test('escapeHTML: leaves ordinary text untouched', () => {
	assert.equal(Render.escapeHTML('agent-42 (staging)'), 'agent-42 (staging)');
});

test('backendLabel: maps ids to labels, empty falls back to Ollama', () => {
	assert.equal(Render.backendLabel('ollama'), 'Ollama');
	assert.equal(Render.backendLabel('anthropic'), 'Anthropic');
	assert.equal(Render.backendLabel(''), 'Ollama');
	assert.equal(Render.backendLabel(undefined), 'Ollama');
});

test('renderAgentListItem: shows the backend label', () => {
	assert.match(Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running', backend: 'anthropic' }), /Anthropic/);
	assert.match(Render.renderAgentListItem({ id: 'agt_b', name: 'B', status: 'running', backend: 'ollama' }), /Ollama/);
});

test('renderCreateForm: defaults pre-fill the model fields and select the backend', () => {
	const html = Render.renderCreateForm({ backend: 'ollama', model: 'glm-5.3:cloud', fastModel: 'glm-5.3-flash:cloud' });
	assert.match(html, /value="glm-5\.3:cloud"/);
	assert.match(html, /value="glm-5\.3-flash:cloud"/);
	assert.match(html, /name="backend" value="ollama" checked/);
	// The ollama model block is visible for an ollama default.
	assert.match(html, /create-form__ollama"(?!\s*hidden)/);
});

test('renderCreateForm: an anthropic default hides the ollama model block', () => {
	const html = Render.renderCreateForm({ backend: 'anthropic' });
	assert.match(html, /name="backend" value="anthropic" checked/);
	assert.match(html, /class="create-form__ollama" hidden/);
});

test('renderCreateForm: handles missing defaults without throwing', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /name="backend" value="ollama" checked/);
	assert.match(html, /class="create-form__model"/);
});

test('renderCreateForm: a model default containing HTML is escaped in the value attribute', () => {
	const html = Render.renderCreateForm({ model: '"><script>x</script>' });
	assert.doesNotMatch(html, /<script>x<\/script>/);
});

test('renderCreateForm: the repo field is always present and pre-fills from defaults', () => {
	const html = Render.renderCreateForm({ backend: 'anthropic', repo: 'psenna/ai-sandbox.git' });
	assert.match(html, /class="create-form__repo"[^>]*value="psenna\/ai-sandbox\.git"/);
});

test('renderCreateForm: an empty repo default leaves the field blank', () => {
	const html = Render.renderCreateForm({});
	assert.match(html, /class="create-form__repo"[^>]*value=""/);
});

test('renderCreateForm: a repo default containing HTML is escaped', () => {
	const html = Render.renderCreateForm({ repo: '"><script>x</script>' });
	assert.doesNotMatch(html, /<script>x<\/script>/);
});

test('renderAnthropicStatus: unset', () => {
	const html = Render.renderAnthropicStatus({ configured: false });
	assert.match(html, /No Anthropic credential/);
	assert.match(html, /anthropic-panel__status--unset/);
});

test('renderAnthropicStatus: api key with a date', () => {
	const html = Render.renderAnthropicStatus({ configured: true, kind: 'api_key', updated_at: '2026-09-05T12:00:00Z' });
	assert.match(html, /API key/);
	assert.match(html, /set 2026-09-05/);
	assert.match(html, /anthropic-panel__status--set/);
});

test('renderAnthropicStatus: oauth token, missing date is tolerated', () => {
	const html = Render.renderAnthropicStatus({ configured: true, kind: 'oauth' });
	assert.match(html, /OAuth token/);
	assert.doesNotMatch(html, /set /);
});

test('renderAnthropicStatus: null input degrades to unset', () => {
	assert.match(Render.renderAnthropicStatus(null), /anthropic-panel__status--unset/);
});
