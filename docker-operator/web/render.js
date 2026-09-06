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

	// renderAgentListItem renders one sidebar <li> for a single agent.
	// selectedID may be null/undefined; it is compared with === so no agent
	// matches unless one is actually selected.
	function renderAgentListItem(agent, selectedID) {
		var label = statusLabel(agent.status);
		var selected = agent.id === selectedID ? ' agent-item--selected' : '';
		var name = agent.name ? escapeHTML(agent.name) : '(unnamed)';
		return (
			'<li class="agent-item' + selected + '" data-agent-id="' + escapeHTML(agent.id) + '">' +
				'<span class="status-dot ' + label.cls + '" title="' + label.text + '"></span>' +
				'<span class="agent-item__name">' + name + '</span>' +
				'<span class="agent-item__backend" title="backend">' + escapeHTML(backendLabel(agent.backend)) + '</span>' +
			'</li>'
		);
	}

	// renderAgentList renders the sidebar's full <li> list, in the order the
	// API returned it, or a one-line empty state when there are no agents.
	function renderAgentList(agents, selectedID) {
		if (!agents || agents.length === 0) {
			return '<li class="agent-list__empty">No agents yet — click “New Agent” to create one.</li>';
		}
		return agents
			.map(function (a) { return renderAgentListItem(a, selectedID); })
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

	// renderCreateForm renders the "New Agent" form. defaults pre-fills the
	// backend choice, the two Ollama model fields and the repo from the
	// operator's configuration (GET /api/agents' default_* fields). The
	// caller wires the backend radio to show/hide .create-form__ollama and
	// submits the form's values to POST /api/agents.
	function renderCreateForm(defaults) {
		defaults = defaults || {};
		var backend = defaults.backend === 'anthropic' ? 'anthropic' : 'ollama';
		var model = escapeHTML(defaults.model || '');
		var fastModel = escapeHTML(defaults.fastModel || '');
		var repo = escapeHTML(defaults.repo || '');
		var ollamaHidden = backend === 'ollama' ? '' : ' hidden';
		return (
			'<form class="create-form">' +
				'<h2 class="create-form__title">New agent</h2>' +
				'<label class="create-form__row">Name<input class="create-form__name" type="text" placeholder="(optional)"></label>' +
				'<label class="create-form__row">Description<input class="create-form__description" type="text" placeholder="(optional)"></label>' +
				'<label class="create-form__row">Repository' +
					'<input class="create-form__repo" type="text" value="' + repo + '" placeholder="owner/repo.git — blank for a bare terminal">' +
				'</label>' +
				'<fieldset class="create-form__row create-form__backend">' +
					'<legend>Backend</legend>' +
					'<label><input type="radio" name="backend" value="ollama"' + (backend === 'ollama' ? ' checked' : '') + '> Ollama</label>' +
					'<label><input type="radio" name="backend" value="anthropic"' + (backend === 'anthropic' ? ' checked' : '') + '> Anthropic account</label>' +
				'</fieldset>' +
				'<div class="create-form__ollama"' + ollamaHidden + '>' +
					'<label class="create-form__row">Opus-tier model<input class="create-form__model" type="text" value="' + model + '"></label>' +
					'<label class="create-form__row">Sonnet &amp; Haiku-tier model<input class="create-form__fast-model" type="text" value="' + fastModel + '"></label>' +
				'</div>' +
				'<p class="create-form__anthropic-note" hidden>Uses the shared Anthropic login (set it in the sidebar first).</p>' +
				'<div class="create-form__actions">' +
					'<button class="create-form__submit" type="submit">Create</button>' +
					'<button class="create-form__cancel" type="button">Cancel</button>' +
				'</div>' +
				'<p class="create-form__error" role="alert" hidden></p>' +
			'</form>'
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
			var actions = (e.is_dir ? '' : '<button class="file-table__download" data-path="' + p + '">Download</button>') +
				'<button class="file-table__delete" data-path="' + p + '">Delete</button>';
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
					'<button class="file-browser__new-folder" type="button">New folder</button>' +
					'<button class="file-browser__upload" type="button">Upload</button>' +
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
		renderAgentListItem: renderAgentListItem,
		renderAgentList: renderAgentList,
		renderCapacity: renderCapacity,
		renderCreateForm: renderCreateForm,
		renderAnthropicStatus: renderAnthropicStatus,
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
