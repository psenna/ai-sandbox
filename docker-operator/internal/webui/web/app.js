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
		defaults: { backend: 'ollama', model: '', fastModel: '', ollamaUrl: '', autoCompactThreshold: '', maxContextTokens: '', autoMode: 'on' },
		// agentImage is render.js's by-harness map (harnessImageTags): one
		// entry per harness, each with its own repo/default tag/offered tags/
		// last-checked/last-error. render.js is loaded before app.js (see
		// index.html), so building the empty map here is safe.
		agentImage: window.Render.harnessImageTags(null),
		// templates caches GET /api/templates's list for the create form's
		// template picker. Only relevant while that form is open, so it is
		// fetched lazily from showCreateForm rather than at load time.
		templates: [],
		// unread[id] is true for an agent that finished a turn (its
		// server-reported `activity` went working -> waiting) while it was
		// not the open agent -- cleared the moment selectAgent opens it, so
		// re-polling while it's open never re-flags it. agentActivity[id]
		// remembers each agent's last-seen activity across polls so
		// trackActivity below can detect that transition; a poll with no
		// signal (a transient read failure, or a harness that hasn't wired
		// activity reporting) keeps the previous value rather than losing
		// the streak.
		unread: {},
		agentActivity: {},
	};

	var sidebarList = document.getElementById('agent-list');
	var capacityEl = document.getElementById('agent-capacity');
	var newAgentBtn = document.getElementById('new-agent-btn');
	var filesBtn = document.getElementById('files-btn');
	var mainArea = document.getElementById('main-area');
	var anthropicPanel = document.getElementById('anthropic-panel');
	var agentImagePanel = document.getElementById('agent-image-panel');
	var sidebarEl = document.getElementById('sidebar');
	var sidebarToggle = document.getElementById('sidebar-toggle');

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
		sidebarList.innerHTML = window.Render.renderAgentList(state.agents, state.selectedID, state.unread);
		capacityEl.textContent = window.Render.renderCapacity(state.agents, state.maxAgents);
	}

	// trackActivity updates state.unread/state.agentActivity from one poll's
	// agents. See state.unread's own comment for why a blank reading doesn't
	// erase the last-known activity.
	function trackActivity(agents) {
		agents.forEach(function (a) {
			var prev = state.agentActivity[a.id];
			if (a.activity === 'waiting' && prev === 'working' && a.id !== state.selectedID) {
				state.unread[a.id] = true;
			}
			if (a.activity) state.agentActivity[a.id] = a.activity;
		});
	}

	async function refreshAgents() {
		var data = await fetchJSON('/api/agents');
		state.agents = data.agents || [];
		trackActivity(state.agents);
		state.maxAgents = data.max_agents || 0;
		state.defaults = {
			backend: data.default_backend || 'ollama',
			model: data.default_model || '',
			fastModel: data.default_fast_model || '',
			ollamaUrl: data.default_ollama_url || '',
			repo: data.default_repo || '',
			autoCompactThreshold: data.default_auto_compact_threshold || '',
			maxContextTokens: data.default_max_context_tokens || '',
			autoMode: data.default_auto_mode || 'on',
		};
		renderSidebar();
	}

	function selectAgent(id) {
		state.selectedID = id;
		delete state.unread[id];
		renderSidebar();
		if (typeof window.renderAgentDetail === 'function') {
			window.renderAgentDetail(mainArea, id);
		}
	}

	// --- create form ---------------------------------------------------------

	// currentForm returns the live .create-form element. A function, not a
	// cached variable: applyTemplateToForm below replaces the form's DOM node
	// wholesale (via outerHTML) whenever a template is applied, and every
	// template-bar handler that touches the form must see that replacement,
	// not a stale detached reference to the node it replaced.
	function currentForm() { return mainArea.querySelector('.create-form'); }

	function buildFormDefaults() {
		return Object.assign({}, state.defaults, { agentImage: state.agentImage });
	}

	function currentBackendOf(form) {
		var checked = form.querySelector('input[name="backend"]:checked');
		return checked ? checked.value : 'ollama';
	}

	function currentHarnessOf(form) {
		var checked = form.querySelector('input[name="harness"]:checked');
		return checked ? checked.value : 'claude-code';
	}

	// wireCreateForm attaches every listener the create/update form itself
	// needs (backend toggle, cancel, submit). Called fresh against whatever
	// .create-form element currently exists -- the initial one, or the
	// replacement applyTemplateToForm builds -- so it never binds to a node
	// that later gets swapped out from under it.
	function wireCreateForm(form) {
		var ollamaBlock = form.querySelector('.create-form__ollama');
		var anthropicNote = form.querySelector('.create-form__anthropic-note');
		var opencodeNote = form.querySelector('.create-form__opencode-note');
		var anthropicRadio = form.querySelector('input[name="backend"][value="anthropic"]');
		var anthropicLabel = form.querySelector('.create-form__backend-anthropic');
		var ollamaRadio = form.querySelector('input[name="backend"][value="ollama"]');
		var errorEl = form.querySelector('.create-form__error');
		var submitBtn = form.querySelector('.create-form__submit');

		// syncBackendAndHarness mirrors render.js's own harness/backend rule on
		// every `change`: an opencode harness can never leave the Anthropic
		// backend selected, so if it was checked, force it back to Ollama
		// first; then hide+disable the Anthropic option and toggle the
		// opencode note; THEN run the (possibly just-corrected) Ollama-fields
		// visibility check.
		function syncBackendAndHarness() {
			var opencode = !window.Render.harnessSupportsBackend(currentHarnessOf(form), 'anthropic');
			if (opencode && anthropicRadio && anthropicRadio.checked) {
				anthropicRadio.checked = false;
				if (ollamaRadio) ollamaRadio.checked = true;
			}
			if (anthropicRadio) anthropicRadio.disabled = opencode;
			if (anthropicLabel) anthropicLabel.hidden = opencode;
			if (opencodeNote) opencodeNote.hidden = !opencode;

			var anthropic = currentBackendOf(form) === 'anthropic';
			ollamaBlock.hidden = anthropic;
			anthropicNote.hidden = !anthropic;
		}
		form.querySelectorAll('input[name="backend"], input[name="harness"]').forEach(function (el) {
			el.addEventListener('change', syncBackendAndHarness);
		});
		syncBackendAndHarness();

		// syncImageTagSelect re-renders the image-tag <select> for whichever
		// harness is selected right now -- with NO network request: state
		// .agentImage already holds every harness's tag list from the 3s poll.
		// The tag the user picked survives the switch ONLY when the newly
		// selected harness actually offers it (the two repositories publish
		// independent tag histories); otherwise the new harness's own default
		// is selected. Mirrors syncBackendAndHarness above: a `change` handler
		// on the harness radios, re-running render.js's pure renderer rather
		// than mutating options by hand. The select carries no listeners of its
		// own, so replacing it outright is safe.
		function syncImageTagSelect() {
			var sel = form.querySelector('.create-form__image-tag');
			if (!sel) return;
			var info = window.Render.imageTagsForHarness(state.agentImage, currentHarnessOf(form));
			var keep = window.Render.imageTagOffered(info, sel.value) ? sel.value : '';
			sel.outerHTML = window.Render.renderImageTagSelect('create-form__image-tag', info.tags, info.defaultTag, keep);
		}
		form.querySelectorAll('input[name="harness"]').forEach(function (el) {
			el.addEventListener('change', syncImageTagSelect);
		});

		form.querySelector('.create-form__cancel').addEventListener('click', function () {
			showPlaceholder();
		});

		form.addEventListener('submit', function (ev) {
			ev.preventDefault();
			var backend = currentBackendOf(form);
			var body = {
				name: form.querySelector('.create-form__name').value.trim(),
				description: form.querySelector('.create-form__description').value.trim(),
				backend: backend,
				// harness is always sent explicitly, same as backend -- this
				// form's convention is "always send radio-group values",
				// reserving omit-when-default for tri-state fields.
				harness: currentHarnessOf(form),
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
			// Auto mode follows the same backend-agnostic, send-only-when-
			// not-"operator default" rule: the select's first option's value
			// is "", meaning "omit the field and let the operator default
			// apply".
			var autoMode = form.querySelector('.create-form__auto-mode').value;
			if (autoMode) body.auto_mode = autoMode;
			// The image-tag <select>'s selection only needs sending when it
			// differs from THIS HARNESS's own default tag (each harness has
			// its own default -- issue #199).
			var sel = form.querySelector('.create-form__image-tag');
			var harnessDefaultTag = window.Render.imageTagsForHarness(state.agentImage, body.harness).defaultTag;
			if (sel && sel.value && sel.value !== harnessDefaultTag) body.image_tag = sel.value;
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

	// infraFieldsFromForm reads the 9 template-eligible fields off the
	// current form, trimmed, following the exact same "send only when it
	// differs from the operator default" rule wireCreateForm's own submit
	// body already uses for image_tag -- so saving a template pins a tag
	// only when the user genuinely picked a non-default one.
	// harness is deliberately excluded from these fields: store.Template has no
	// Harness field yet, so a saved template can't carry it.
	function infraFieldsFromForm(form) {
		var backend = currentBackendOf(form);
		var fields = {
			backend: backend,
			repo: form.querySelector('.create-form__repo').value.trim(),
			auto_compact_threshold: form.querySelector('.create-form__auto-compact').value.trim(),
			max_context_tokens: form.querySelector('.create-form__max-context-tokens').value.trim(),
			auto_mode: form.querySelector('.create-form__auto-mode').value,
			image_tag: '',
			model: '',
			fast_model: '',
			ollama_url: '',
		};
		var sel = form.querySelector('.create-form__image-tag');
		var harnessDefaultTag = window.Render.imageTagsForHarness(state.agentImage, currentHarnessOf(form)).defaultTag;
		if (sel && sel.value && sel.value !== harnessDefaultTag) fields.image_tag = sel.value;
		if (backend === 'ollama') {
			fields.model = form.querySelector('.create-form__model').value.trim();
			fields.fast_model = form.querySelector('.create-form__fast-model').value.trim();
			fields.ollama_url = form.querySelector('.create-form__ollama-url').value.trim();
		}
		return fields;
	}

	// applyTemplateToForm rebuilds ONLY the .create-form element (via
	// renderCreateForm's opts.values, the same mechanism the update-agent
	// form already uses for an agent record) with the selected template's 9
	// infra fields, while explicitly carrying the agent's own current Name/
	// Description straight through untouched. This exclusion is load-bearing,
	// not cosmetic: renderCreateForm reads values.name/values.description
	// directly, so passing the Template record itself (which has its OWN
	// name/description) would silently overwrite whatever the user already
	// typed for the agent.
	function applyTemplateToForm(t) {
		var form = currentForm();
		var values = {
			name: form.querySelector('.create-form__name').value,
			description: form.querySelector('.create-form__description').value,
			// store.Template has no Harness field, so the re-render carries the
			// CURRENT form's harness selection through rather than letting it
			// reset to the claude-code default, same as name/description above.
			harness: currentHarnessOf(form),
			backend: t.backend,
			model: t.model,
			fast_model: t.fast_model,
			ollama_url: t.ollama_url,
			repo: t.repo,
			auto_compact_threshold: t.auto_compact_threshold,
			max_context_tokens: t.max_context_tokens,
			image_tag: t.image_tag,
			auto_mode: t.auto_mode,
		};
		form.outerHTML = window.Render.renderCreateForm(buildFormDefaults(), { values: values });
		wireCreateForm(currentForm());
	}

	// refreshTemplateBar re-renders ONLY the .template-bar element from the
	// current state.templates and re-wires it -- the .create-form element
	// (and whatever the user is mid-typing into it) is never touched here,
	// unlike applyTemplateToForm above. selectedId (optional) is the
	// template to leave selected in the dropdown afterwards.
	function refreshTemplateBar(selectedId) {
		var bar = mainArea.querySelector('.template-bar');
		if (!bar) return; // the create form isn't open (or was navigated away from)
		bar.outerHTML = window.Render.renderTemplateBar(state.templates);
		wireTemplateBar(selectedId);
	}

	function wireTemplateBar(selectedId) {
		var templateBar = mainArea.querySelector('.template-bar');
		var templateSelect = templateBar.querySelector('.template-bar__select');
		var saveBtn = templateBar.querySelector('.template-bar__save');
		var deleteBtn = templateBar.querySelector('.template-bar__delete');
		var errorEl = templateBar.querySelector('.template-bar__error');
		if (selectedId) templateSelect.value = selectedId;

		function currentTemplate() {
			var matches = state.templates.filter(function (t) { return t.id === templateSelect.value; });
			return matches.length ? matches[0] : null;
		}
		function syncButtons() {
			var t = currentTemplate();
			saveBtn.textContent = t ? 'Update template' : 'Save as template';
			deleteBtn.hidden = !t;
		}
		syncButtons();

		templateSelect.addEventListener('change', function () {
			errorEl.hidden = true;
			syncButtons();
			// Blank selection ("— Select a template —") is a deliberate no-op
			// on the form fields -- it must never silently discard manual edits.
			var t = currentTemplate();
			if (t) applyTemplateToForm(t);
		});

		saveBtn.addEventListener('click', function () {
			var existing = currentTemplate();
			var name = window.prompt('Template name:', existing ? existing.name : '');
			if (name === null) return; // cancelled
			name = name.trim();
			if (!name) return;

			var body = Object.assign(
				{ name: name, description: existing ? existing.description : '' },
				infraFieldsFromForm(currentForm())
			);
			errorEl.hidden = true;
			saveBtn.disabled = true;

			var save = existing
				? fetchJSON('/api/templates/' + existing.id, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
				: fetchJSON('/api/templates', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

			save.then(function (saved) {
				return fetchJSON('/api/templates').then(function (data) {
					state.templates = (data && data.templates) || [];
					refreshTemplateBar(saved.id);
				});
			}).catch(function (e) {
				saveBtn.disabled = false;
				errorEl.textContent = e.message;
				errorEl.hidden = false;
			});
		});

		deleteBtn.addEventListener('click', function () {
			var t = currentTemplate();
			if (!t) return;
			window.OperatorConfirm.show(
				'This cannot be undone.',
				{ title: 'Delete the template "' + t.name + '"?', confirmLabel: 'Delete', danger: true }
			).then(function (confirmed) {
				if (!confirmed) return;
				fetchJSON('/api/templates/' + t.id, { method: 'DELETE' })
					.then(function () {
						state.templates = state.templates.filter(function (x) { return x.id !== t.id; });
						refreshTemplateBar('');
					})
					.catch(function (e) {
						errorEl.textContent = e.message;
						errorEl.hidden = false;
					});
			});
		});
	}

	function showCreateForm() {
		mainArea.innerHTML = window.Render.renderTemplateBar(state.templates) + window.Render.renderCreateForm(buildFormDefaults());
		wireCreateForm(currentForm());
		wireTemplateBar();

		// The template list is fetched lazily here (not the load-time IIFE,
		// not the 3s poll below) since it only matters while this form is
		// open. state.templates keeps whatever it held from a previous open
		// until this resolves, so reopening the form isn't blocked on it; a
		// failure degrades to "no templates" rather than blocking the form.
		fetchJSON('/api/templates')
			.then(function (data) { state.templates = (data && data.templates) || []; })
			.catch(function () { state.templates = []; })
			.then(function () {
				if (!currentForm()) return; // navigated away while the fetch was in flight
				refreshTemplateBar();
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
				'<button class="anthropic-panel__apikey btn btn--ghost btn--sm" type="button">Set API key</button>' +
				'<button class="anthropic-panel__login btn btn--ghost btn--sm" type="button">Log in</button>' +
				(status && status.configured ? '<button class="anthropic-panel__remove btn btn--danger btn--sm" type="button">Remove</button>' : '') +
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
				window.OperatorConfirm.show(
					'Agents already created keep the copy they were given.',
					{ title: 'Remove the stored Anthropic credential?', confirmLabel: 'Remove', danger: true }
				).then(function (confirmed) {
					if (!confirmed) return;
					fetchJSON('/api/anthropic/auth', { method: 'DELETE' }).then(refreshAnthropicPanel).catch(alertErr('Could not remove the credential'));
				});
			});
		}
	}

	// --- agent image panel -------------------------------------------------

	function mapAgentImage(data) {
		state.agentImage = window.Render.harnessImageTags(data);
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

	// --- sidebar collapse -----------------------------------------------------
	// Collapses the sidebar to a slim rail (agent list, panels and the two
	// top buttons hidden -- see .sidebar--collapsed in style.css) so the
	// terminal/file browser can take the full window width when the agent
	// list isn't needed. Defaults to expanded (visible) on first load;
	// persisted the same defensively-wrapped way auth.js persists the
	// operator token, so a blocked/unavailable localStorage just means the
	// preference doesn't survive a reload, never a broken sidebar.
	var SIDEBAR_COLLAPSED_KEY = 'docker-operator:sidebar-collapsed';

	function readSidebarCollapsed() {
		try { return window.localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === '1'; } catch (e) { return false; }
	}
	function writeSidebarCollapsed(collapsed) {
		try { window.localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? '1' : '0'); } catch (e) { /* best effort */ }
	}

	function setSidebarCollapsed(collapsed) {
		sidebarEl.classList.toggle('sidebar--collapsed', collapsed);
		sidebarToggle.textContent = collapsed ? '›' : '‹';
		sidebarToggle.title = collapsed ? 'Expand sidebar' : 'Collapse sidebar';
		sidebarToggle.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
		writeSidebarCollapsed(collapsed);
	}

	if (sidebarEl && sidebarToggle) {
		setSidebarCollapsed(readSidebarCollapsed());
		sidebarToggle.addEventListener('click', function () {
			setSidebarCollapsed(!sidebarEl.classList.contains('sidebar--collapsed'));
		});
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
