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

test('renderCreateForm: an anthropic default hides the ollama block (server + model fields)', () => {
	const html = Render.renderCreateForm({ backend: 'anthropic' });
	assert.match(html, /name="backend" value="anthropic" checked/);
	assert.match(html, /class="create-form__ollama" hidden/);
	// The Ollama server field lives inside that block, so it hides with it.
	assert.match(html, /class="create-form__ollama-url"/);
});

test('renderCreateForm: the Ollama server field is blank with the operator default as its placeholder', () => {
	const html = Render.renderCreateForm({ backend: 'ollama', ollamaUrl: 'http://ollama:11434' });
	assert.match(html, /class="create-form__ollama-url" type="text" placeholder="http:\/\/ollama:11434"/);
	// Blank value: submitting it untouched means "use the operator default".
	assert.doesNotMatch(html, /class="create-form__ollama-url"[^>]*value=/);
});

test('renderCreateForm: the Ollama server placeholder falls back when the operator set no default', () => {
	const html = Render.renderCreateForm({ backend: 'ollama' });
	assert.match(html, /class="create-form__ollama-url" type="text" placeholder="operator default"/);
});

test('renderCreateForm: an ollama_url default containing HTML is escaped in the placeholder', () => {
	const html = Render.renderCreateForm({ ollamaUrl: '"><script>x</script>' });
	assert.doesNotMatch(html, /<script>x<\/script>/);
});

test('renderCreateForm: handles missing defaults without throwing', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /name="backend" value="ollama" checked/);
	assert.match(html, /class="create-form__model"/);
});

test('renderCreateForm: the auto-compact and max-context fields pre-fill from the operator defaults', () => {
	const html = Render.renderCreateForm({ autoCompactThreshold: '85', maxContextTokens: '200000' });
	assert.match(html, /class="create-form__auto-compact"[^>]*value="85"/);
	assert.match(html, /class="create-form__max-context-tokens"[^>]*value="200000"/);
});

test('renderCreateForm: the auto-compact and max-context fields render blank with no defaults', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /class="create-form__auto-compact" type="text" value=""/);
	assert.match(html, /class="create-form__max-context-tokens" type="text" value=""/);
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

test('renderAgentInfo: renders both sections with the agent\'s resolved values', () => {
	const html = Render.renderAgentInfo({
		agent: {
			id: 'agt_a1b2c3d4',
			name: 'Alpha',
			description: 'the worker',
			backend: 'ollama',
			model: 'glm-5.3:cloud',
			fast_model: 'glm-5.3-flash:cloud',
			ollama_url: 'http://ollama:11434',
			repo: 'acme/widget.git',
			auto_compact_threshold: '85',
			max_context_tokens: '200000',
		},
		operator: { agent_image: 'ghcr.io/example/agent:1.2.3', docker_runtime: 'crun' },
	});
	assert.match(html, /agent-info/);
	assert.match(html, /Agent/);
	assert.match(html, /Operator/);
	assert.match(html, /agt_a1b2c3d4/);
	assert.match(html, /acme\/widget\.git/);
	assert.match(html, /ghcr\.io\/example\/agent:1\.2\.3/);
	assert.match(html, /crun/);
	assert.match(html, /Ollama/);
});

test('renderAgentInfo: blank parameters render their placeholder, not an empty cell', () => {
	const html = Render.renderAgentInfo({
		agent: { backend: 'anthropic' },
		operator: { agent_image: 'ghcr.io/example/agent:1', docker_runtime: 'crun' },
	});
	assert.match(html, /agent-info__value--blank/);
	assert.match(html, /built-in default/);
	assert.doesNotMatch(html, /<dd class="agent-info__value"><\/dd>/);
});

test('renderAgentInfo: values containing HTML are escaped, never rendered raw', () => {
	const html = Render.renderAgentInfo({
		agent: { id: '"><img src=id>', name: '<script>evil()</script>', repo: '"><img src=x>' },
		operator: {},
	});
	assert.doesNotMatch(html, /<script>evil\(\)<\/script>/);
	assert.doesNotMatch(html, /<img src=x>/);
	assert.doesNotMatch(html, /<img src=id>/);
});

test('renderAgentInfo: missing input degrades to placeholders instead of throwing', () => {
	assert.doesNotThrow(() => Render.renderAgentInfo());
	assert.doesNotThrow(() => Render.renderAgentInfo(null));
	assert.match(Render.renderAgentInfo(), /agent-info/);
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

// --- agent image tag helpers ---------------------------------------------

test('isDateTimeTag: matches only YYYYMMDD-HHMMSS', () => {
	assert.ok(Render.isDateTimeTag('20260101-120000'));
	assert.ok(Render.isDateTimeTag('19991231-235959'));
	for (const bad of ['', 'latest', '2026010-120000', '20260101-12000', '20260101_120000', null, undefined]) {
		assert.equal(Render.isDateTimeTag(bad), false, JSON.stringify(bad));
	}
});

test('newestDateTimeTag: returns the lexically greatest date-time tag, or empty', () => {
	assert.equal(Render.newestDateTimeTag(['20251231-090000', 'latest', '20260101-120000']), '20260101-120000');
	assert.equal(Render.newestDateTimeTag(['latest', 'main']), '');
	assert.equal(Render.newestDateTimeTag([]), '');
	assert.equal(Render.newestDateTimeTag(null), '');
});

test('upgradeAvailable: true only for a date-time current tag with a strictly newer date-time tag in the list', () => {
	assert.equal(Render.upgradeAvailable('20260101-120000', ['20260101-120000', '20260201-090000']), true);
	assert.equal(Render.upgradeAvailable('20260201-090000', ['20260101-120000', '20260201-090000']), false);
	assert.equal(Render.upgradeAvailable('20260301-000000', ['20260101-120000', '20260201-090000']), false);
	assert.equal(Render.upgradeAvailable('', ['20260201-090000']), false);
	assert.equal(Render.upgradeAvailable('latest', ['20260201-090000']), false);
	assert.equal(Render.upgradeAvailable('20260101-120000', ['latest', 'main']), false);
	assert.equal(Render.upgradeAvailable('20260101-120000', null), false);
	assert.equal(Render.upgradeAvailable('20260101-120000', []), false);
	assert.equal(Render.upgradeAvailable('20260201-090000', ['20260201-090000', '20260101-120000']), false);
	assert.equal(Render.upgradeAvailable('20260101-120000', ['20251231-000000', 'latest', '20260601-000000']), true);
});

test('renderAgentListItem: upgrade marker present only when upgrade_available is truthy, and before the backend badge', () => {
	const withUpgrade = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running', backend: 'ollama', upgrade_available: true });
	assert.match(withUpgrade, /agent-item__upgrade/);
	assert.ok(
		withUpgrade.indexOf('agent-item__upgrade') < withUpgrade.indexOf('agent-item__backend'),
		'upgrade marker should sit before the backend badge',
	);

	assert.doesNotMatch(
		Render.renderAgentListItem({ id: 'agt_b', name: 'B', status: 'running', upgrade_available: false }),
		/agent-item__upgrade/,
	);
	assert.doesNotMatch(
		Render.renderAgentListItem({ id: 'agt_c', name: 'C', status: 'running' }),
		/agent-item__upgrade/,
	);
});

test('imageTagOf: tag only when no slash follows the last colon', () => {
	assert.equal(Render.imageTagOf('ghcr.io/psenna/agent:20260101-120000'), '20260101-120000');
	assert.equal(Render.imageTagOf('host/x:tag'), 'tag');
	assert.equal(Render.imageTagOf('host:5000/x'), '');
	assert.equal(Render.imageTagOf('x'), '');
	assert.equal(Render.imageTagOf(''), '');
});

test('formatCheckedAgo: falsy is "never checked", otherwise coarse buckets', () => {
	assert.equal(Render.formatCheckedAgo(''), 'never checked');
	assert.equal(Render.formatCheckedAgo(null), 'never checked');
	assert.equal(Render.formatCheckedAgo('not-a-date'), 'never checked');
	assert.equal(Render.formatCheckedAgo(new Date().toISOString()), 'checked just now');
	assert.equal(Render.formatCheckedAgo(new Date(Date.now() - 5 * 60 * 1000).toISOString()), 'checked 5 minutes ago');
	assert.equal(Render.formatCheckedAgo(new Date(Date.now() - 3 * 3600 * 1000).toISOString()), 'checked 3 hours ago');
	assert.equal(Render.formatCheckedAgo(new Date(Date.now() - 2 * 86400 * 1000).toISOString()), 'checked 2 days ago');
});

test('renderImageTagSelect: default first and labelled, then newest-first deduped', () => {
	const html = Render.renderImageTagSelect('c', ['20251231-090000', '20260101-120000', '20251231-090000', 'latest'], 'latest', '');
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.equal(opts[0], '<option value="latest" selected>latest (default)</option>');
	assert.equal(opts[1], '<option value="20260101-120000">20260101-120000</option>');
	assert.equal(opts[2], '<option value="20251231-090000">20251231-090000</option>');
	assert.equal(opts.length, 3, 'default deduped from the tail, no repeats: ' + html);
});

test('renderImageTagSelect: a selectedTag not in the list still appears and is selected', () => {
	const html = Render.renderImageTagSelect('c', ['20260101-120000'], 'latest', '20200101-000000');
	assert.match(html, /<option value="20200101-000000" selected>20200101-000000<\/option>/);
	assert.doesNotMatch(html, /value="latest" selected/);
});

test('renderImageTagSelect: falls back to the default option when selectedTag is empty', () => {
	const html = Render.renderImageTagSelect('c', ['20260101-120000'], 'latest', '');
	assert.match(html, /<option value="latest" selected>latest \(default\)<\/option>/);
});

test('renderImageTagSelect: escapes every value', () => {
	const html = Render.renderImageTagSelect('c', ['"><img src=x>'], '"><b>', '');
	assert.doesNotMatch(html, /<img src=x>/);
	assert.doesNotMatch(html, /<b>/);
});

test('renderAgentImagePanel: with tags shows the newest and a Check now button', () => {
	const html = Render.renderAgentImagePanel({
		tags: ['20260101-120000'], newest: '20260101-120000', operatorDefault: 'latest',
		checkedAt: new Date().toISOString(), lastError: '',
	});
	assert.match(html, /20260101-120000/);
	assert.match(html, /checked just now/);
	assert.match(html, /agent-image-panel__refresh/);
	assert.doesNotMatch(html, /agent-image-panel__error/);
});

test('renderAgentImagePanel: no tags shows "none discovered"', () => {
	const html = Render.renderAgentImagePanel({ tags: [], newest: '', operatorDefault: 'latest', checkedAt: null });
	assert.match(html, /none discovered/);
	assert.match(html, /never checked/);
});

test('renderAgentImagePanel: a last error renders the error line', () => {
	const html = Render.renderAgentImagePanel({ tags: [], lastError: 'registry is unreachable' });
	assert.match(html, /agent-image-panel__error/);
	assert.match(html, /registry is unreachable/);
});

test('renderAgentImagePanel: missing input does not throw', () => {
	assert.doesNotThrow(() => Render.renderAgentImagePanel());
});

// --- renderCreateForm: opts + image-tag row -----------------------------

test('renderCreateForm: still renders with a single argument', () => {
	const html = Render.renderCreateForm({ backend: 'ollama' });
	assert.match(html, /class="create-form"/);
	assert.match(html, /New agent/);
	assert.match(html, />Create</);
});

test('renderCreateForm: opts.title and opts.submitLabel override the defaults', () => {
	const html = Render.renderCreateForm({}, { title: 'Update agent', submitLabel: 'Update' });
	assert.match(html, /Update agent/);
	assert.match(html, />Update</);
	assert.doesNotMatch(html, /New agent/);
});

test('renderCreateForm: the image-tag row renders the select seeded from defaults', () => {
	const html = Render.renderCreateForm({ imageTags: ['20260101-120000'], imageDefaultTag: 'latest' });
	assert.match(html, /create-form__image-tag/);
	assert.match(html, /latest \(default\)/);
	assert.match(html, /20260101-120000/);
});

test('renderCreateForm: opts.values pre-fills name/description and a selected image tag', () => {
	const html = Render.renderCreateForm(
		{ imageTags: ['20260101-120000'], imageDefaultTag: 'latest' },
		{ values: { name: 'Neo', description: 'the one', image_tag: '20260101-120000' } });
	assert.match(html, /class="create-form__name" type="text" value="Neo"/);
	assert.match(html, /class="create-form__description" type="text" value="the one"/);
	assert.match(html, /<option value="20260101-120000" selected>/);
});

test('renderCreateForm: opts.values overrides the operator defaults for every create-form field', () => {
	const html = Render.renderCreateForm(
		{ backend: 'ollama', model: 'op-default', fastModel: 'fast-default', repo: 'op/default.git', autoCompactThreshold: '80', maxContextTokens: '100000' },
		{
			title: 'Update agent', submitLabel: 'Update',
			values: {
				backend: 'ollama', model: 'agent-opus', fast_model: 'agent-fast',
				ollama_url: 'http://gpu-box:11434', repo: 'acme/widget.git',
				auto_compact_threshold: '95', max_context_tokens: '250000',
			},
		});
	assert.match(html, /Update agent/);
	assert.match(html, /class="create-form__model" type="text" value="agent-opus"/);
	assert.match(html, /class="create-form__fast-model" type="text" value="agent-fast"/);
	assert.match(html, /class="create-form__ollama-url" type="text" value="http:\/\/gpu-box:11434" placeholder=/);
	assert.match(html, /class="create-form__repo" type="text" value="acme\/widget.git"/);
	assert.match(html, /class="create-form__auto-compact" type="text" value="95"/);
	assert.match(html, /class="create-form__max-context-tokens" type="text" value="250000"/);
});

test('renderCreateForm: opts.values.backend=anthropic hides the ollama block even when the operator default is ollama', () => {
	const html = Render.renderCreateForm(
		{ backend: 'ollama' },
		{ values: { backend: 'anthropic' } });
	assert.match(html, /class="create-form__ollama" hidden/);
	assert.match(html, /value="anthropic" checked/);
});

test('renderCreateForm: with no image_tag, opts.values.image seeds the selected tag from the resolved ref', () => {
	const html = Render.renderCreateForm(
		{ imageTags: ['20260101-120000'], imageDefaultTag: 'latest' },
		{ values: { image: 'ghcr.io/x/agent:20260101-120000' } });
	assert.match(html, /<option value="20260101-120000" selected>/);
});
