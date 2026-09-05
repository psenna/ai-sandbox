// app.js -- DOM wiring for the sidebar shell: fetches /api/agents, renders
// the list via render.js's pure functions, and wires up the "New Agent"
// button and agent selection.
//
// The agent detail view itself (terminal, rename, delete) is wired in by
// terminal.js (added in a later issue), which app.js calls into via
// window.renderAgentDetail if that script has been loaded -- keeping this
// file's own scope to exactly what issue #75 covers: the sidebar shell and
// an empty main-area placeholder.
(function () {
	'use strict';

	var state = {
		agents: [],
		maxAgents: 0,
		selectedID: null,
	};

	var sidebarList = document.getElementById('agent-list');
	var capacityEl = document.getElementById('agent-capacity');
	var newAgentBtn = document.getElementById('new-agent-btn');
	var mainArea = document.getElementById('main-area');

	// apiError extracts internal/api's {"error":{"message":...}} envelope
	// when present, falling back to a generic message for a response that
	// isn't JSON at all (a proxy error page, a dropped connection, etc.).
	function apiError(status, body) {
		try {
			var env = JSON.parse(body);
			if (env && env.error && env.error.message) {
				return new Error(env.error.message);
			}
		} catch (e) {
			// body wasn't JSON; fall through to the generic message below.
		}
		return new Error('request failed (' + status + ')');
	}

	async function fetchJSON(url, options) {
		var resp = await fetch(url, options);
		var text = await resp.text();
		if (!resp.ok) throw apiError(resp.status, text);
		return text ? JSON.parse(text) : null;
	}

	function renderSidebar() {
		sidebarList.innerHTML = window.Render.renderAgentList(state.agents, state.selectedID);
		capacityEl.textContent = window.Render.renderCapacity(state.agents, state.maxAgents);
	}

	async function refreshAgents() {
		var data = await fetchJSON('/api/agents');
		state.agents = data.agents || [];
		state.maxAgents = data.max_agents || 0;
		renderSidebar();
	}

	function selectAgent(id) {
		state.selectedID = id;
		renderSidebar();
		if (typeof window.renderAgentDetail === 'function') {
			window.renderAgentDetail(mainArea, id);
		}
	}

	async function createAgent() {
		newAgentBtn.disabled = true;
		try {
			var agent = await fetchJSON('/api/agents', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({}),
			});
			await refreshAgents();
			selectAgent(agent.id);
		} catch (e) {
			window.alert('Could not create agent: ' + e.message);
		} finally {
			newAgentBtn.disabled = false;
		}
	}

	sidebarList.addEventListener('click', function (ev) {
		var li = ev.target.closest('[data-agent-id]');
		if (li) selectAgent(li.getAttribute('data-agent-id'));
	});

	newAgentBtn.addEventListener('click', createAgent);

	refreshAgents().catch(function (e) {
		sidebarList.innerHTML =
			'<li class="agent-list__error">Failed to load agents: ' + window.Render.escapeHTML(e.message) + '</li>';
	});

	// Poll for status changes (creating -> running, an unexpected stop,
	// etc.) every few seconds. Simplest correct approach for a V1 local tool
	// with a handful of agents at most -- no push channel needed yet.
	setInterval(function () {
		refreshAgents().catch(function () { /* transient failure; retried next tick */ });
	}, 3000);
})();
