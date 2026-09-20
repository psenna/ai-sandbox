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

test('renderAgentList: threads the unreadIds map down to just the matching agent', () => {
	const agents = [
		{ id: 'agt_a', name: 'Alpha', status: 'running', activity: 'waiting' },
		{ id: 'agt_b', name: 'Bravo', status: 'running', activity: 'waiting' },
	];
	const html = Render.renderAgentList(agents, null, { agt_b: true });
	const items = html.split('</li>').filter((s) => s.trim() !== '');
	assert.doesNotMatch(items[0], /agent-item--unread/);
	assert.match(items[1], /agent-item--unread/);
});

test('renderAgentListItem: unnamed agent shows a placeholder label', () => {
	const html = Render.renderAgentListItem({ id: 'agt_c', name: '', status: 'creating' });
	assert.match(html, /\(unnamed\)/);
	assert.match(html, /status-creating/);
	assert.match(html, /data-agent-id="agt_c"/);
});

test('renderAgentListItem: status is shown as visible text, not only the dot color', () => {
	const html = Render.renderAgentListItem({ id: 'agt_d', name: 'Delta', status: 'error' });
	assert.match(html, /class="agent-item__activity">Error<\/span>/);
});

test('renderAgentListItem: an unrecognised status renders itself as visible text too', () => {
	const html = Render.renderAgentListItem({ id: 'agt_e', name: 'Echo', status: 'bogus' });
	assert.match(html, /class="agent-item__activity">bogus<\/span>/);
});

test('renderAgentListItem: a missing status renders "Unknown" as visible text too', () => {
	const html = Render.renderAgentListItem({ id: 'agt_f', name: 'Foxtrot', status: '' });
	assert.match(html, /class="agent-item__activity">Unknown<\/span>/);
});

test('renderAgentListItem: activity "working" pulses the dot and marks the label', () => {
	const html = Render.renderAgentListItem({ id: 'agt_g', name: 'Golf', status: 'running', activity: 'working' });
	assert.match(html, /class="status-dot status-running status-dot--pulse"/);
	assert.match(html, /class="agent-item__activity agent-item__activity--working">Working…<\/span>/);
});

test('renderAgentListItem: no activity signal renders the plain status, no pulse', () => {
	const html = Render.renderAgentListItem({ id: 'agt_i', name: 'India', status: 'running' });
	assert.doesNotMatch(html, /status-dot--pulse/);
	assert.match(html, /class="agent-item__activity">Running<\/span>/);
});

test('renderAgentListItem: unread (finished-and-unviewed) shows "Waiting for you" and wins over a live "working" signal', () => {
	const html = Render.renderAgentListItem({ id: 'agt_h', name: 'Hotel', status: 'running', activity: 'working' }, null, true);
	assert.match(html, /<li class="agent-item agent-item--unread"/);
	assert.match(html, /class="agent-item__activity agent-item__activity--unread">/);
	assert.match(html, /Waiting for you/);
	assert.doesNotMatch(html, /Working…/);
	assert.doesNotMatch(html, /status-dot--pulse/);
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

test('renderCreateForm: the auto-mode select defaults to "operator default" and labels it from defaults.autoMode', () => {
	const on = Render.renderCreateForm({ autoMode: 'on' });
	assert.match(on, /<option value=""\s+selected>Operator default \(on\)<\/option>/);
	const off = Render.renderCreateForm({ autoMode: 'off' });
	assert.match(off, /<option value=""\s+selected>Operator default \(off\)<\/option>/);
});

test('renderCreateForm: opts.values.auto_mode selects the matching option, not "operator default"', () => {
	const html = Render.renderCreateForm({ autoMode: 'on' }, { values: { auto_mode: 'off' } });
	assert.match(html, /<option value="off" selected>Off<\/option>/);
	// The auto-mode select's own "operator default" option -- not any other
	// select on the page -- must be the unselected one.
	assert.match(html, /<select class="create-form__auto-mode"><option value="">Operator default/);
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
			auto_mode: 'on',
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
	assert.match(html, />On</);
});

test('renderAgentInfo: shows this agent\'s own resolved image, separate from the operator-wide default', () => {
	const html = Render.renderAgentInfo({
		agent: {
			id: 'agt_a1b2c3d4',
			image: 'ghcr.io/psenna/ai-sandbox-agent:20260913-025754',
			image_id: 'sha256:0123456789abcdef0123',
		},
		operator: { agent_image: 'ghcr.io/psenna/ai-sandbox-agent:latest', docker_runtime: 'crun' },
	});
	assert.match(html, /20260913-025754/);
	// Two agents can share the same operator-default tag while running
	// different builds -- the per-agent row must show THIS agent's own tag
	// and resolved id, not the operator's config-wide default.
	assert.match(html, /ghcr\.io\/psenna\/ai-sandbox-agent:latest/); // still present, in the Operator section
	assert.match(html, /0123456789ab/); // short id: algorithm prefix stripped, 12 hex chars
	assert.doesNotMatch(html, /0123456789abcdef0123/); // never the untruncated digest
});

test('renderAgentInfo: an agent with no recorded image falls back to placeholders, not a blank cell', () => {
	const html = Render.renderAgentInfo({
		agent: { id: 'agt_a1b2c3d4' },
		operator: { agent_image: 'ghcr.io/psenna/ai-sandbox-agent:latest', docker_runtime: 'crun' },
	});
	assert.match(html, /\(operator default\)/);
	assert.match(html, />unknown</);
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

test('renderAgentListItem: upgrade_ready adds the --ready modifier to the upgrade marker', () => {
	const ready = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running', upgrade_available: true, upgrade_ready: true });
	assert.match(ready, /class="agent-item__upgrade agent-item__upgrade--ready"/);
	assert.match(ready, /already on this host/);

	// upgrade_ready is not a subset of upgrade_available: a host can hold a
	// newer image the registry snapshot does not list yet, so it flags alone.
	assert.match(
		Render.renderAgentListItem({ id: 'agt_b', name: 'B', status: 'running', upgrade_ready: true }),
		/agent-item__upgrade--ready/);

	const availableOnly = Render.renderAgentListItem({ id: 'agt_c', name: 'C', status: 'running', upgrade_available: true });
	assert.match(availableOnly, /class="agent-item__upgrade"/);
	assert.doesNotMatch(availableOnly, /agent-item__upgrade--ready/);

	assert.doesNotMatch(Render.renderAgentListItem({ id: 'agt_d', name: 'D', status: 'running' }), /agent-item__upgrade/);
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

// --- agent image: per-harness map (issue #200) ---------------------------

test('harnessImageTags: maps the API body to a camelCase by-harness map, one entry per harness', () => {
	const map = Render.harnessImageTags({
		harnesses: {
			'claude-code': {
				repo: 'ghcr.io/x/agent', default_tag: '20260101-120000', newest: '20260101-120000',
				tags: [{ tag: '20260101-120000', present: true }, { tag: '20251231-090000', present: false }],
				checked_at: '2026-01-02T00:00:00Z', last_error: '',
			},
		},
	});
	assert.deepEqual(Object.keys(map), ['claude-code', 'opencode']);
	assert.equal(map['claude-code'].repo, 'ghcr.io/x/agent');
	assert.equal(map['claude-code'].defaultTag, '20260101-120000');
	assert.equal(map['claude-code'].checkedAt, '2026-01-02T00:00:00Z');
	assert.deepEqual(map['claude-code'].tags, [
		{ tag: '20260101-120000', present: true },
		{ tag: '20251231-090000', present: false },
	]);
	// A harness the server did not report is still present, just empty.
	assert.deepEqual(map.opencode, { repo: '', defaultTag: '', newest: '', tags: [], checkedAt: null, lastError: '' });
});

test('harnessImageTags: missing/empty input yields one empty entry per known harness', () => {
	for (const input of [null, undefined, {}, { harnesses: null }]) {
		const map = Render.harnessImageTags(input);
		assert.deepEqual(Object.keys(map), ['claude-code', 'opencode']);
		assert.deepEqual(map['claude-code'].tags, []);
	}
});

test('harnessImageTags: a harness this build does not know is kept, after the known ones', () => {
	const map = Render.harnessImageTags({ harnesses: { 'future-harness': { default_tag: 'latest' } } });
	assert.deepEqual(Object.keys(map), ['claude-code', 'opencode', 'future-harness']);
	assert.equal(map['future-harness'].defaultTag, 'latest');
});

test('imageTagsForHarness: picks the harness entry, degrading to claude-code then to an empty entry', () => {
	const map = Render.harnessImageTags({
		harnesses: {
			'claude-code': { default_tag: 'cc', tags: [{ tag: 'cc', present: true }] },
			opencode: { default_tag: 'oc', tags: [{ tag: 'oc', present: false }] },
		},
	});
	assert.equal(Render.imageTagsForHarness(map, 'opencode').defaultTag, 'oc');
	assert.equal(Render.imageTagsForHarness(map, 'claude-code').defaultTag, 'cc');
	// "" (a record written before the harness field existed) and anything
	// unrecognised is claude-code, like config.NormalizeHarness.
	assert.equal(Render.imageTagsForHarness(map, '').defaultTag, 'cc');
	assert.equal(Render.imageTagsForHarness(map, undefined).defaultTag, 'cc');
	assert.equal(Render.imageTagsForHarness(map, 'bogus').defaultTag, 'cc');
	assert.deepEqual(Render.imageTagsForHarness(null, 'opencode'),
		{ repo: '', defaultTag: '', newest: '', tags: [], checkedAt: null, lastError: '' });
});

test('imageTagOffered: true only for a tag that harness actually offers', () => {
	const entry = { tags: [{ tag: '20260101-120000', present: true }, { tag: 'latest', present: false }] };
	assert.equal(Render.imageTagOffered(entry, '20260101-120000'), true);
	assert.equal(Render.imageTagOffered(entry, 'latest'), true);
	assert.equal(Render.imageTagOffered(entry, '20260202-020202'), false);
	assert.equal(Render.imageTagOffered(entry, ''), false);
	assert.equal(Render.imageTagOffered(null, 'latest'), false);
	assert.equal(Render.imageTagOffered(entry.tags, 'latest'), true);
});

test('renderImageTagSelect: keeps the server\'s order, de-dupes, and labels the default in place', () => {
	const html = Render.renderImageTagSelect('c', [
		{ tag: '20260101-120000', present: false },
		{ tag: '20251231-090000', present: true },
		{ tag: '20251231-090000', present: true },
		{ tag: 'latest', present: true },
	], 'latest', '');
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.equal(opts.length, 3, 'no repeats: ' + html);
	// Server order is preserved: internal/agent.OfferedImageTags already put
	// the newest published tags first and appended the default last.
	assert.equal(opts[0], '<option value="20260101-120000" data-pull-required="true">20260101-120000 (pull required)</option>');
	assert.equal(opts[1], '<option value="20251231-090000">20251231-090000</option>');
	assert.equal(opts[2], '<option value="latest" selected>latest (default)</option>');
});

test('renderImageTagSelect: a tag the host does not hold is labelled "pull required", a present one is not', () => {
	const html = Render.renderImageTagSelect('c', [
		{ tag: '20260101-120000', present: true },
		{ tag: '20251231-090000', present: false },
	], '', '');
	assert.match(html, /<option value="20260101-120000">20260101-120000<\/option>/);
	assert.match(html, /<option value="20251231-090000" data-pull-required="true">20251231-090000 \(pull required\)<\/option>/);
});

test('renderImageTagSelect: a default that is not on the host carries both notes', () => {
	const html = Render.renderImageTagSelect('c', [{ tag: 'latest', present: false }], 'latest', '');
	assert.match(html, /<option value="latest" selected data-pull-required="true">latest \(default, pull required\)<\/option>/);
});

test('renderImageTagSelect: no default for this harness => a blank "(operator default)" option, selected', () => {
	const html = Render.renderImageTagSelect('c', [{ tag: '20260101-120000', present: true }], '', '');
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.equal(opts[0], '<option value="" selected>(operator default)</option>');
	assert.equal(opts[1], '<option value="20260101-120000">20260101-120000</option>');
});

test('renderImageTagSelect: a selectedTag this harness does not offer is appended, selected and pull-required', () => {
	const html = Render.renderImageTagSelect('c', [{ tag: '20260101-120000', present: true }], 'latest', '20200101-000000');
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.equal(opts[opts.length - 1],
		'<option value="20200101-000000" selected data-pull-required="true">20200101-000000 (pull required)</option>');
	assert.doesNotMatch(html, /value="latest" selected/);
});

test('renderImageTagSelect: falls back to the default option when selectedTag is empty', () => {
	const html = Render.renderImageTagSelect(
		'c', [{ tag: 'latest', present: true }, { tag: '20260101-120000', present: true }], 'latest', '');
	assert.match(html, /<option value="latest" selected>latest \(default\)<\/option>/);
});

test('renderImageTagSelect: tolerates a plain string tag list (no presence info => no "pull required")', () => {
	const html = Render.renderImageTagSelect('c', ['20260101-120000'], '', '');
	assert.match(html, /<option value="20260101-120000">20260101-120000<\/option>/);
	assert.doesNotMatch(html, /pull required/);
});

test('renderImageTagSelect: escapes every value', () => {
	const html = Render.renderImageTagSelect('c', [{ tag: '"><img src=x>', present: true }], '"><b>', '');
	assert.doesNotMatch(html, /<img src=x>/);
	assert.doesNotMatch(html, /<b>/);
});

test('renderImageTagSelect: an empty list with no default still renders a submittable select', () => {
	assert.equal(
		Render.renderImageTagSelect('create-form__image-tag', [], '', ''),
		'<select class="create-form__image-tag"><option value="" selected>(operator default)</option></select>');
});

test('renderAgentImagePanel: one block per harness, claude-code first, under a single Check now button', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: {
			'claude-code': { newest: '20260101-120000', tags: [{ tag: '20260101-120000', present: true }], checked_at: new Date().toISOString() },
			opencode: { newest: '20260202-020202', tags: [{ tag: '20260202-020202', present: false }], checked_at: new Date().toISOString() },
		},
	}));
	assert.match(html, /data-harness="claude-code"/);
	assert.match(html, /data-harness="opencode"/);
	assert.match(html, /Claude Code/);
	assert.match(html, /Newest tag: 20260101-120000/);
	assert.match(html, /Newest tag: 20260202-020202/);
	assert.ok(html.indexOf('data-harness="claude-code"') < html.indexOf('data-harness="opencode"'),
		'claude-code block should come first: ' + html);
	// One shared refresh button: POST /api/agent-image/refresh refreshes every
	// harness in a single call.
	assert.equal(html.match(/agent-image-panel__refresh/g).length, 1);
	assert.ok(html.lastIndexOf('data-harness=') < html.indexOf('agent-image-panel__refresh'),
		'the Check now button sits after every harness block');
	assert.equal((html.match(/checked just now/g) || []).length, 2);
});

test('renderAgentImagePanel: each harness reports its own newest tag and its own checked-at', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: {
			'claude-code': { newest: '20260101-120000', tags: [{ tag: '20260101-120000', present: true }], checked_at: new Date().toISOString() },
			opencode: { newest: '', tags: [], checked_at: null },
		},
	}));
	assert.equal((html.match(/none discovered/g) || []).length, 1);
	assert.equal((html.match(/never checked/g) || []).length, 1);
});

test('renderAgentImagePanel: a per-harness last error renders inside that harness\'s block only', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: {
			'claude-code': { tags: [{ tag: '20260101-120000', present: true }] },
			opencode: { tags: [], last_error: 'registry is unreachable' },
		},
	}));
	assert.equal((html.match(/agent-image-panel__error/g) || []).length, 1);
	assert.match(html.slice(html.indexOf('data-harness="opencode"')), /registry is unreachable/);
});

test('renderAgentImagePanel: falls back to the newest date-time tag when the server sent no newest', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: { 'claude-code': { tags: [{ tag: '20251231-090000', present: true }, { tag: '20260101-120000', present: false }] } },
	}));
	assert.match(html, /Newest tag: 20260101-120000/);
});

test('renderAgentImagePanel: accepts the raw API body as well as an already-mapped map', () => {
	const raw = { harnesses: { 'claude-code': { newest: '20260101-120000', tags: [] } } };
	assert.equal(Render.renderAgentImagePanel(raw), Render.renderAgentImagePanel(Render.harnessImageTags(raw)));
});

test('renderAgentImagePanel: missing input does not throw and still renders the Check now button', () => {
	assert.doesNotThrow(() => Render.renderAgentImagePanel());
	assert.match(Render.renderAgentImagePanel(), /agent-image-panel__refresh/);
});

test('renderAgentImagePanel: a harness key containing HTML is escaped', () => {
	const html = Render.renderAgentImagePanel({ '"><img src=x>': { tags: [] } });
	assert.doesNotMatch(html, /<img src=x>/);
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
	const html = Render.renderCreateForm({
		agentImage: Render.harnessImageTags({
			harnesses: { 'claude-code': { default_tag: 'latest', tags: [{ tag: '20260101-120000', present: true }, { tag: 'latest', present: true }] } },
		}),
	});
	assert.match(html, /create-form__image-tag/);
	assert.match(html, /latest \(default\)/);
	assert.match(html, /20260101-120000/);
});

test('renderCreateForm: opts.values pre-fills name/description and a selected image tag', () => {
	const html = Render.renderCreateForm(
		{ agentImage: Render.harnessImageTags({ harnesses: { 'claude-code': { default_tag: 'latest', tags: [{ tag: '20260101-120000', present: true }] } } }) },
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

// --- renderCreateForm: harness selector (issue #182) ---------------------

test('renderCreateForm: defaults to claude-code', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /name="harness" value="claude-code" checked/);
	assert.doesNotMatch(html, /name="harness" value="opencode" checked/);
});

test('renderCreateForm: opts.values.harness = "opencode" selects the opencode radio', () => {
	const html = Render.renderCreateForm({}, { values: { harness: 'opencode' } });
	assert.match(html, /name="harness" value="opencode" checked/);
});

test('renderCreateForm: harness=opencode hides and disables the Anthropic backend option', () => {
	const html = Render.renderCreateForm({}, { values: { harness: 'opencode' } });
	assert.match(html, /class="create-form__backend-anthropic"[^>]*hidden/);
	assert.match(html, /name="backend" value="anthropic"[^>]*disabled/);
});

test('renderCreateForm: harness=opencode still leaves the Ollama fields visible', () => {
	const html = Render.renderCreateForm({}, { values: { harness: 'opencode' } });
	assert.match(html, /create-form__ollama"(?!\s*hidden)/);
});

test('renderCreateForm: harness=opencode forces backend=ollama even when values say anthropic', () => {
	const html = Render.renderCreateForm({}, { values: { harness: 'opencode', backend: 'anthropic' } });
	assert.match(html, /name="backend" value="ollama" checked/);
	assert.doesNotMatch(html, /name="backend" value="anthropic" checked/);
});

test('renderCreateForm: claude-code keeps the Anthropic backend available', () => {
	const html = Render.renderCreateForm();
	assert.doesNotMatch(html, /class="create-form__backend-anthropic"[^>]*hidden/);
	assert.doesNotMatch(html, /name="backend" value="anthropic"[^>]*disabled/);
	assert.match(html, /class="create-form__opencode-note"[^>]*hidden/);
});

test('renderCreateForm: opts.harnessLocked disables both harness radios and labels the legend', () => {
	const html = Render.renderCreateForm({}, { harnessLocked: true });
	assert.match(html, /name="harness" value="claude-code"[^>]*disabled/);
	assert.match(html, /name="harness" value="opencode"[^>]*disabled/);
	assert.match(html, /Harness \(set at create time\)/);

	const unlocked = Render.renderCreateForm();
	assert.doesNotMatch(unlocked, /name="harness" value="claude-code"[^>]*disabled/);
	assert.doesNotMatch(unlocked, /name="harness" value="opencode"[^>]*disabled/);
});

test('harnessSupportsBackend: opencode is Ollama-only, claude-code runs on either', () => {
	assert.equal(Render.harnessSupportsBackend('opencode', 'anthropic'), false);
	assert.equal(Render.harnessSupportsBackend('opencode', 'ollama'), true);
	assert.equal(Render.harnessSupportsBackend('claude-code', 'anthropic'), true);
	assert.equal(Render.harnessSupportsBackend('claude-code', 'ollama'), true);
});

test('renderCreateForm: the image-tag row is sourced from the SELECTED harness\'s own tags', () => {
	const defaults = {
		agentImage: Render.harnessImageTags({
			harnesses: {
				'claude-code': { default_tag: '20260101-120000', tags: [{ tag: '20260101-120000', present: true }] },
				opencode: { default_tag: '20260202-020202', tags: [{ tag: '20260202-020202', present: false }] },
			},
		}),
	};

	const cc = Render.renderCreateForm(defaults);
	assert.match(cc, /<option value="20260101-120000" selected>20260101-120000 \(default\)<\/option>/);
	assert.doesNotMatch(cc, /20260202-020202/);

	// Same defaults, opencode selected: opencode's OWN tag history and default,
	// never claude-code's -- what app.js's syncImageTagSelect re-renders on a
	// harness radio change.
	const oc = Render.renderCreateForm(defaults, { values: { harness: 'opencode' } });
	assert.match(oc, /<option value="20260202-020202" selected data-pull-required="true">20260202-020202 \(default, pull required\)<\/option>/);
	assert.doesNotMatch(oc, /20260101-120000/);
});

test('renderCreateForm: an agent record with no harness field falls back to claude-code\'s tags', () => {
	const defaults = {
		agentImage: Render.harnessImageTags({
			harnesses: { 'claude-code': { default_tag: 'latest', tags: [{ tag: 'latest', present: true }] } },
		}),
	};
	const html = Render.renderCreateForm(defaults, { values: { name: 'legacy' } });
	assert.match(html, /<option value="latest" selected>latest \(default\)<\/option>/);
});

test('renderCreateForm: no agentImage map at all still renders a submittable image-tag select', () => {
	assert.match(Render.renderCreateForm({}),
		/<select class="create-form__image-tag"><option value="" selected>\(operator default\)<\/option><\/select>/);
});

test('renderCreateForm: with no image_tag, opts.values.image seeds the selected tag from the resolved ref', () => {
	const html = Render.renderCreateForm(
		{ agentImage: Render.harnessImageTags({ harnesses: { 'claude-code': { default_tag: 'latest', tags: [{ tag: '20260101-120000', present: true }] } } }) },
		{ values: { image: 'ghcr.io/x/agent:20260101-120000' } });
	assert.match(html, /<option value="20260101-120000" selected>/);
});

// renderCreateForm(defaults, {values: <a Template-shaped object>}) is exactly
// how app.js's applyTemplateToForm prefills the form -- this proves that
// works with ZERO field-name mapping (Template's JSON tags were deliberately
// copied from Agent's own), and that the template's own name/description
// never leak into the agent's Name/Description fields: applyTemplateToForm
// is responsible for passing the CURRENT form's name/description back
// through itself, so a values object built from a template alone should
// leave those two fields blank, not populate them.
test('renderCreateForm: a Template-shaped opts.values pre-fills every infra field, not name/description', () => {
	const template = {
		id: 'tpl_00000001',
		name: 'my template',
		description: 'template description',
		backend: 'ollama',
		model: 'tpl-opus',
		fast_model: 'tpl-fast',
		ollama_url: 'http://gpu-box:11434',
		repo: 'acme/widget.git',
		auto_compact_threshold: '95',
		max_context_tokens: '250000',
		image_tag: '20260101-120000',
		auto_mode: 'on',
	};
	// Mirrors applyTemplateToForm: pull the reusable fields off the template,
	// but never its own name/description.
	const values = {
		backend: template.backend, model: template.model, fast_model: template.fast_model,
		ollama_url: template.ollama_url, repo: template.repo,
		auto_compact_threshold: template.auto_compact_threshold,
		max_context_tokens: template.max_context_tokens,
		image_tag: template.image_tag, auto_mode: template.auto_mode,
	};
	const html = Render.renderCreateForm(
		{ agentImage: Render.harnessImageTags({ harnesses: { 'claude-code': { default_tag: 'latest', tags: [{ tag: '20260101-120000', present: true }] } } }) },
		{ values: values });

	assert.match(html, /class="create-form__model" type="text" value="tpl-opus"/);
	assert.match(html, /class="create-form__fast-model" type="text" value="tpl-fast"/);
	assert.match(html, /class="create-form__ollama-url" type="text" value="http:\/\/gpu-box:11434" placeholder=/);
	assert.match(html, /class="create-form__repo" type="text" value="acme\/widget.git"/);
	assert.match(html, /class="create-form__auto-compact" type="text" value="95"/);
	assert.match(html, /class="create-form__max-context-tokens" type="text" value="250000"/);
	assert.match(html, /<option value="20260101-120000" selected>/);
	assert.match(html, /<option value="on" selected>On<\/option>/);
	// The template's OWN name/description must never appear on the form.
	assert.match(html, /class="create-form__name" type="text" value=""/);
	assert.match(html, /class="create-form__description" type="text" value=""/);
	assert.doesNotMatch(html, /my template/);
	assert.doesNotMatch(html, /template description/);
});

// --- template bar ------------------------------------------------------

test('renderTemplateBar: empty state shows only the blank option', () => {
	const html = Render.renderTemplateBar([]);
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.deepEqual(opts, ['<option value="">— Select a template —</option>']);
	assert.match(html, /class="template-bar__save btn btn--ghost" type="button">Save as template<\/button>/);
	assert.match(html, /class="template-bar__delete btn btn--danger" type="button" hidden>Delete<\/button>/);
});

test('renderTemplateBar: null/undefined also renders the empty state', () => {
	assert.match(Render.renderTemplateBar(null), /template-bar__select/);
	assert.match(Render.renderTemplateBar(undefined), /template-bar__select/);
});

test('renderTemplateBar: renders one <option> per template, alphabetically sorted by name', () => {
	const templates = [
		{ id: 'tpl_c', name: 'Charlie' },
		{ id: 'tpl_a', name: 'Alpha' },
		{ id: 'tpl_b', name: 'Bravo' },
	];
	const html = Render.renderTemplateBar(templates);
	const aIndex = html.indexOf('tpl_a');
	const bIndex = html.indexOf('tpl_b');
	const cIndex = html.indexOf('tpl_c');
	assert.ok(aIndex >= 0 && bIndex > aIndex && cIndex > bIndex, 'expected Alpha, Bravo, Charlie in order, got: ' + html);
});

test('renderTemplateBar: a template name containing HTML is escaped', () => {
	const html = Render.renderTemplateBar([{ id: 'tpl_x', name: '<script>alert(1)</script>' }]);
	assert.doesNotMatch(html, /<script>/);
	assert.match(html, /&lt;script&gt;/);
});

test('renderTemplateBar: an unnamed template shows a placeholder label', () => {
	const html = Render.renderTemplateBar([{ id: 'tpl_x', name: '' }]);
	assert.match(html, /\(unnamed template\)/);
});
