// app.js -- DOM wiring for the sidebar shell and the "New Agent" flow:
// fetches /api/agents, renders the list via render.js's pure functions,
// drives the create form (backend + model pickers), and the Settings modal
// overlay (the Anthropic-account and Agent-image sections).
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
		anthropicAccounts: [],
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
	var activityBtn = document.getElementById('activity-btn');
	var settingsBtn = document.getElementById('settings-btn');
	var mainArea = document.getElementById('main-area');
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
			defaultAnthropicAccountId: data.default_anthropic_account_id || '',
		};
		renderSidebar();
		renderDashboardNow();
	}

	// renderDashboardNow repaints the Activity page when -- and only when -- it
	// is the view currently mounted in the main area. It exists so every list
	// refresh (initial load, poll, create, delete, update) keeps the page's
	// "last active" times fresh without the guard leaking into refreshAgents:
	// the state feed there stays unconditional, only this repaint is guarded.
	function renderDashboardNow() {
		if (!mainArea.querySelector('.activity-page')) return;
		var body = mainArea.querySelector('.activity-page__body');
		var scrollTop = body ? body.scrollTop : 0;
		// The replacement is wholesale (innerHTML), so carry the scroll
		// position across it -- otherwise a long fleet list jumps to the top
		// on every poll.
		mainArea.innerHTML = window.Render.renderDashboard(state.agents);
		var next = mainArea.querySelector('.activity-page__body');
		if (next) next.scrollTop = scrollTop;
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
		return Object.assign({}, state.defaults, { agentImage: state.agentImage, anthropicAccounts: state.anthropicAccounts });
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
			if (backend === 'anthropic') {
				var accountSel = form.querySelector('.create-form__anthropic-account');
				var defaultAccountId = state.defaults.defaultAnthropicAccountId || '';
				if (accountSel && accountSel.value && accountSel.value !== defaultAccountId) {
					body.account_id = accountSel.value;
				}
			}
			errorEl.hidden = true;
			submitBtn.disabled = true;
			fetchJSON('/api/agents', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body),
			})
				.then(function () {
					// agentChangePolicy('create', ...) is 'sidebar-only': the
					// form stays on screen so its values survive for the next
					// agent, and only the list updates. The button was disabled
					// above on the assumption the view was about to be replaced
					// -- re-enable it, or a second agent can never be created.
					// A failed list refresh is not surfaced here: the 3s poll
					// picks the new agent up on its next tick.
					submitBtn.disabled = false;
					refreshAgents().catch(function () { /* the 3s poll heals it */ });
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
		// The re-render below replaces the WHOLE form, so the Advanced
		// disclosure's open/closed state -- which lives on the <details>
		// element and nowhere else -- has to be captured and handed back, or
		// applying a template silently collapses a section the user had open.
		// Every field VALUE travels through `values`; this is UI state only.
		var advanced = form.querySelector('.create-form__advanced');
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
		form.outerHTML = window.Render.renderCreateForm(buildFormDefaults(), {
			values: values,
			advancedOpen: !!(advanced && advanced.open),
		});
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
		// Release whatever the slot held first, like showPlaceholder and the
		// Files button already do. This was previously implicit: the create
		// SUCCESS path navigated to the new agent, and that re-render tore the
		// old view down. Now that create leaves the form on screen
		// (agentChangePolicy), nothing else would -- so an agent left open
		// behind the form would keep its WebSocket and xterm alive on a
		// detached node for as long as the form stays up.
		if (typeof window.teardownActiveView === 'function') window.teardownActiveView();
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

	// --- Settings overlay -----------------------------------------------------
	//
	// Settings is a MODAL on document.body, not a main-area page: opening it
	// claims nothing, so whatever view is behind the scrim survives, and
	// window.teardownActiveView (terminal.js's own teardown) never touches it.
	// That is confirm.js's ownership model -- a private module-level handle,
	// close-before-open, and an Escape listener scoped to its own lifetime.

	// settingsOverlay is { root, onKey } while open, null otherwise.
	var settingsOverlay = null;

	function closeSettings() {
		if (!settingsOverlay) return;
		document.removeEventListener('keydown', settingsOverlay.onKey);
		if (settingsOverlay.root.parentNode) settingsOverlay.root.parentNode.removeChild(settingsOverlay.root);
		settingsOverlay = null;
	}

	function openSettings() {
		closeSettings(); // close-before-open, like confirm.js's show()

		var root = document.createElement('div');
		root.className = 'settings-overlay';
		root.innerHTML = window.Render.renderSettingsOverlay();
		document.body.appendChild(root);

		// The nav swaps which section is shown; every section stays in the DOM
		// (render.js renders them all, inactive ones hidden) so the poll-driven
		// refreshes keep filling them whichever page is on screen.
		var navItems = root.querySelectorAll('.settings-overlay__nav-item');
		function activate(id) {
			navItems.forEach(function (btn) {
				var on = btn.getAttribute('data-settings-nav') === id;
				btn.classList.toggle('settings-overlay__nav-item--active', on);
				btn.setAttribute('aria-current', on ? 'true' : 'false');
			});
			root.querySelectorAll('[data-settings-section]').forEach(function (sec) {
				sec.hidden = sec.getAttribute('data-settings-section') !== id;
			});
		}
		navItems.forEach(function (btn) {
			btn.addEventListener('click', function () { activate(btn.getAttribute('data-settings-nav')); });
		});

		function onKey(ev) {
			if (ev.key !== 'Escape') return;
			// A confirm dialog stacked on top of this overlay -- the credential
			// Remove button opens one -- owns Escape first. confirm.js is a
			// sibling on document.body and has no idea this overlay is
			// underneath, so without this guard one press dismisses both.
			if (document.querySelector('.confirm-overlay')) return;
			closeSettings();
		}
		document.addEventListener('keydown', onKey);
		root.querySelector('.settings-overlay__close').addEventListener('click', closeSettings);
		root.addEventListener('mousedown', function (ev) {
			if (ev.target === root) closeSettings();
		});
		settingsOverlay = { root: root, onKey: onKey };

		// Both sections refresh on open. Each also repaints from its own
		// state (state.anthropicAccounts / state.agentImage), which the 3s
		// poll keeps fresh whether or not this overlay was ever opened.
		refreshAnthropicAccounts();
		refreshAgentImagePanel();
		root.querySelector('.settings-overlay__nav-item').focus();
	}

	// --- Settings sections: Anthropic account and Agent image -----------------
	//
	// Both live inside the overlay's sections, built from render.js's
	// renderSettingsOverlay() (one [data-settings-body] div per section) and
	// filled from here. Nothing is rendered while the overlay is closed, so
	// every continuation re-queries its body and bails if it is gone -- the
	// same "navigated away while the fetch was in flight" guard as currentForm()
	// below and refreshTemplateBar.

	function settingsBody(id) {
		if (!settingsOverlay) return null;
		return settingsOverlay.root.querySelector('[data-settings-body="' + id + '"]');
	}

	// --- Settings section: Anthropic Accounts -------------------------------

	var anthropicAccountBusy = false;
	var anthropicAccountError = null;

	function renderAnthropicAccountsPanelNow() {
		var body = settingsBody('anthropic-account');
		if (!body) return;
		body.innerHTML = window.Render.renderAnthropicAccountsPanel(state.anthropicAccounts, {
			busy: anthropicAccountBusy,
			error: anthropicAccountError,
		});
		wireAnthropicAccountsPanel();
	}

	// refreshAnthropicAccounts ALWAYS updates state.anthropicAccounts (so the
	// create/update forms' account <select> stays fresh via the 3s poll
	// whether or not Settings is open), and only then repaints the section
	// body if it is on screen -- the same division of labor
	// refreshAgentImagePanel already uses for state.agentImage.
	function refreshAnthropicAccounts() {
		return fetchJSON('/api/anthropic/accounts')
			.then(function (data) {
				state.anthropicAccounts = (data && data.accounts) || [];
				anthropicAccountError = null;
				renderAnthropicAccountsPanelNow();
			})
			.catch(function (e) {
				anthropicAccountError = 'unavailable: ' + e.message;
				var body = settingsBody('anthropic-account');
				if (!body) return;
				renderAnthropicAccountsPanelNow();
			});
	}

	function wireAnthropicAccountsPanel() {
		var body = settingsBody('anthropic-account');
		if (!body) return;

		var apikeyBtn = body.querySelector('.anthropic-accounts__add-apikey');
		if (apikeyBtn) apikeyBtn.addEventListener('click', function () {
			var name = window.prompt('Name this account:');
			if (!name || !name.trim()) return;
			var key = window.prompt('Paste the Anthropic API key (starts with sk-ant-):');
			if (!key) return;
			anthropicAccountBusy = true;
			renderAnthropicAccountsPanelNow();
			createAnthropicAccount({ name: name.trim(), kind: 'api_key', value: key.trim() })
				.then(function () {
					anthropicAccountBusy = false;
					renderAnthropicAccountsPanelNow();
				}, function () {
					anthropicAccountBusy = false;
					renderAnthropicAccountsPanelNow();
				});
		});

		var loginBtn = body.querySelector('.anthropic-accounts__add-login');
		if (loginBtn) loginBtn.addEventListener('click', function () {
			var name = window.prompt('Name this account:');
			if (!name || !name.trim()) return;
			anthropicAccountBusy = true;
			renderAnthropicAccountsPanelNow();
			startAnthropicLogin(name.trim());
		});

		body.querySelectorAll('.anthropic-accounts__set-default').forEach(function (btn) {
			btn.addEventListener('click', function () {
				var id = btn.getAttribute('data-id');
				fetchJSON('/api/anthropic/accounts/' + encodeURIComponent(id) + '/default', { method: 'PUT' })
					.then(refreshAnthropicAccounts)
					.catch(alertErr('Could not set the default account'));
			});
		});

		body.querySelectorAll('.anthropic-accounts__remove').forEach(function (btn) {
			btn.addEventListener('click', function () {
				var id = btn.getAttribute('data-id');
				var name = btn.getAttribute('data-name');
				window.OperatorConfirm.show(
					'Agents already pinned to "' + name + '" will fail to start again (until repointed to a different account) the next time they restart or wake.',
					{ title: 'Remove the account "' + name + '"?', confirmLabel: 'Remove', danger: true }
				).then(function (confirmed) {
					if (!confirmed) return;
					fetchJSON('/api/anthropic/accounts/' + encodeURIComponent(id), { method: 'DELETE' })
						.then(refreshAnthropicAccounts)
						.catch(alertErr('Could not remove the account'));
				});
			});
		});
	}

	function createAnthropicAccount(payload) {
		return fetchJSON('/api/anthropic/accounts', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(payload),
		})
			.then(refreshAnthropicAccounts)
			.catch(alertErr('Could not create the account'));
	}

	function startAnthropicLogin(name) {
		return fetchJSON('/api/anthropic/login', { method: 'POST' })
			.then(function () {
				if (typeof window.renderAnthropicLogin === 'function') {
					// The login terminal claims the main area, so the Settings
					// modal has to come down first -- otherwise its scrim sits
					// over the very terminal the user is meant to type into.
					closeSettings();
					anthropicAccountBusy = false;
					renderAnthropicAccountsPanelNow();
					window.renderAnthropicLogin(mainArea, {
						submitToken: function (token) {
							return createAnthropicAccount({ name: name, kind: 'oauth', value: token });
						},
						onClose: function () {
							fetchJSON('/api/anthropic/login', { method: 'DELETE' }).catch(function () { /* best effort */ });
							// Back to Settings, where the login was started --
							// it shows the freshly-created account.
							openSettings();
						},
					});
				}
			})
			.catch(function (e) {
				anthropicAccountBusy = false;
				renderAnthropicAccountsPanelNow();
				window.alert('Could not start the login helper: ' + e.message);
			});
	}

	// --- agent image panel -------------------------------------------------

	function mapAgentImage(data) {
		state.agentImage = window.Render.harnessImageTags(data);
	}

	// agentImageAction is the tag-manager action currently in flight, or
	// null: {kind, label}. It lives HERE and not as a `disabled` attr on a
	// button because the 3s poll repaints the section on every tick -- an
	// attr dies with the button it sat on, but every render (the poll's
	// included) reads this, so a minutes-long Pull latest keeps its busy
	// line across repaints and cannot be double-fired.
	var agentImageAction = null;
	// agentImageError / agentImageNote likewise persist ACROSS poll
	// repaints -- a failure the user has not read yet must not be wiped by
	// the next tick. Both clear when the next action starts.
	var agentImageError = null;
	var agentImageNote = null;

	// renderAgentImagePanelNow writes the section body, and only when Settings
	// is open. state.agentImage is fed separately (refreshAgentImagePanel
	// below) precisely so the create/update forms' image-tag picker keeps
	// working whether or not this page was ever opened.
	function renderAgentImagePanelNow() {
		var body = settingsBody('agent-image');
		if (!body) return;
		// noTitle: the section heading already says "Agent image".
		body.innerHTML = window.Render.renderAgentImagePanel(state.agentImage, {
			noTitle: true,
			busy: agentImageAction,
			error: agentImageError,
			note: agentImageNote,
		});
		wireAgentImagePanel();
	}

	// wireAgentImagePanel attaches the three action buttons and every
	// per-tag Delete. Called by renderAgentImagePanelNow against whatever
	// nodes exist right now -- the poll repaint replaces them wholesale, so
	// no captured node is safe across a tick; that is why the busy state is
	// the module-level agentImageAction instead.
	function wireAgentImagePanel() {
		var body = settingsBody('agent-image');
		if (!body) return;

		var refreshBtn = body.querySelector('.agent-image-panel__refresh');
		if (refreshBtn) refreshBtn.addEventListener('click', function () {
			startAgentImageAction('refresh', 'Checking for new tags…', function () {
				return fetchJSON('/api/agent-image/refresh', { method: 'POST' });
			});
		});

		var cleanupBtn = body.querySelector('.agent-image-panel__cleanup');
		if (cleanupBtn) cleanupBtn.addEventListener('click', function () {
			window.OperatorConfirm.show(
				'Removes every agent image tag that no agent references, except each harness\'s newest. Tags an agent still uses are never removed; a locally built image cannot be pulled again.',
				{ title: 'Clean up unused image tags?', confirmLabel: 'Clean up', danger: true }
			).then(function (confirmed) {
				if (!confirmed) return;
				startAgentImageAction('cleanup', 'Removing unused image tags…', function () {
					return fetchJSON('/api/agent-image/cleanup', { method: 'POST' });
				});
			});
		});

		var pullBtn = body.querySelector('.agent-image-panel__pull');
		if (pullBtn) pullBtn.addEventListener('click', function () {
			// No confirm: pulling is additive, never destructive.
			startAgentImageAction('pull', 'Pulling the newest published image — this can take a few minutes…', function () {
				return fetchJSON('/api/agent-image/pull-latest', { method: 'POST' });
			});
		});

		body.querySelectorAll('.agent-image-panel__delete').forEach(function (btn) {
			btn.addEventListener('click', function () {
				var h = btn.getAttribute('data-harness');
				var tag = btn.getAttribute('data-tag');
				window.OperatorConfirm.show(
					'Remove the image tag "' + tag + '"? It can always be pulled again — unless it was built locally.',
					{ title: 'Remove image tag "' + tag + '"?', confirmLabel: 'Remove', danger: true }
				).then(function (confirmed) {
					if (!confirmed) return;
					startAgentImageAction('delete', 'Removing ' + tag + '…', function () {
						return fetchJSON(
							'/api/agent-image/tags/' + encodeURIComponent(h) + '/' + encodeURIComponent(tag),
							{ method: 'DELETE' }
						);
					});
				});
			});
		});
	}

	// startAgentImageAction runs one tag-manager action to completion, with
	// the busy/error/note lifecycle around it. Exactly one action runs at a
	// time (the render disables every button while one is in flight, and
	// this guard is the backstop); every action response carries the
	// refreshed {harnesses} map, and cleanup/pull add a `report` the note
	// summarizes. The confirm dialogs open BEFORE this runs, so the section
	// can still repaint underneath a dialog without breaking it -- a dialog
	// lives on document.body, not inside the section.
	function startAgentImageAction(kind, label, run) {
		if (agentImageAction) return;
		agentImageAction = { kind: kind, label: label };
		agentImageError = null;
		agentImageNote = null;
		renderAgentImagePanelNow();
		run().then(function (data) {
			mapAgentImage(data);
			agentImageNote = window.Render.agentImageReportNote(data);
			agentImageAction = null;
			renderAgentImagePanelNow();
		}, function (e) {
			agentImageAction = null;
			agentImageError = e.message;
			renderAgentImagePanelNow();
		});
	}

	// refreshAgentImagePanel ALWAYS updates state.agentImage, and only then
	// repaints the section body if it is on screen. The 3s poll drives this
	// for the tag picker's benefit as much as the panel's, so the state feed
	// must not become conditional on the Settings page being open.
	function refreshAgentImagePanel() {
		return fetchJSON('/api/agent-image/tags')
			.then(function (data) {
				mapAgentImage(data);
				renderAgentImagePanelNow();
			})
			.catch(function (e) {
				var body = settingsBody('agent-image');
				if (!body) return;
				body.innerHTML =
					'<span class="agent-image-panel__status--unavailable">' +
					window.Render.escapeHTML('unavailable: ' + e.message) + '</span>';
			});
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

	// The Activity page follows the issue's pattern -- pure markup in
	// render.js, claim/release in app.js -- with no dashboard.js middle file:
	// the page has no fetch and no listeners, and the mounted-check
	// (renderDashboardNow) has to live here anyway, since app.js owns mainArea
	// and state.agents.
	if (activityBtn) {
		activityBtn.addEventListener('click', function () {
			// Exactly the Files teardown: tearing the slot down kills the
			// terminal *viewer* (xterm + its WebSocket, now on a detached node);
			// the agent's own session and container are untouched, and clicking
			// the agent again reattaches a fresh viewer. Render directly here,
			// NOT via renderDashboardNow -- the page isn't mounted yet at click
			// time, so the guard would bail. render.js loads before app.js by
			// construction, so no typeof guard is needed.
			if (typeof window.teardownActiveView === 'function') window.teardownActiveView();
			state.selectedID = null;
			renderSidebar();
			mainArea.innerHTML = window.Render.renderDashboard(state.agents);
		});
	}

	if (settingsBtn) settingsBtn.addEventListener('click', openSettings);

	// onAgentDeleted is called by terminal.js after a successful DELETE. The
	// main area is reset only when the deleted agent is the one on screen
	// (agentChangePolicy); deleting any other agent just updates the list, so
	// a view that has nothing to do with it -- Settings, Files, another
	// agent's terminal -- is left alone.
	window.onAgentDeleted = function (id) {
		var resetMain = window.Render.agentChangePolicy('delete', state.selectedID, id) === 'reset-main-area';
		if (resetMain) state.selectedID = null;
		refreshAgents()
			.then(function () { if (resetMain) showPlaceholder(); })
			.catch(function () { /* matches today: a failed refresh shows no placeholder */ });
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
	window.getAnthropicAccounts = function () { return state.anthropicAccounts; };

	refreshAgents().catch(function (e) {
		sidebarList.innerHTML =
			'<li class="agent-list__error">Failed to load agents: ' + window.Render.escapeHTML(e.message) + '</li>';
	});
	// The image tags and Anthropic accounts are both fetched at load time:
	// the 3s poll below keeps feeding state.agentImage / state.anthropicAccounts
	// for the create/update forms' pickers, whether or not Settings is ever
	// opened.
	refreshAgentImagePanel();
	refreshAnthropicAccounts();

	// Poll for status changes (creating -> running, an unexpected stop, etc.)
	// every few seconds. Simplest correct approach for a V1 local tool with a
	// handful of agents at most -- no push channel needed yet.
	setInterval(function () {
		refreshAgents().catch(function () { /* transient failure; retried next tick */ });
		refreshAgentImagePanel().catch(function () { /* transient failure; retried next tick */ });
		refreshAnthropicAccounts().catch(function () { /* transient failure; retried next tick */ });
	}, 3000);
})();
