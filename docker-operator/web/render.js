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

	var Render = {
		escapeHTML: escapeHTML,
		statusLabel: statusLabel,
		renderAgentListItem: renderAgentListItem,
		renderAgentList: renderAgentList,
		renderCapacity: renderCapacity,
	};

	if (typeof module !== 'undefined' && module.exports) {
		module.exports = Render;
	} else {
		global.Render = Render;
	}
})(typeof window !== 'undefined' ? window : globalThis);
