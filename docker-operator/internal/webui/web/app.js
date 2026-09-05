// app.js -- DOM wiring for the sidebar shell and the "New Agent" flow:
// fetches /api/agents, renders the list via render.js's pure functions,
// drives the create form (backend + model pickers), and the sidebar's
// Anthropic-account panel.
//
// The agent detail view (terminal, rename, delete) and the Anthropic login
// terminal are wired in by terminal.js, which app.js calls into via
// window.renderAgentDetail / window.renderAnthropicLogin when that script
// has loaded.
(function () {
	'use strict';

	var state = {
		agents: [],
		maxAgents: 0,
		selectedID: null,
		defaults: { backend: 'ollama', model: '', fastModel: '' },
	};

	var sidebarList = document.getElementById('agent-list');
	var capacityEl = document.getElementById('agent-capacity');
	var newAgentBtn = document.getElementById('new-agent-btn');
	var mainArea = document.getElementById('main-area');
	var anthropicPanel = document.getElementById('anthropic-panel');

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
		state.defaults = {
			backend: data.default_backend || 'ollama',
			model: data.default_model || '',
			fastModel: data.default_fast_model || '',
			repo: data.default_repo || '',
		};
		renderSidebar();
	}

	function selectAgent(id) {
		state.selectedID = id;
		renderSidebar();
		if (typeof window.renderAgentDetail === 'function') {
			window.renderAgentDetail(mainArea, id);
		}
	}

	// --- create form ---------------------------------------------------------

	function showCreateForm() {
		mainArea.innerHTML = window.Render.renderCreateForm(state.defaults);
		var form = mainArea.querySelector('.create-form');
		var ollamaBlock = form.querySelector('.create-form__ollama');
		var anthropicNote = form.querySelector('.create-form__anthropic-note');
		var errorEl = form.querySelector('.create-form__error');
		var submitBtn = form.querySelector('.create-form__submit');

		function currentBackend() {
			var checked = form.querySelector('input[name="backend"]:checked');
			return checked ? checked.value : 'ollama';
		}
		function syncBackend() {
			var anthropic = currentBackend() === 'anthropic';
			ollamaBlock.hidden = anthropic;
			anthropicNote.hidden = !anthropic;
		}
		form.querySelectorAll('input[name="backend"]').forEach(function (el) {
			el.addEventListener('change', syncBackend);
		});
		syncBackend();

		form.querySelector('.create-form__cancel').addEventListener('click', function () {
			showPlaceholder();
		});

		form.addEventListener('submit', function (ev) {
			ev.preventDefault();
			var backend = currentBackend();
			var body = {
				name: form.querySelector('.create-form__name').value.trim(),
				description: form.querySelector('.create-form__description').value.trim(),
				backend: backend,
			};
			var repo = form.querySelector('.create-form__repo').value.trim();
			if (repo) body.repo = repo;
			if (backend === 'ollama') {
				body.model = form.querySelector('.create-form__model').value.trim();
				body.fast_model = form.querySelector('.create-form__fast-model').value.trim();
			}
			errorEl.hidden = true;
			submitBtn.disabled = true;
			fetchJSON('/api/agents', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body),
			})
				.then(function (agent) {
					return refreshAgents().then(function () { selectAgent(agent.id); });
				})
				.catch(function (e) {
					submitBtn.disabled = false;
					errorEl.textContent = e.message;
					errorEl.hidden = false;
				});
		});
	}

	function showPlaceholder() {
		if (typeof window.teardownActiveView === 'function') window.teardownActiveView();
		state.selectedID = null;
		renderSidebar();
		mainArea.innerHTML = '<p class="placeholder">Select an agent, or create a new one.</p>';
	}

	// --- Anthropic account panel -------------------------------------------

	function refreshAnthropicPanel() {
		return fetchJSON('/api/anthropic/auth')
			.then(renderAnthropicPanel)
			.catch(function (e) {
				anthropicPanel.innerHTML =
					'<div class="anthropic-panel__title">Anthropic account</div>' +
					'<span class="anthropic-panel__status anthropic-panel__status--unset">' +
					window.Render.escapeHTML('unavailable: ' + e.message) + '</span>';
			});
	}

	function renderAnthropicPanel(status) {
		anthropicPanel.innerHTML =
			'<div class="anthropic-panel__title">Anthropic account</div>' +
			window.Render.renderAnthropicStatus(status) +
			'<div class="anthropic-panel__actions">' +
				'<button class="anthropic-panel__apikey" type="button">Set API key</button>' +
				'<button class="anthropic-panel__login" type="button">Log in</button>' +
				(status && status.configured ? '<button class="anthropic-panel__remove" type="button">Remove</button>' : '') +
			'</div>';

		anthropicPanel.querySelector('.anthropic-panel__apikey').addEventListener('click', function () {
			var key = window.prompt('Paste your Anthropic API key (starts with sk-ant-):');
			if (!key) return;
			putAnthropicAuth({ kind: 'api_key', value: key.trim() });
		});
		anthropicPanel.querySelector('.anthropic-panel__login').addEventListener('click', startAnthropicLogin);
		var removeBtn = anthropicPanel.querySelector('.anthropic-panel__remove');
		if (removeBtn) {
			removeBtn.addEventListener('click', function () {
				if (!window.confirm('Remove the stored Anthropic credential? Agents already created keep the copy they were given.')) return;
				fetchJSON('/api/anthropic/auth', { method: 'DELETE' }).then(refreshAnthropicPanel).catch(alertErr('Could not remove the credential'));
			});
		}
	}

	function putAnthropicAuth(payload) {
		return fetchJSON('/api/anthropic/auth', {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(payload),
		})
			.then(refreshAnthropicPanel)
			.catch(alertErr('Could not store the credential'));
	}

	function startAnthropicLogin() {
		fetchJSON('/api/anthropic/login', { method: 'POST' })
			.then(function () {
				if (typeof window.renderAnthropicLogin === 'function') {
					window.renderAnthropicLogin(mainArea, {
						submitToken: function (token) {
							return putAnthropicAuth({ kind: 'oauth', value: token });
						},
						onClose: function () {
							fetchJSON('/api/anthropic/login', { method: 'DELETE' }).catch(function () { /* best effort */ });
							showPlaceholder();
						},
					});
				}
			})
			.catch(alertErr('Could not start the login helper'));
	}

	function alertErr(prefix) {
		return function (e) { window.alert(prefix + ': ' + e.message); };
	}

	// --- wiring -------------------------------------------------------------

	sidebarList.addEventListener('click', function (ev) {
		var li = ev.target.closest('[data-agent-id]');
		if (li) selectAgent(li.getAttribute('data-agent-id'));
	});

	newAgentBtn.addEventListener('click', showCreateForm);

	// onAgentDeleted is called by terminal.js after a successful DELETE.
	window.onAgentDeleted = function (id) {
		if (state.selectedID === id) state.selectedID = null;
		refreshAgents().then(showPlaceholder).catch(function () {});
	};

	refreshAgents().catch(function (e) {
		sidebarList.innerHTML =
			'<li class="agent-list__error">Failed to load agents: ' + window.Render.escapeHTML(e.message) + '</li>';
	});
	refreshAnthropicPanel();

	// Poll for status changes (creating -> running, an unexpected stop, etc.)
	// every few seconds. Simplest correct approach for a V1 local tool with a
	// handful of agents at most -- no push channel needed yet.
	setInterval(function () {
		refreshAgents().catch(function () { /* transient failure; retried next tick */ });
	}, 3000);
})();
