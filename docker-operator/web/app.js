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
		defaults: { backend: 'ollama', model: '', fastModel: '', ollamaUrl: '', autoCompactThreshold: '', maxContextTokens: '' },
		agentImage: { tags: [], newest: '', operatorDefault: '', checkedAt: null, lastError: '' },
	};

	var sidebarList = document.getElementById('agent-list');
	var capacityEl = document.getElementById('agent-capacity');
	var newAgentBtn = document.getElementById('new-agent-btn');
	var filesBtn = document.getElementById('files-btn');
	var mainArea = document.getElementById('main-area');
	var anthropicPanel = document.getElementById('anthropic-panel');
	var agentImagePanel = document.getElementById('agent-image-panel');

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
		// window.OperatorAuth (auth.js) attaches the Bearer token and handles a
		// 401 by prompting for a new one; fall back to a bare fetch only if
		// that script somehow did not load.
		var doFetch = (window.OperatorAuth && window.OperatorAuth.fetch) || fetch;
		var resp = await doFetch(url, options);
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
			ollamaUrl: data.default_ollama_url || '',
			repo: data.default_repo || '',
			autoCompactThreshold: data.default_auto_compact_threshold || '',
			maxContextTokens: data.default_max_context_tokens || '',
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
		var formDefaults = Object.assign({}, state.defaults, {
			imageTags: state.agentImage.tags,
			imageDefaultTag: state.agentImage.operatorDefault,
		});
		mainArea.innerHTML = window.Render.renderCreateForm(formDefaults);
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
			// Auto-compact threshold is backend-agnostic: only sent when the
			// user entered a value, so an empty field means "omit the variable
			// and let the agent use Claude Code's built-in default".
			var autoCompact = form.querySelector('.create-form__auto-compact').value.trim();
			if (autoCompact) body.auto_compact_threshold = autoCompact;
			// Max-context tokens follows the same backend-agnostic, send-only-
			// when-entered rule.
			var maxContextTokens = form.querySelector('.create-form__max-context-tokens').value.trim();
			if (maxContextTokens) body.max_context_tokens = maxContextTokens;
			// The image-tag <select>'s first option is the operator default;
			// only send image_tag when the user picked something else.
			var sel = form.querySelector('.create-form__image-tag');
			if (sel && sel.value && sel.value !== state.agentImage.operatorDefault) body.image_tag = sel.value;
			if (backend === 'ollama') {
				body.model = form.querySelector('.create-form__model').value.trim();
				body.fast_model = form.querySelector('.create-form__fast-model').value.trim();
				var ollamaURL = form.querySelector('.create-form__ollama-url').value.trim();
				if (ollamaURL) body.ollama_url = ollamaURL;
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

	// --- agent image panel -------------------------------------------------

	function mapAgentImage(data) {
		data = data || {};
		state.agentImage = {
			tags: data.tags || [],
			newest: data.newest || '',
			operatorDefault: data.operator_default || '',
			checkedAt: data.checked_at || null,
			lastError: data.last_error || '',
		};
	}

	function renderAgentImagePanelNow() {
		agentImagePanel.innerHTML = window.Render.renderAgentImagePanel(state.agentImage);
		var btn = agentImagePanel.querySelector('.agent-image-panel__refresh');
		if (btn) {
			btn.addEventListener('click', function () {
				btn.disabled = true;
				fetchJSON('/api/agent-image/refresh', { method: 'POST' })
					.then(function (data) {
						mapAgentImage(data);
						renderAgentImagePanelNow();
					})
					.catch(function (e) {
						agentImagePanel.innerHTML =
							'<div class="agent-image-panel__title">Agent image</div>' +
							'<span class="agent-image-panel__status--unavailable">' +
							window.Render.escapeHTML('unavailable: ' + e.message) + '</span>';
					});
			});
		}
	}

	function refreshAgentImagePanel() {
		return fetchJSON('/api/agent-image/tags')
			.then(function (data) {
				mapAgentImage(data);
				renderAgentImagePanelNow();
			})
			.catch(function (e) {
				agentImagePanel.innerHTML =
					'<div class="agent-image-panel__title">Agent image</div>' +
					'<span class="agent-image-panel__status--unavailable">' +
					window.Render.escapeHTML('unavailable: ' + e.message) + '</span>';
			});
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

	if (filesBtn) {
		filesBtn.addEventListener('click', function () {
			if (typeof window.teardownActiveView === 'function') window.teardownActiveView();
			state.selectedID = null;
			renderSidebar();
			if (typeof window.renderFileBrowser === 'function') {
				window.renderFileBrowser(mainArea);
			}
		});
	}

	// onAgentDeleted is called by terminal.js after a successful DELETE.
	window.onAgentDeleted = function (id) {
		if (state.selectedID === id) state.selectedID = null;
		refreshAgents().then(showPlaceholder).catch(function () {});
	};

	// onAgentUpdated is called by terminal.js after a successful in-place
	// update: refresh the list (status went updating -> running) and re-attach
	// to the agent, which gives the viewer a fresh terminal on the new
	// container.
	window.onAgentUpdated = function (id) {
		refreshAgents().then(function () { selectAgent(id); }).catch(function () { selectAgent(id); });
	};

	// So terminal.js's openUpdateForm can reach the operator's create-form
	// defaults (backend/model/... ) without a second /api/agents round-trip.
	window.getAgentDefaults = function () { return state.defaults; };

	refreshAgents().catch(function (e) {
		sidebarList.innerHTML =
			'<li class="agent-list__error">Failed to load agents: ' + window.Render.escapeHTML(e.message) + '</li>';
	});
	refreshAnthropicPanel();
	refreshAgentImagePanel();

	// Poll for status changes (creating -> running, an unexpected stop, etc.)
	// every few seconds. Simplest correct approach for a V1 local tool with a
	// handful of agents at most -- no push channel needed yet.
	setInterval(function () {
		refreshAgents().catch(function () { /* transient failure; retried next tick */ });
		refreshAgentImagePanel().catch(function () { /* transient failure; retried next tick */ });
	}, 3000);
})();
