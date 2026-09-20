// render.js -- pure functions that turn API data into HTML strings.
//
// No `document`/`window`/DOM access anywhere in this file, on purpose: this
// is the half of the UI issue #75's "DOM/component-level test" exercises
// directly with plain Node (no browser, no jsdom, no build step) -- the
// rest of the DOM wiring (event listeners, fetch calls) lives in app.js and
// is reviewed by hand rather than unit tested, since it has nothing left to
// test once this half is correct.
//
// Exposed as a plain global (window.Render) for the browser, and via
// module.exports for render.test.js -- a hand-written substitute for a
// bundler's dual CJS/browser output, appropriate for a project with no
// build pipeline at all.
(function (global) {
	'use strict';

	var ESCAPE_MAP = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

	// escapeHTML makes a string safe to interpolate into an HTML string.
	// Every piece of agent-supplied data (name, description, id) goes
	// through this before it touches a template -- an agent's name is
	// user-editable free text, not something to trust.
	function escapeHTML(s) {
		return String(s).replace(/[&<>"']/g, function (c) { return ESCAPE_MAP[c]; });
	}

	// STATUS_LABELS mirrors docker-operator/internal/store.Status's five
	// constants (creating/running/stopped/error/deleting). Keep this in sync
	// if that set ever changes.
	var STATUS_LABELS = {
		creating: { text: 'Creating', cls: 'status-creating' },
		running: { text: 'Running', cls: 'status-running' },
		stopped: { text: 'Stopped', cls: 'status-stopped' },
		error: { text: 'Error', cls: 'status-error' },
		deleting: { text: 'Deleting', cls: 'status-deleting' },
	};

	// statusLabel maps a status string to a short human label plus a CSS
	// class for its status dot. An unrecognised value (a future status this
	// build of the UI doesn't know about yet) degrades to a visible
	// "Unknown" rather than throwing or rendering blank.
	function statusLabel(status) {
		return STATUS_LABELS[status] || { text: status || 'Unknown', cls: 'status-unknown' };
	}

	// renderAgentListItem renders one sidebar <li> for a single agent, as two
	// rows: the name/badges row, then its one status line. selectedID may be
	// null/undefined; it is compared with === so no agent matches unless one
	// is actually selected.
	//
	// isUnread (app.js's state.unread) means this agent finished a turn
	// (agent.activity flipped working -> waiting) while it was not the open
	// agent, and hasn't been opened since -- it wins over a live "working"
	// signal so the row never shows two conflicting status lines: a
	// just-finished agent reads as "come look", not "still going".
	function renderAgentListItem(agent, selectedID, isUnread) {
		var label = statusLabel(agent.status);
		var selected = agent.id === selectedID ? ' agent-item--selected' : '';
		var name = agent.name ? escapeHTML(agent.name) : '(unnamed)';
		// upgrade_ready is the stronger signal (a newer image of this agent's
		// own harness is ALREADY on this host, so the update needs no pull) and
		// wins the marker's modifier. It is not a subset of upgrade_available
		// -- see agentView's doc comment -- so either flag alone shows a marker.
		var upgrade = '';
		if (agent.upgrade_ready) {
			upgrade = '<span class="agent-item__upgrade agent-item__upgrade--ready" title="a newer agent image is already on this host">⬆</span>';
		} else if (agent.upgrade_available) {
			upgrade = '<span class="agent-item__upgrade" title="a newer agent image is available">⬆</span>';
		}

		var dotCls = label.cls;
		var activityCls = '';
		var activityText = escapeHTML(label.text);
		var activityDot = '';
		if (isUnread) {
			activityCls = ' agent-item__activity--unread';
			activityText = 'Waiting for you';
			activityDot = '<span class="agent-item__activity-dot"></span>';
		} else if (agent.activity === 'working') {
			dotCls += ' status-dot--pulse';
			activityCls = ' agent-item__activity--working';
			activityText = 'Working…';
		}

		return (
			'<li class="agent-item' + selected + (isUnread ? ' agent-item--unread' : '') + '" data-agent-id="' + escapeHTML(agent.id) + '">' +
				'<div class="agent-item__row">' +
					'<span class="status-dot ' + dotCls + '" title="' + label.text + '"></span>' +
					'<span class="agent-item__name">' + name + '</span>' +
					upgrade +
					'<span class="agent-item__backend" title="backend">' + escapeHTML(backendLabel(agent.backend)) + '</span>' +
				'</div>' +
				'<div class="agent-item__meta">' +
					'<span class="agent-item__activity' + activityCls + '">' + activityDot + activityText + '</span>' +
				'</div>' +
			'</li>'
		);
	}

	// renderAgentList renders the sidebar's full <li> list, in the order the
	// API returned it, or a one-line empty state when there are no agents.
	// unreadIds (app.js's state.unread) is a plain {id: true} map; absent/
	// missing entries mean "not unread", so passing nothing is the same as
	// passing an empty list.
	function renderAgentList(agents, selectedID, unreadIds) {
		if (!agents || agents.length === 0) {
			return '<li class="agent-list__empty">No agents yet — click “New Agent” to create one.</li>';
		}
		return agents
			.map(function (a) { return renderAgentListItem(a, selectedID, !!(unreadIds && unreadIds[a.id])); })
			.join('');
	}

	// renderCapacity renders the sidebar footer's "N of M agents" text.
	function renderCapacity(agents, maxAgents) {
		var count = agents ? agents.length : 0;
		return count + ' of ' + maxAgents + ' agent' + (maxAgents === 1 ? '' : 's');
	}

	// backendLabel maps a backend id (config.BackendOllama /
	// config.BackendAnthropic, or "" on an old record) to a short label.
	function backendLabel(backend) {
		if (backend === 'anthropic') return 'Anthropic';
		if (backend === 'ollama' || !backend) return 'Ollama';
		return backend;
	}

	// harnessSupportsBackend mirrors config.HarnessSupportsBackend: opencode is
	// Ollama-only in v1; claude-code runs on either backend.
	function harnessSupportsBackend(harness, backend) {
		return harness !== 'opencode' || backend !== 'anthropic';
	}

	// HARNESSES mirrors config.Harnesses(): the set GET /api/agent-image/tags
	// always returns one entry per. The order is load-bearing -- it is the
	// order renderAgentImagePanel renders its per-harness blocks in.
	var HARNESSES = ['claude-code', 'opencode'];

	var HARNESS_LABELS = { 'claude-code': 'Claude Code', 'opencode': 'opencode' };

	// normalizeHarness mirrors config.NormalizeHarness: only "opencode" is
	// opencode; everything else -- including "" on a record written before the
	// field existed -- is claude-code.
	function normalizeHarness(harness) {
		return harness === 'opencode' ? 'opencode' : 'claude-code';
	}

	// harnessLabel maps a harness id to the label the UI shows for it.
	function harnessLabel(harness) {
		return HARNESS_LABELS[harness] || String(harness || '');
	}

	// renderCreateForm renders the "New Agent" form -- also the "Update agent"
	// form when opts.{title,submitLabel} say so. defaults pre-fills the backend
	// choice, the Ollama server + two model fields and the repo from the
	// operator's configuration (GET /api/agents' default_* fields). The Ollama
	// server field is left blank with the operator's default shown as its
	// placeholder, so submitting it untouched means "use the operator default".
	//
	// defaults.agentImage is a harnessImageTags by-harness map ({claude-code:
	// {repo, defaultTag, newest, tags, checkedAt, lastError}, opencode: {...}}).
	// The image-tag row is sourced from whichever harness is selected right
	// now (see the `harness`/`imageInfo` locals below) -- each harness has its
	// own image repository and its own default tag, so the row is re-derived
	// per render rather than shared.
	//
	// opts.values, when given (the update form passes the agent record),
	// OVERRIDES those defaults field by field so the form opens pre-filled with
	// the agent's current backend/model/fast_model/ollama_url/repo/
	// auto_compact_threshold/max_context_tokens and its current image tag. The
	// caller wires the backend radio to show/hide .create-form__ollama and
	// submits the form's values to POST /api/agents (or .../{id}/update).
	//
	// Auto mode is the one field NOT merged with pick(): values.auto_mode (an
	// agent record) is always already resolved to "on"/"off", never "", so
	// the update form opens on the agent's actual current setting rather than
	// re-showing "operator default" for an agent that explicitly chose one.
	//
	// A new Harness fieldset (values.harness, defaulting to "claude-code" --
	// there is no operator-wide default) sits just above the Backend fieldset.
	// Selecting/opening on harness=opencode forces backend to "ollama" and
	// renders the Anthropic radio hidden+disabled (mirrors
	// config.HarnessSupportsBackend -- opencode is Ollama-only in v1), with an
	// explanatory note shown in its place; the caller mirrors this rule on
	// `change` events for live toggling, but the initial render already gets
	// it right, which is what makes this unit-testable here. opts.harnessLocked
	// renders the harness radios disabled (used by the update form, since
	// internal/agent's Update never changes a persisted agent's harness) and
	// appends "(set at create time)" to the legend, without hiding or omitting
	// the fieldset.
	function renderCreateForm(defaults, opts) {
		defaults = defaults || {};
		opts = opts || {};
		var title = escapeHTML(opts.title || 'New agent');
		var submitLabel = escapeHTML(opts.submitLabel || 'Create');
		var values = opts.values || {};
		// values.* (the agent record, snake_case) wins over defaults.* (the
		// operator config) so an update form opens on the agent's own settings.
		var pick = function (v, d) { return (v === undefined || v === null || v === '') ? (d || '') : v; };
		// Harness has no operator-wide default (GET /api/agents has no
		// default_harness, and internal/agent resolves an empty harness to
		// claude-code), so unlike backend this reads opts.values only -- the
		// update form's agent record -- and otherwise falls back to claude-code.
		var harness = values.harness === 'opencode' ? 'opencode' : 'claude-code';
		var opencode = harness === 'opencode';
		var backend = (values.backend || defaults.backend) === 'anthropic' ? 'anthropic' : 'ollama';
		// Mirrors config.HarnessSupportsBackend: opencode is Ollama-only in v1,
		// so an opencode form can never open on the anthropic backend even if a
		// stale record or template said so.
		if (opencode) backend = 'ollama';
		var model = escapeHTML(pick(values.model, defaults.model));
		var fastModel = escapeHTML(pick(values.fast_model, defaults.fastModel));
		var ollamaURL = escapeHTML(defaults.ollamaUrl || '');
		// Only emitted for the update form (opts.values carries a resolved
		// ollama_url); the create form's field stays value-less so submitting
		// it untouched means "use the operator default".
		var ollamaURLAttr = values.ollama_url ? ' value="' + escapeHTML(values.ollama_url) + '"' : '';
		var repo = escapeHTML(pick(values.repo, defaults.repo));
		var autoCompact = escapeHTML(pick(values.auto_compact_threshold, defaults.autoCompactThreshold));
		var maxContextTokens = escapeHTML(pick(values.max_context_tokens, defaults.maxContextTokens));
		// Auto mode is tri-state: "" (use the operator default, shown as the
		// select's first option), "on" or "off". values.auto_mode (the update
		// form's agent record) is already the RESOLVED value, never "" -- so
		// it is only used to pick which option is selected, not merged with
		// defaults the way pick() merges the other fields above.
		var autoModeValue = values.auto_mode || '';
		var operatorAutoModeLabel = defaults.autoMode === 'off' ? 'off' : 'on';
		var selectedImageTag = values.image_tag || imageTagOf(values.image) || '';
		// The image-tag row is keyed by the harness selected RIGHT NOW (the
		// update form's locked harness, or claude-code by default): each
		// harness has its own image repository, its own published tag history
		// and its own default tag, so the options are never shared. app.js
		// re-renders just this select on a harness change (syncImageTagSelect).
		var imageInfo = imageTagsForHarness(defaults.agentImage, harness);
		var ollamaHidden = backend === 'ollama' ? '' : ' hidden';
		var nameValue = escapeHTML(values.name || '');
		var descriptionValue = escapeHTML(values.description || '');
		return (
			'<form class="create-form">' +
				'<h2 class="create-form__title">' + title + '</h2>' +
				'<label class="create-form__row">Name<input class="create-form__name" type="text" value="' + nameValue + '" placeholder="(optional)"></label>' +
				'<label class="create-form__row">Description<input class="create-form__description" type="text" value="' + descriptionValue + '" placeholder="(optional)"></label>' +
				'<label class="create-form__row">Repository' +
					'<input class="create-form__repo" type="text" value="' + repo + '" placeholder="owner/repo.git — blank for a bare terminal">' +
				'</label>' +
				'<label class="create-form__row">Agent image' +
					renderImageTagSelect('create-form__image-tag', imageInfo.tags, imageInfo.defaultTag, selectedImageTag) +
				'</label>' +
				'<label class="create-form__row">Auto-compact threshold' +
					'<input class="create-form__auto-compact" type="text" value="' + autoCompact + '" placeholder="Claude Code auto-compact threshold — blank to use the built-in default">' +
				'</label>' +
				'<label class="create-form__row">Max-context tokens' +
					'<input class="create-form__max-context-tokens" type="text" value="' + maxContextTokens + '" placeholder="Claude Code max-context tokens — blank to use the built-in default">' +
				'</label>' +
				'<label class="create-form__row">Auto mode' +
					'<select class="create-form__auto-mode">' +
						'<option value=""' + (autoModeValue === '' ? ' selected' : '') + '>Operator default (' + operatorAutoModeLabel + ')</option>' +
						'<option value="on"' + (autoModeValue === 'on' ? ' selected' : '') + '>On</option>' +
						'<option value="off"' + (autoModeValue === 'off' ? ' selected' : '') + '>Off</option>' +
					'</select>' +
				'</label>' +
				'<fieldset class="create-form__row create-form__harness">' +
					'<legend>Harness' + (opts.harnessLocked ? ' (set at create time)' : '') + '</legend>' +
					'<label><input type="radio" name="harness" value="claude-code"' +
						(harness === 'claude-code' ? ' checked' : '') + (opts.harnessLocked ? ' disabled' : '') + '> Claude Code</label>' +
					'<label><input type="radio" name="harness" value="opencode"' +
						(opencode ? ' checked' : '') + (opts.harnessLocked ? ' disabled' : '') + '> opencode</label>' +
				'</fieldset>' +
				'<fieldset class="create-form__row create-form__backend">' +
					'<legend>Backend</legend>' +
					'<label><input type="radio" name="backend" value="ollama"' + (backend === 'ollama' ? ' checked' : '') + '> Ollama</label>' +
					'<label class="create-form__backend-anthropic"' + (opencode ? ' hidden' : '') + '>' +
						'<input type="radio" name="backend" value="anthropic"' +
						(backend === 'anthropic' ? ' checked' : '') + (opencode ? ' disabled' : '') +
						'> Anthropic account</label>' +
				'</fieldset>' +
				'<div class="create-form__ollama"' + ollamaHidden + '>' +
					'<label class="create-form__row">Ollama server' +
						'<input class="create-form__ollama-url" type="text"' + ollamaURLAttr + ' placeholder="' + (ollamaURL || 'operator default') + '">' +
					'</label>' +
					'<label class="create-form__row">Opus-tier model<input class="create-form__model" type="text" value="' + model + '"></label>' +
					'<label class="create-form__row">Sonnet &amp; Haiku-tier model<input class="create-form__fast-model" type="text" value="' + fastModel + '"></label>' +
				'</div>' +
				'<p class="create-form__anthropic-note" hidden>Uses the shared Anthropic login (set it in the sidebar first).</p>' +
				'<p class="create-form__opencode-note"' + (opencode ? '' : ' hidden') + '>opencode runs against Ollama only — the Anthropic backend is not available for it.</p>' +
				'<div class="create-form__actions">' +
					'<button class="create-form__submit btn btn--primary" type="submit">' + submitLabel + '</button>' +
					'<button class="create-form__cancel btn btn--ghost" type="button">Cancel</button>' +
				'</div>' +
				'<p class="create-form__error" role="alert" hidden></p>' +
			'</form>'
		);
	}

	// renderTemplateBar renders the template picker sitting above the
	// create-agent form: a <select> of saved templates (GET /api/templates),
	// a context-aware save/update button, and a delete button. It is a
	// separate root element from renderCreateForm's own markup -- inserted
	// as a sibling by app.js, never spliced into the form string -- so the
	// update-agent form (terminal.js's openUpdateForm, which also calls
	// renderCreateForm) is untouched by construction.
	//
	// Options are sorted alphabetically by name for display; the store
	// itself stays ID-ordered (the same division of labor renderImageTagSelect
	// already has between server order and display order). An unnamed
	// template (empty Name -- should not normally happen, the API rejects an
	// empty name, but a defensively-rendered placeholder beats a blank
	// <option> a user can't click) shows a placeholder label.
	function renderTemplateBar(templates) {
		var sorted = (templates || []).slice().sort(function (a, b) {
			var an = (a && a.name) || '';
			var bn = (b && b.name) || '';
			return an < bn ? -1 : an > bn ? 1 : 0;
		});
		var options = '<option value="">— Select a template —</option>' +
			sorted.map(function (t) {
				var label = t.name ? escapeHTML(t.name) : '(unnamed template)';
				return '<option value="' + escapeHTML(t.id) + '">' + label + '</option>';
			}).join('');
		return (
			'<div class="template-bar">' +
				'<label class="create-form__row template-bar__row">Template' +
					'<select class="template-bar__select">' + options + '</select>' +
				'</label>' +
				'<div class="template-bar__actions">' +
					'<button class="template-bar__save btn btn--ghost" type="button">Save as template</button>' +
					'<button class="template-bar__delete btn btn--danger" type="button" hidden>Delete</button>' +
				'</div>' +
				'<p class="template-bar__error" role="alert" hidden></p>' +
			'</div>'
		);
	}

	// renderAgentInfo renders the "Agent info" overlay's body from
	// GET /api/agents/{id}/info's body ({agent, operator}): two sections —
	// the agent's identity (its immutable ID) and resolved create-time
	// parameters (what the New Agent form sent, or what the operator
	// defaults filled in for it), and the operator-level parameters that
	// apply to every agent. A blank parameter renders its placeholder (— or
	// "built-in default") instead of an empty cell, so a reader can tell
	// "not set" from a rendering bug.
	// shortImageID renders a Docker image ID ("sha256:abcdef0123...") the way
	// `docker images`/`docker ps` do: the algorithm prefix stripped, first 12
	// hex chars. Falsy/unrecognised input renders as '' so the caller's blank
	// placeholder kicks in instead of a garbled partial string.
	function shortImageID(id) {
		var s = String(id || '');
		var hex = s.indexOf(':') >= 0 ? s.slice(s.indexOf(':') + 1) : s;
		return hex ? hex.slice(0, 12) : '';
	}

	function renderAgentInfo(info) {
		info = info || {};
		var agent = info.agent || {};
		var operator = info.operator || {};

		function row(label, value, blank) {
			var cls = 'agent-info__value' + (value ? '' : ' agent-info__value--blank');
			return '<dt class="agent-info__label">' + label + '</dt>' +
				'<dd class="' + cls + '">' + escapeHTML(value || blank) + '</dd>';
		}

		return (
			'<div class="agent-info">' +
				'<section class="agent-info__section">' +
					'<h3 class="agent-info__heading">Agent</h3>' +
					'<dl class="agent-info__list">' +
						row('ID', agent.id, '—') +
						row('Name', agent.name, '(unnamed)') +
						row('Description', agent.description, '—') +
						row('Backend', agent.backend ? backendLabel(agent.backend) : '', '—') +
						row('Model', agent.model, '—') +
						row('Fast model', agent.fast_model, '—') +
						row('Ollama server', agent.ollama_url, '—') +
						row('Repository', agent.repo, '—') +
						row('Auto-compact threshold', agent.auto_compact_threshold, 'built-in default') +
						row('Max-context tokens', agent.max_context_tokens, 'built-in default') +
						row('Auto mode', agent.auto_mode === 'on' ? 'On' : agent.auto_mode === 'off' ? 'Off' : '', '—') +
						// This agent's OWN resolved image (which tag it was created/
						// updated against, and the concrete image ID that tag pointed
						// at on the daemon at the time) -- distinct from the
						// "Operator" section's "Agent image" row below, which is
						// always the operator-wide configured default and does not
						// change per agent.
						row('Image', agent.image, '(operator default)') +
						row('Resolved image ID', shortImageID(agent.image_id), 'unknown') +
					'</dl>' +
				'</section>' +
				'<section class="agent-info__section">' +
					'<h3 class="agent-info__heading">Operator</h3>' +
					'<dl class="agent-info__list">' +
						row('Agent image', operator.agent_image, '—') +
						row('Docker runtime', operator.docker_runtime, '—') +
					'</dl>' +
				'</section>' +
			'</div>'
		);
	}

	// renderAnthropicStatus renders the sidebar Anthropic-account panel's
	// one-line status from GET /api/anthropic/auth's body
	// ({configured, kind, updated_at}).
	function renderAnthropicStatus(status) {
		status = status || {};
		if (!status.configured) {
			return '<span class="anthropic-panel__status anthropic-panel__status--unset">No Anthropic credential</span>';
		}
		var kind = status.kind === 'oauth' ? 'OAuth token' : 'API key';
		var when = status.updated_at ? new Date(status.updated_at) : null;
		var whenText = when && !isNaN(when.getTime()) ? ' · set ' + when.toISOString().slice(0, 10) : '';
		return '<span class="anthropic-panel__status anthropic-panel__status--set">' + escapeHTML(kind) + escapeHTML(whenText) + '</span>';
	}

	// --- agent image tags ---------------------------------------------------

	var DATE_TIME_TAG_RE = /^\d{8}-\d{6}$/;

	// isDateTimeTag mirrors docker-operator/internal/agent.IsDateTimeTag:
	// the immutable :YYYYMMDD-HHMMSS tag the agent-image CI publishes.
	function isDateTimeTag(tag) {
		return DATE_TIME_TAG_RE.test(String(tag || ''));
	}

	// newestDateTimeTag returns the lexically-greatest date-time tag (== the
	// newest, for this format), or '' when there is none.
	function newestDateTimeTag(tags) {
		var newest = '';
		(tags || []).forEach(function (t) {
			if (isDateTimeTag(t) && t > newest) newest = t;
		});
		return newest;
	}

	// upgradeAvailable mirrors docker-operator/internal/agent.UpgradeAvailable:
	// true only when currentTag is a date-time tag AND some entry of tags is a
	// date-time tag lexically greater than it. Exported for tests and other
	// callers; renderAgentListItem trusts the server's upgrade_available field
	// rather than recomputing.
	function upgradeAvailable(currentTag, tags) {
		if (!isDateTimeTag(currentTag)) return false;
		return (tags || []).some(function (t) {
			return isDateTimeTag(t) && t > currentTag;
		});
	}

	// imageTagOf is a best-effort client-side ImageTagOf: the substring after
	// the last ':' ONLY when no '/' follows that ':' (so "host:5000/x" => '',
	// "host/x:tag" => 'tag', "x" => '').
	function imageTagOf(ref) {
		var s = String(ref || '');
		var colon = s.lastIndexOf(':');
		if (colon < 0) return '';
		if (s.indexOf('/', colon) >= 0) return '';
		return s.slice(colon + 1);
	}

	// formatCheckedAgo renders an ISO timestamp as a coarse "checked ..." bucket.
	function formatCheckedAgo(iso) {
		if (!iso) return 'never checked';
		var then = new Date(iso).getTime();
		if (isNaN(then)) return 'never checked';
		var secs = Math.max(0, Math.round((Date.now() - then) / 1000));
		if (secs < 60) return 'checked just now';
		var mins = Math.round(secs / 60);
		if (mins < 60) return 'checked ' + mins + ' minute' + (mins === 1 ? '' : 's') + ' ago';
		var hours = Math.round(mins / 60);
		if (hours < 24) return 'checked ' + hours + ' hour' + (hours === 1 ? '' : 's') + ' ago';
		var days = Math.round(hours / 24);
		return 'checked ' + days + ' day' + (days === 1 ? '' : 's') + ' ago';
	}

	// normalizeImageTagOptions accepts the API's [{tag, present}] list (and
	// tolerates a plain ["tag", ...] array, which carries no presence
	// information -- those count as present, so an old-shaped payload never
	// labels every option "pull required"), drops blanks and de-duplicates by
	// tag KEEPING THE SERVER'S ORDER: internal/agent.OfferedImageTags already
	// orders the list (newest published date-time tags first, then every tag
	// the host holds, then the default), and that ordering is the product
	// decision -- re-sorting it here would float a hand-built local tag above
	// the newest published one.
	function normalizeImageTagOptions(tags) {
		var seen = {};
		var out = [];
		(tags || []).forEach(function (t) {
			var isString = typeof t === 'string';
			var tag = isString ? t : String((t && t.tag) || '');
			if (!tag || seen[tag]) return;
			seen[tag] = true;
			out.push({ tag: tag, present: isString ? true : !!(t && t.present) });
		});
		return out;
	}

	// imageTagNames returns just the tag strings of an options list, for the
	// string-based helpers above (newestDateTimeTag/upgradeAvailable).
	function imageTagNames(tags) {
		return normalizeImageTagOptions(tags).map(function (o) { return o.tag; });
	}

	// harnessImageTags maps GET /api/agent-image/tags' (and POST
	// /api/agent-image/refresh's) body -- {harnesses: {<harness>: {repo,
	// default_tag, newest, tags: [{tag, present}], checked_at, last_error}}} --
	// into the by-harness map the UI holds in state, with camelCase names and
	// normalised options. Every harness of HARNESSES is always present (one the
	// server omitted maps to an empty entry), in HARNESSES order, followed by
	// any harness the server reported that this build doesn't know about -- so
	// a future third harness still renders rather than silently vanishing.
	function harnessImageTags(data) {
		var src = (data && data.harnesses) || {};
		var out = {};
		function put(h) {
			var e = src[h] || {};
			out[h] = {
				repo: e.repo || '',
				defaultTag: e.default_tag || '',
				newest: e.newest || '',
				tags: normalizeImageTagOptions(e.tags),
				checkedAt: e.checked_at || null,
				lastError: e.last_error || '',
			};
		}
		HARNESSES.forEach(put);
		Object.keys(src).forEach(function (h) { if (!out[h]) put(h); });
		return out;
	}

	// imageTagsForHarness picks one harness's entry out of a harnessImageTags
	// map: an exact key match first (so a harness this build doesn't know still
	// works), else the normalizeHarness fallback, else an all-blank entry. It
	// never returns undefined and never returns a half-filled entry, so callers
	// can read .tags/.defaultTag unconditionally.
	function imageTagsForHarness(byHarness, harness) {
		var map = byHarness || {};
		var e = map[harness] || map[normalizeHarness(harness)] || {};
		return {
			repo: e.repo || '',
			defaultTag: e.defaultTag || '',
			newest: e.newest || '',
			tags: normalizeImageTagOptions(e.tags),
			checkedAt: e.checkedAt || null,
			lastError: e.lastError || '',
		};
	}

	// imageTagOffered reports whether tag is one of the options `entry` offers.
	// entry may be a harnessImageTags entry or a bare options array. app.js uses
	// it to decide whether the tag the user picked survives a harness switch:
	// the two repositories publish independent tag histories, so a claude-code
	// tag usually does NOT exist for opencode.
	function imageTagOffered(entry, tag) {
		var t = String(tag || '');
		if (!t) return false;
		var tags = Array.isArray(entry) ? entry : ((entry && entry.tags) || []);
		return tags.some(function (o) {
			return (typeof o === 'string' ? o : String((o && o.tag) || '')) === t;
		});
	}

	// renderImageTagSelect renders a <select> over one harness's offered tags
	// (GET /api/agent-image/tags' [{tag, present}] list for that harness), IN
	// THE SERVER'S ORDER, plus defaultTag and selectedTag appended if the
	// harness does not offer them. A tag the host does not already hold is
	// labelled "pull required" (and carries data-pull-required="true") so the
	// user knows choosing it makes the update wait on a registry pull; the
	// operator default is labelled "(default)" in place, never hoisted.
	// selectedTag (else defaultTag, else "") is the selected option, and every
	// value is escaped.
	function renderImageTagSelect(cls, tags, defaultTag, selectedTag) {
		var def = String(defaultTag || '');
		var selected = String(selectedTag || '');

		var ordered = normalizeImageTagOptions(tags);
		var seen = {};
		ordered.forEach(function (o) { seen[o.tag] = true; });
		// A default or a currently-selected tag the harness does not offer is
		// still shown (and, being absent from the host's inventory, marked
		// "pull required") rather than silently dropped -- an agent pinned to
		// an old tag must keep seeing the tag it is actually on.
		if (def && !seen[def]) { seen[def] = true; ordered.push({ tag: def, present: false }); }
		if (selected && !seen[selected]) { seen[selected] = true; ordered.push({ tag: selected, present: false }); }

		var matched = selected || def;

		var html = '<select class="' + escapeHTML(cls) + '">';
		if (!def) {
			// No default for this harness (its inventory is unavailable, or a
			// digest-pinned image): offer a blank option so "leave it to the
			// operator default" stays submittable.
			html += '<option value=""' + (matched === '' ? ' selected' : '') + '>(operator default)</option>';
		}
		ordered.forEach(function (o) {
			var notes = [];
			if (o.tag === def) notes.push('default');
			if (!o.present) notes.push('pull required');
			var label = notes.length ? o.tag + ' (' + notes.join(', ') + ')' : o.tag;
			html += '<option value="' + escapeHTML(o.tag) + '"' +
				(o.tag === matched ? ' selected' : '') +
				(o.present ? '' : ' data-pull-required="true"') +
				'>' + escapeHTML(label) + '</option>';
		});
		return html + '</select>';
	}

	// renderAgentImagePanel renders the sidebar "Agent image" panel body from a
	// harnessImageTags map: ONE block per harness (each with its own newest
	// tag, its own last-checked time and its own error line -- the two
	// repositories are polled independently, issue #199), under a SINGLE
	// "Check now" button, since POST /api/agent-image/refresh refreshes every
	// harness in one call. The raw API body is accepted too, so a caller that
	// hasn't mapped it yet still renders.
	function renderAgentImagePanel(byHarness) {
		var map = (byHarness && byHarness.harnesses) ? harnessImageTags(byHarness) : (byHarness || {});
		var keys = HARNESSES.filter(function (h) { return Object.prototype.hasOwnProperty.call(map, h); });
		Object.keys(map).forEach(function (h) { if (keys.indexOf(h) < 0) keys.push(h); });

		var html = '<div class="agent-image-panel__title">Agent image</div>';
		keys.forEach(function (h) {
			var e = imageTagsForHarness(map, h);
			var newest = e.newest || newestDateTimeTag(imageTagNames(e.tags));
			html +=
				'<div class="agent-image-panel__harness" data-harness="' + escapeHTML(h) + '">' +
					'<span class="agent-image-panel__harness-name">' + escapeHTML(harnessLabel(h)) + '</span>' +
					'<span class="agent-image-panel__newest">Newest tag: ' + (newest ? escapeHTML(newest) : 'none discovered') + '</span>' +
					'<span class="agent-image-panel__checked">' + escapeHTML(formatCheckedAgo(e.checkedAt)) + '</span>' +
					(e.lastError ? '<p class="agent-image-panel__error">' + escapeHTML(e.lastError) + '</p>' : '') +
				'</div>';
		});
		html += '<div class="agent-image-panel__actions">' +
			'<button class="agent-image-panel__refresh btn btn--ghost btn--sm" type="button">Check now</button>' +
			'</div>';
		return html;
	}

	// formatBytes renders a byte count as a short human string. A negative or
	// non-finite input renders as an em dash.
	function formatBytes(n) {
		if (typeof n !== 'number' || !isFinite(n) || n < 0) return '—';
		if (n < 1024) return n + ' B';
		var units = ['KiB', 'MiB', 'GiB', 'TiB'];
		var v = n;
		for (var i = 0; i < units.length; i++) {
			v = v / 1024;
			if (v < 1024 || i === units.length - 1) return v.toFixed(1) + ' ' + units[i];
		}
		return v.toFixed(1) + ' TiB';
	}

	// formatModTime renders an ISO timestamp as "YYYY-MM-DD HH:MM". An
	// unparseable value renders as an em dash.
	function formatModTime(iso) {
		var d = new Date(iso);
		if (isNaN(d.getTime())) return '—';
		return d.toISOString().slice(0, 16).replace('T', ' ');
	}

	// renderBreadcrumb renders the file browser's path breadcrumb. The root
	// crumb (data-path="") is labelled "Files"; each accumulated segment is a
	// button, except the last, which is the current, non-button crumb.
	function renderBreadcrumb(path) {
		var parts = String(path || '').split('/').filter(function (p) { return p !== ''; });
		var html = '<nav class="file-browser__breadcrumb">';
		if (parts.length === 0) {
			html += '<span class="crumb crumb--current">Files</span>';
			return html + '</nav>';
		}
		html += '<button class="crumb" data-path="">Files</button>';
		var acc = '';
		for (var i = 0; i < parts.length; i++) {
			acc = acc ? acc + '/' + parts[i] : parts[i];
			if (i === parts.length - 1) {
				html += '<span class="crumb crumb--current">' + escapeHTML(parts[i]) + '</span>';
			} else {
				html += '<button class="crumb" data-path="' + escapeHTML(acc) + '">' + escapeHTML(parts[i]) + '</button>';
			}
		}
		return html + '</nav>';
	}

	// renderFileTable renders the entries of one directory (GET /api/files'
	// `entries`). Directories open on click; files get download + delete
	// actions. An empty list renders a one-line empty state.
	function renderFileTable(entries) {
		if (!entries || entries.length === 0) {
			return '<p class="file-table__empty">This folder is empty.</p>';
		}
		var rows = entries.map(function (e) {
			var p = escapeHTML(e.path);
			var name = escapeHTML(e.name);
			var nameCell = e.is_dir
				? '<button class="file-table__open" data-path="' + p + '">' + name + '</button>'
				: name;
			var sizeCell = e.is_dir ? '—' : escapeHTML(formatBytes(e.size));
			var actions = (e.is_dir ? '' : '<button class="file-table__download btn btn--ghost btn--sm" data-path="' + p + '">Download</button>') +
				'<button class="file-table__delete btn btn--danger btn--sm" data-path="' + p + '">Delete</button>';
			return (
				'<tr class="file-table__row" data-path="' + p + '" data-is-dir="' + (e.is_dir ? 'true' : 'false') + '">' +
					'<td class="file-table__name">' + nameCell + '</td>' +
					'<td class="file-table__size">' + sizeCell + '</td>' +
					'<td class="file-table__modified">' + escapeHTML(formatModTime(e.mod_time)) + '</td>' +
					'<td class="file-table__actions">' + actions + '</td>' +
				'</tr>'
			);
		}).join('');
		return (
			'<table class="file-table">' +
				'<thead><tr><th>Name</th><th>Size</th><th>Modified</th><th>Actions</th></tr></thead>' +
				'<tbody>' + rows + '</tbody>' +
			'</table>'
		);
	}

	// renderFileBrowser renders the whole browser: breadcrumb, a toolbar (new
	// folder + upload, with a hidden multi-file input), and a dropzone
	// wrapping the file table, plus a hidden error line.
	function renderFileBrowser(path, entries) {
		return (
			'<div class="file-browser">' +
				renderBreadcrumb(path) +
				'<div class="file-browser__toolbar">' +
					'<button class="file-browser__new-folder btn btn--ghost btn--sm" type="button">New folder</button>' +
					'<button class="file-browser__upload btn btn--ghost btn--sm" type="button">Upload</button>' +
					'<input type="file" class="file-browser__file-input" multiple hidden>' +
				'</div>' +
				'<div class="file-browser__dropzone">' +
					renderFileTable(entries) +
				'</div>' +
				'<p class="file-browser__error" role="alert" hidden></p>' +
			'</div>'
		);
	}

	var Render = {
		escapeHTML: escapeHTML,
		statusLabel: statusLabel,
		backendLabel: backendLabel,
		harnessSupportsBackend: harnessSupportsBackend,
		normalizeHarness: normalizeHarness,
		harnessLabel: harnessLabel,
		renderAgentListItem: renderAgentListItem,
		renderAgentList: renderAgentList,
		renderCapacity: renderCapacity,
		renderCreateForm: renderCreateForm,
		renderTemplateBar: renderTemplateBar,
		renderAgentInfo: renderAgentInfo,
		renderAnthropicStatus: renderAnthropicStatus,
		isDateTimeTag: isDateTimeTag,
		newestDateTimeTag: newestDateTimeTag,
		upgradeAvailable: upgradeAvailable,
		imageTagOf: imageTagOf,
		formatCheckedAgo: formatCheckedAgo,
		harnessImageTags: harnessImageTags,
		imageTagsForHarness: imageTagsForHarness,
		imageTagOffered: imageTagOffered,
		imageTagNames: imageTagNames,
		renderImageTagSelect: renderImageTagSelect,
		renderAgentImagePanel: renderAgentImagePanel,
		formatBytes: formatBytes,
		formatModTime: formatModTime,
		renderBreadcrumb: renderBreadcrumb,
		renderFileTable: renderFileTable,
		renderFileBrowser: renderFileBrowser,
	};

	if (typeof module !== 'undefined' && module.exports) {
		module.exports = Render;
	} else {
		global.Render = Render;
	}
})(typeof window !== 'undefined' ? window : globalThis);
