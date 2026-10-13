// templates.js -- DOM wiring for the Templates page. Mirrors files.js's
// dual-export/guard shape: pure rendering lives in render.js
// (renderTemplatesPage / renderTemplateForm, unit-tested), this file is
// the event wiring reviewed by hand.
//
// Exposes window.renderTemplatesPage(container), which app.js calls when
// the sidebar "Templates" button is clicked.
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
			description: fd.get('description') || '',
			backend: fd.get('backend') || '',
			harness: fd.get('harness') || '',
			model: fd.get('model') || '',
			fast_model: fd.get('fast_model') || '',
			ollama_url: fd.get('ollama_url') || '',
			repo: fd.get('repo') || '',
			auto_compact_threshold: fd.get('auto_compact_threshold') || '',
			max_context_tokens: fd.get('max_context_tokens') || '',
			image_tag: fd.get('image_tag') || '',
			auto_mode: fd.get('auto_mode') || '',
		};
	}

	function render(container) {
		var state = { templates: [] };

		function showError(msg) {
			var el = container.querySelector('.templates-page__error');
			if (!el) return;
			el.textContent = msg;
			el.hidden = !msg;
		}

		function showDetail(html) {
			var el = container.querySelector('.templates-page__detail');
			if (!el) return;
			el.innerHTML = html;
			el.hidden = !html;
		}

		function load() {
			fetchJSON('/api/templates')
				.then(function (data) {
					state.templates = data.templates || [];
					container.innerHTML = window.Render.renderTemplatesPage(state.templates);
					bind();
				})
				.catch(function (e) {
					container.innerHTML = window.Render.renderTemplatesPage([]);
					bind();
					showError('Could not load templates: ' + e.message);
				});
		}

		function openForm(template) {
			showDetail(window.Render.renderTemplateForm(template));
			bindDetail(template);
		}

		function bindDetail(template) {
			var form = container.querySelector('.template-form');
			if (!form) return;
			form.addEventListener('submit', function (ev) {
				ev.preventDefault();
				var body = JSON.stringify(formToRequest(form));
				var req = template && template.id
					? fetchJSON('/api/templates/' + encodeURIComponent(template.id), { method: 'PUT', body: body, headers: { 'Content-Type': 'application/json' } })
					: fetchJSON('/api/templates', { method: 'POST', body: body, headers: { 'Content-Type': 'application/json' } });
				req.then(function () { showDetail(''); load(); })
					.catch(function (e) { showError('Could not save template: ' + e.message); });
			});
		}

		function bind() {
			var newBtn = container.querySelector('.templates-page__new');
			if (newBtn) newBtn.addEventListener('click', function () { openForm(null); });

			container.querySelectorAll('.templates-page__edit').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-template-id]').getAttribute('data-template-id');
					var t = state.templates.find(function (x) { return x.id === id; });
					if (t) openForm(t);
				});
			});

			container.querySelectorAll('.templates-page__delete').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-template-id]').getAttribute('data-template-id');
					window.OperatorConfirm.show(
						'Deleting this template permanently removes its configuration. This cannot be undone.',
						{ title: 'Delete this template?', confirmLabel: 'Delete', danger: true }
					).then(function (confirmed) {
						if (!confirmed) return;
						fetchJSON('/api/templates/' + encodeURIComponent(id), { method: 'DELETE' })
							.then(load)
							.catch(function (e) { showError('Could not delete template: ' + e.message); });
					});
				});
			});
		}

		load();
	}

	window.renderTemplatesPage = render;
})();
