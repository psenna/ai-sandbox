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

test('renderAgentListItem: harness badge on the name row, backend on the meta row', () => {
	const html = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running', backend: 'anthropic', harness: 'opencode' });

	// Harness leads, in the same row as the name; the backend moved to the row
	// below it.
	const row = html.slice(html.indexOf('agent-item__row'), html.indexOf('agent-item__meta'));
	const meta = html.slice(html.indexOf('agent-item__meta'));
	assert.match(row, /agent-item__harness/);
	assert.match(row, /opencode/);
	assert.doesNotMatch(row, /agent-item__backend/);
	assert.match(meta, /agent-item__backend/);
	assert.match(meta, /Anthropic/);
});

test('renderAgentListItem: harness badge falls back to claude-code when the field is absent', () => {
	// agent.harness is omitempty in the API payload, so a record written before
	// the field existed arrives empty and must read as claude-code, not blank.
	for (const a of [
		{ id: 'agt_a', name: 'A', status: 'running' },
		{ id: 'agt_b', name: 'B', status: 'running', harness: '' },
	]) {
		const row = Render.renderAgentListItem(a).slice(0, Render.renderAgentListItem(a).indexOf('agent-item__meta'));
		assert.match(row, /agent-item__harness/);
		assert.match(row, /Claude Code/);
	}
});

test('renderAgentListItem: harness badge renders claude-code explicitly', () => {
	const html = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running', harness: 'claude-code' });
	assert.match(html, /agent-item__harness[^>]*>Claude Code</);
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

test('renderAgentListItem: upgrade marker present only when upgrade_available is truthy, and before the harness badge', () => {
	const withUpgrade = Render.renderAgentListItem({ id: 'agt_a', name: 'A', status: 'running', backend: 'ollama', upgrade_available: true });
	assert.match(withUpgrade, /agent-item__upgrade/);
	assert.ok(
		withUpgrade.indexOf('agent-item__upgrade') < withUpgrade.indexOf('agent-item__harness'),
		'upgrade marker should sit before the harness badge',
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

test('renderImageTagSelect: orders by tag name newest-first, de-dupes, and labels the default in place', () => {
	const html = Render.renderImageTagSelect('c', [
		{ tag: '20251231-090000', present: true },
		{ tag: '20260101-120000', present: false },
		{ tag: '20251231-090000', present: true },
		{ tag: 'latest', present: true },
	], 'latest', '');
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.equal(opts.length, 3, 'no repeats: ' + html);
	// Descending by tag name, whatever order they arrived in. For the
	// :YYYYMMDD-HHMMSS tags this repo uses that is newest-first. The default
	// is labelled in place rather than hoisted -- "latest" simply sorts first
	// here because letters sort above digits.
	assert.equal(opts[0], '<option value="latest" selected>latest (default)</option>');
	assert.equal(opts[1], '<option value="20260101-120000" data-pull-required="true">20260101-120000 (pull required)</option>');
	assert.equal(opts[2], '<option value="20251231-090000">20251231-090000</option>');
});

test('renderImageTagSelect: a default the harness does not offer still sorts into place', () => {
	// The appended default/selected entries are by definition not in the
	// server's list; they must not sink to the bottom just for that.
	const html = Render.renderImageTagSelect('c', ['20260101-120000'], '20260909-090909', '');
	const opts = html.match(/<option[^>]*>[^<]*<\/option>/g);
	assert.equal(opts[0], '<option value="20260909-090909" selected data-pull-required="true">20260909-090909 (default, pull required)</option>');
	assert.equal(opts[1], '<option value="20260101-120000">20260101-120000</option>');
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

test('renderAgentImagePanel: one column per harness, claude-code first, with three action buttons', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: {
			'claude-code': { newest: '20260101-120000', tags: [{ tag: '20260101-120000', present: true }], checked_at: new Date().toISOString() },
			opencode: { newest: '20260202-020202', tags: [{ tag: '20260202-020202', present: false }], checked_at: new Date().toISOString() },
		},
	}));
	assert.match(html, /data-harness="claude-code"/);
	assert.match(html, /data-harness="opencode"/);
	assert.match(html, /Claude Code/);
	assert.match(html, /Newest published: 20260101-120000/);
	assert.match(html, /Newest published: 20260202-020202/);
	assert.ok(html.indexOf('data-harness="claude-code"') < html.indexOf('data-harness="opencode"'),
		'claude-code column should come first: ' + html);
	// One shared actions row: all three actions cover every harness in a
	// single call each.
	assert.match(html, /agent-image-panel__harnesses/);
	for (const cls of ['refresh', 'cleanup', 'pull']) {
		assert.equal(html.match(new RegExp('agent-image-panel__' + cls, 'g')).length, 1,
			'exactly one ' + cls + ' button');
	}
	assert.ok(html.indexOf('agent-image-panel__refresh') < html.indexOf('agent-image-panel__cleanup'),
		'buttons in order: Check now, Cleanup, Pull latest');
	assert.ok(html.indexOf('agent-image-panel__cleanup') < html.indexOf('agent-image-panel__pull'),
		'buttons in order: Check now, Cleanup, Pull latest');
	// The actions row precedes the columns.
	assert.ok(html.indexOf('agent-image-panel__actions') < html.indexOf('agent-image-panel__harnesses'),
		'the actions row sits above the harness columns');
	assert.equal((html.match(/checked just now/g) || []).length, 2);
});

test('renderAgentImagePanel: only PRESENT tags get a delete button, and each carries its harness and tag', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: {
			'claude-code': {
				default_tag: '20260101-120000',
				newest: '20260101-120000',
				tags: [
					{ tag: '20260101-120000', present: true },
					{ tag: '20251231-090000', present: true },
					{ tag: '20260303-030303', present: false },
				],
			},
		},
	}));
	assert.equal((html.match(/agent-image-panel__delete/g) || []).length, 2,
		'one Delete per present tag; a not-yet-pulled published tag gets none');
	assert.match(html, /data-harness="claude-code" data-tag="20260101-120000"/);
	assert.match(html, /data-tag="20251231-090000"/);
	assert.doesNotMatch(html, /data-tag="20260303-030303"/);
	// The badges: the newest, and the operator default.
	assert.match(html, /20260101-120000 <span class="agent-image-panel__tag-note">\(newest, default\)<\/span>/);
	assert.doesNotMatch(html, /20251231-090000 <span/);
});

test('renderAgentImagePanel: a harness with nothing on the server says so, with no table', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: { 'claude-code': { newest: '20260202-020202', tags: [{ tag: '20260202-020202', present: false }] } },
	}));
	assert.match(html, /No image tags on the server/);
	assert.doesNotMatch(html, /agent-image-panel__tags/);
});

test('renderAgentImagePanel: opts.busy disables every button and paints the label', () => {
	const html = Render.renderAgentImagePanel(Render.harnessImageTags({
		harnesses: { 'claude-code': { tags: [{ tag: '20260101-120000', present: true }] } },
	}), { busy: { label: 'Pulling…' } });
	assert.match(html, /agent-image-panel__busy">Pulling…</);
	// Every action button AND every per-tag delete: none must be clickable
	// while an action is in flight.
	assert.equal((html.match(/type="button" disabled/g) || []).length, 4);
});

test('renderAgentImagePanel: opts.error and opts.note paint their lines', () => {
	const base = Render.harnessImageTags({ harnesses: {} });
	const withErr = Render.renderAgentImagePanel(base, { error: 'boom' });
	assert.match(withErr, /agent-image-panel__error">boom</);
	const withNote = Render.renderAgentImagePanel(base, { note: 'Removed 2 tags' });
	assert.match(withNote, /agent-image-panel__note">Removed 2 tags</);
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

test('renderAgentImagePanel: a per-harness last error renders inside that harness\'s column only', () => {
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
	assert.match(html, /Newest published: 20260101-120000/);
});

test('renderAgentImagePanel: accepts the raw API body as well as an already-mapped map', () => {
	const raw = { harnesses: { 'claude-code': { newest: '20260101-120000', tags: [] } } };
	assert.equal(Render.renderAgentImagePanel(raw), Render.renderAgentImagePanel(Render.harnessImageTags(raw)));
});

test('renderAgentImagePanel: missing input does not throw and still renders the three buttons', () => {
	assert.doesNotThrow(() => Render.renderAgentImagePanel());
	const html = Render.renderAgentImagePanel();
	for (const cls of ['refresh', 'cleanup', 'pull']) {
		assert.match(html, new RegExp('agent-image-panel__' + cls));
	}
});

test('renderAgentImagePanel: a harness key and a tag containing HTML are escaped', () => {
	const html = Render.renderAgentImagePanel({ '"><img src=x>': { tags: [{ tag: '"><script>', present: true }] } });
	assert.doesNotMatch(html, /<img src=x>/);
	assert.doesNotMatch(html, /<script>/);
});

// --- agentImageReportNote: the one-line summary of an action's report ------

test('agentImageReportNote: summarizes a cleanup report by removed count', () => {
	const note = Render.agentImageReportNote({ report: [
		{ harness: 'claude-code', removed: ['20260101-000000', 'latest'] },
		{ harness: 'opencode', removed: ['20260102-000000'] },
	] });
	assert.equal(note, 'Removed 3 tags');
});

test('agentImageReportNote: summarizes a pull report by harness', () => {
	const note = Render.agentImageReportNote({ report: [
		{ harness: 'claude-code', tag: '20260101-000000' },
		{ harness: 'opencode', tag: '20260102-000000' },
	] });
	assert.equal(note, 'Claude Code pulled 20260101-000000 · opencode pulled 20260102-000000');
});

test('agentImageReportNote: a failing harness rides alongside the successes', () => {
	const note = Render.agentImageReportNote({ report: [
		{ harness: 'claude-code', removed: ['20260101-000000'] },
		{ harness: 'opencode', error: 'registry is unreachable' },
	] });
	assert.equal(note, 'Removed 1 tag · opencode: registry is unreachable');
});

test('agentImageReportNote: no report (refresh, single delete) yields null', () => {
	assert.equal(Render.agentImageReportNote({ harnesses: {} }), null);
	assert.equal(Render.agentImageReportNote(null), null);
	assert.equal(Render.agentImageReportNote({ report: [] }), null);
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
	// The card's class list now starts with choice-card, so the hidden
	// attribute is anchored by the class NAME, not by the whole attribute.
	assert.match(html, /create-form__backend-anthropic"[^>]*hidden/);
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
	// Anchored by class NAME: the card's own attribute now begins with
	// choice-card, and anchoring on class="create-form__backend-anthropic"
	// would match nothing and pass vacuously.
	assert.doesNotMatch(html, /create-form__backend-anthropic"[^>]*hidden/);
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

// --- the Settings overlay ---------------------------------------------------

test('renderSettingsOverlay: a dialog panel with one nav item and one section per entry', () => {
	const html = Render.renderSettingsOverlay();
	assert.match(html, /settings-overlay__panel" role="dialog" aria-modal="true"/);
	assert.match(html, /settings-overlay__title[^>]*>Settings</);
	assert.match(html, /class="settings-overlay__close btn btn--ghost btn--sm" type="button">Close</);
	assert.equal((html.match(/data-settings-nav="/g) || []).length, 2);
	assert.equal((html.match(/data-settings-section="/g) || []).length, 2);
	// The bodies start empty -- app.js fills them -- so the panel never depends
	// on a fetch having happened.
	assert.match(html, /data-settings-body="anthropic-account"><\/div>/);
	assert.match(html, /data-settings-body="agent-image"><\/div>/);
});

test('renderSettingsOverlay: the first section is shown and the rest hidden, with the nav in step', () => {
	const html = Render.renderSettingsOverlay();
	// Every section is rendered, inactive ones hidden: the poll-driven
	// refreshes look their body up by id, so a section that only existed once
	// its nav item was clicked would silently go stale.
	assert.match(html, /<section class="settings-overlay__section" data-settings-section="anthropic-account">/);
	assert.match(html, /<section class="settings-overlay__section" hidden data-settings-section="agent-image">/);
	assert.match(html, /data-settings-nav="anthropic-account" aria-current="true"/);
	assert.match(html, /data-settings-nav="agent-image" aria-current="false"/);
	assert.match(html, /settings-overlay__nav-item settings-overlay__nav-item--active"[^>]*data-settings-nav="anthropic-account"/);
});

test('renderSettingsOverlay: nav items and section bodies stay addressable in order', () => {
	const html = Render.renderSettingsOverlay();
	assert.ok(
		html.indexOf('data-settings-body="anthropic-account"') < html.indexOf('data-settings-body="agent-image"'),
		'expected the Anthropic section before the Agent image one',
	);
	assert.ok(
		html.indexOf('data-settings-nav="anthropic-account"') < html.indexOf('data-settings-nav="agent-image"'),
		'expected the nav in the same order as the sections',
	);
});

test('renderAgentImagePanel: opts.noTitle suppresses only the built-in title', () => {
	const map = { 'claude-code': { repo: 'ghcr.io/x/y', tags: [{ tag: '20260101-000000', present: true }] } };
	const withTitle = Render.renderAgentImagePanel(map);
	assert.match(withTitle, /agent-image-panel__title/);
	assert.match(withTitle, /agent-image-panel__refresh/);

	const noTitle = Render.renderAgentImagePanel(map, { noTitle: true });
	assert.doesNotMatch(noTitle, /agent-image-panel__title/);
	// Everything else is unchanged: the per-harness block and the button.
	assert.match(noTitle, /data-harness="claude-code"/);
	assert.match(noTitle, /agent-image-panel__refresh/);
});

// --- change #3: the sidebar-vs-main-area refresh policy ----------------------

test('agentChangePolicy: create never resets the main area', () => {
	assert.equal(Render.agentChangePolicy('create', null, 'agt_a'), 'sidebar-only');
	assert.equal(Render.agentChangePolicy('create', 'agt_a', 'agt_a'), 'sidebar-only');
});

test('agentChangePolicy: delete resets the main area only when that agent is selected', () => {
	assert.equal(Render.agentChangePolicy('delete', 'agt_a', 'agt_a'), 'reset-main-area');
	assert.equal(Render.agentChangePolicy('delete', 'agt_a', 'agt_b'), 'sidebar-only');
	// Deleting while nothing is selected (e.g. from Settings, which clears the
	// selection) must leave whatever is on screen alone.
	assert.equal(Render.agentChangePolicy('delete', null, 'agt_a'), 'sidebar-only');
	assert.equal(Render.agentChangePolicy('delete', undefined, 'agt_a'), 'sidebar-only');
});

test('agentChangePolicy: an unknown kind degrades to sidebar-only', () => {
	assert.equal(Render.agentChangePolicy('update', 'agt_a', 'agt_a'), 'sidebar-only');
	assert.equal(Render.agentChangePolicy('', 'agt_a', 'agt_a'), 'sidebar-only');
});

// --- change #2: the create form's Anthropic note points at Settings ----------

test('renderCreateForm: the Anthropic note points at Settings, not a sidebar panel', () => {
	const html = Render.renderCreateForm({ backend: 'anthropic' });
	assert.match(html, /set it in Settings first/);
	assert.doesNotMatch(html, /in the sidebar first/);
});

// --- form restructure: decision order, choice cards, Advanced --------------

test('renderCreateForm: harness and backend render as two cards each, with descriptions', () => {
	const html = Render.renderCreateForm();
	// The card LABELS specifically -- `class="choice-card"` or
	// `class="choice-card <modifier>"`, never `class="choice-card__body"`.
	assert.equal((html.match(/class="choice-card[" ]/g) || []).length, 4);
	assert.match(html, /choice-grid/);
	assert.match(html, /choice-card__title">Claude Code</);
	assert.match(html, /choice-card__desc">Anthropic · Ollama</);
	assert.match(html, /choice-card__title">opencode</);
	assert.match(html, /choice-card__desc">Ollama only</);
	assert.match(html, /choice-card__title">Ollama</);
	assert.match(html, /choice-card__title">Anthropic account</);
});

// The card's selected state is painted by `input:checked + .choice-card__body`,
// a SIBLING selector. If the body ever stops being the input's immediate next
// sibling the card silently renders unselected forever -- worth its own test.
test('renderCreateForm: every card body is the immediate sibling of its radio', () => {
	const html = Render.renderCreateForm();
	for (const [name, value] of [
		['harness', 'claude-code'],
		['harness', 'opencode'],
		['backend', 'ollama'],
		['backend', 'anthropic'],
	]) {
		const re = new RegExp(`<input type="radio" name="${name}" value="${value}"[^>]*><span class="choice-card__body">`);
		assert.match(html, re, `${name}=${value} card body must follow its input directly`);
	}
});

test('renderCreateForm: fields render in decision order, harness first', () => {
	const html = Render.renderCreateForm();
	const at = (s) => html.indexOf(s);
	assert.ok(at('create-form__harness') < at('create-form__name'), 'harness must precede name');
	assert.ok(at('create-form__name') < at('name="backend"'), 'identity must precede the backend choice');
	assert.ok(at('name="backend"') < at('create-form__advanced'), 'backend must precede Advanced');
	assert.ok(at('create-form__advanced') < at('create-form__actions'), 'Advanced must precede the action bar');
});

test('renderCreateForm: Identity and Model render as titled sections', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /create-form__section-heading">Identity</);
	assert.match(html, /create-form__section-heading">Model</);
});

// The invariant that keeps infraFieldsFromForm and both submit paths working:
// a closed <details> still contains its fields in a parsed-as-HTML sense only
// for real DOM, but the MARKUP must always carry them -- they are read with
// unguarded .value reads, so conditionally omitting them would throw.
test('renderCreateForm: Advanced starts closed but still renders every advanced field', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /<details class="create-form__advanced">/);
	for (const cls of ['create-form__image-tag', 'create-form__auto-compact', 'create-form__max-context-tokens', 'create-form__auto-mode']) {
		assert.match(html, new RegExp(cls), `${cls} must be present even while Advanced is closed`);
	}
});

test('renderCreateForm: opts.advancedOpen opens the disclosure', () => {
	assert.match(Render.renderCreateForm({}, { advancedOpen: true }), /<details class="create-form__advanced" open>/);
});

test('renderCreateForm: sentence help lives in help lines, not placeholders', () => {
	const html = Render.renderCreateForm();
	assert.match(html, /create-form__help">owner\/repo\.git — blank for a bare terminal</);
	assert.doesNotMatch(html, /placeholder="Claude Code/);
	assert.doesNotMatch(html, /placeholder="owner\/repo\.git/);
});

test('renderCreateForm: the error line renders above the action bar', () => {
	const html = Render.renderCreateForm();
	assert.ok(html.indexOf('create-form__error') < html.indexOf('create-form__actions'));
});
