// cronagents.js -- DOM wiring for the Cron Agents page. Mirrors
// templates.js's dual-export/guard shape: pure rendering lives in
// render.js (renderCronAgentsPage / renderCronAgentForm /
// renderScheduleRunList, unit-tested), this file is the event wiring
// reviewed by hand.
//
// Exposes window.renderCronAgentsPage(container), which app.js calls when
// the sidebar "Cron Agents" button is clicked.
(function () {
	'use strict';

	function doFetch(url, options) {
		return ((typeof window !== 'undefined' && window.OperatorAuth && window.OperatorAuth.fetch) || fetch)(url, options);
	}

	function apiError(status, body) {
		try {
			var env = JSON.parse(body);
			if (env && env.error && env.error.message) return new Error(env.error.message);
		} catch (e) { /* not JSON */ }
		return new Error('request failed (' + status + ')');
	}

	async function fetchJSON(url, options) {
		var resp = await doFetch(url, options);
		var text = await resp.text();
		if (!resp.ok) throw apiError(resp.status, text);
		return text ? JSON.parse(text) : null;
	}

	function formToRequest(form) {
		var fd = new FormData(form);
		return {
			name: fd.get('name') || '',
			template_id: fd.get('template_id') || '',
			schedule: fd.get('schedule') || '',
			enabled: fd.get('enabled') === 'on',
			task: fd.get('task') || '',
			on_complete: fd.get('on_complete') || '',
		};
	}

	function render(container) {
		var state = { cronAgents: [], templates: [] };

		function showError(msg) {
			var el = container.querySelector('.cron-agents-page__error');
			if (!el) return;
			el.textContent = msg;
			el.hidden = !msg;
		}

		function showDetail(html) {
			var el = container.querySelector('.cron-agents-page__detail');
			if (!el) return;
			el.innerHTML = html;
			el.hidden = !html;
		}

		function load() {
			fetchJSON('/api/cron-agents')
				.then(function (data) {
					state.cronAgents = data.cron_agents || [];
					container.innerHTML = window.Render.renderCronAgentsPage(state.cronAgents);
					bind();
				})
				.catch(function (e) {
					container.innerHTML = window.Render.renderCronAgentsPage([]);
					bind();
					showError('Could not load cron agents: ' + e.message);
				});
		}

		function loadTemplatesThenOpenForm(cronAgent) {
			fetchJSON('/api/templates')
				.then(function (data) {
					state.templates = data.templates || [];
					openForm(cronAgent);
				})
				.catch(function (e) {
					showError('Could not load templates: ' + e.message);
				});
		}

		function openForm(cronAgent) {
			showDetail(window.Render.renderCronAgentForm(cronAgent, state.templates));
			if (cronAgent && cronAgent.id) loadRuns(cronAgent.id, cronAgent.on_complete);
			bindDetail(cronAgent);
		}

		function loadRuns(cronAgentID, onComplete) {
			fetchJSON('/api/cron-agents/' + encodeURIComponent(cronAgentID) + '/runs')
				.then(function (data) {
					var detail = container.querySelector('.cron-agents-page__detail');
					if (!detail) return;
					detail.insertAdjacentHTML('beforeend', window.Render.renderScheduleRunList(data.runs || [], onComplete));
					bindRuns();
				})
				.catch(function () { /* run history is best-effort for this view */ });
		}

		// openFiles swaps this page for the Files browser, where an archived
		// transcript lives (transcripts/<id>/).
		function openFiles() {
			if (typeof window.renderFileBrowser === 'function') window.renderFileBrowser(container);
		}

		function bindRuns() {
			container.querySelectorAll('.schedule-run-list__open-agent').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.getAttribute('data-agent-id');
					if (typeof window.openAgent === 'function' && window.openAgent(id)) return;
					openFiles();
				});
			});
			container.querySelectorAll('.schedule-run-list__open-files').forEach(function (btn) {
				btn.addEventListener('click', openFiles);
			});
		}

		function bindDetail(cronAgent) {
			var form = container.querySelector('.cron-agent-form');
			if (!form) return;
			form.addEventListener('submit', function (ev) {
				ev.preventDefault();
				var body = JSON.stringify(formToRequest(form));
				var req = cronAgent && cronAgent.id
					? fetchJSON('/api/cron-agents/' + encodeURIComponent(cronAgent.id), { method: 'PUT', body: body, headers: { 'Content-Type': 'application/json' } })
					: fetchJSON('/api/cron-agents', { method: 'POST', body: body, headers: { 'Content-Type': 'application/json' } });
				req.then(function () { showDetail(''); load(); })
					.catch(function (e) { showError('Could not save cron agent: ' + e.message); });
			});
		}

		function bind() {
			var newBtn = container.querySelector('.cron-agents-page__new');
			if (newBtn) newBtn.addEventListener('click', function () { loadTemplatesThenOpenForm(null); });

			container.querySelectorAll('.cron-agents-page__edit').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-cron-agent-id]').getAttribute('data-cron-agent-id');
					var ca = state.cronAgents.find(function (x) { return x.id === id; });
					if (ca) loadTemplatesThenOpenForm(ca);
				});
			});

			container.querySelectorAll('.cron-agents-page__delete').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-cron-agent-id]').getAttribute('data-cron-agent-id');
					window.OperatorConfirm.show(
						'Deleting this cron agent permanently removes its schedule, task text and completion policy. This cannot be undone.',
						{ title: 'Delete this cron agent?', confirmLabel: 'Delete', danger: true }
					).then(function (confirmed) {
						if (!confirmed) return;
						fetchJSON('/api/cron-agents/' + encodeURIComponent(id), { method: 'DELETE' })
							.then(load)
							.catch(function (e) { showError('Could not delete cron agent: ' + e.message); });
					});
				});
			});
		}

		load();
	}

	window.renderCronAgentsPage = render;
})();
