// files.js -- DOM wiring for the centralized file store browser (issue
// #122). Mirrors terminal.js's dual-export/guard shape: the pure rendering
// lives in render.js (renderFileBrowser / renderFileTable / renderBreadcrumb,
// unit-tested), this file is the event wiring reviewed by hand.
//
// Exposes window.renderFileBrowser(container), which app.js calls when the
// sidebar "Files" button is clicked.
(function () {
	'use strict';

	function doFetch(url, options) {
		return ((typeof window !== 'undefined' && window.OperatorAuth && window.OperatorAuth.fetch) || fetch)(url, options);
	}

	// apiError unwraps internal/api's {"error":{"message":...}} envelope,
	// falling back to a generic message (same shape as app.js).
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

	function render(container) {
		var state = { path: '' };

		function showError(msg) {
			var el = container.querySelector('.file-browser__error');
			if (!el) return;
			el.textContent = msg;
			el.hidden = !msg;
		}

		function setBusy(busy) {
			container.querySelectorAll('.file-browser__toolbar button, .file-browser__file-input')
				.forEach(function (el) { el.disabled = busy; });
		}

		function load(path) {
			fetchJSON('/api/files?path=' + encodeURIComponent(path || ''))
				.then(function (data) {
					state.path = data.path || '';
					container.innerHTML = window.Render.renderFileBrowser(state.path, data.entries || []);
					bind();
				})
				.catch(function (e) {
					container.innerHTML = window.Render.renderFileBrowser(path || '', []);
					bind();
					showError('Could not load files: ' + e.message);
				});
		}

		function cwdChild(name) {
			return state.path ? state.path + '/' + name : name;
		}

		function bind() {
			container.querySelectorAll('.file-browser__breadcrumb .crumb[data-path]').forEach(function (btn) {
				btn.addEventListener('click', function () { load(btn.getAttribute('data-path')); });
			});
			container.querySelectorAll('.file-table__open').forEach(function (btn) {
				btn.addEventListener('click', function () { load(btn.getAttribute('data-path')); });
			});
			container.querySelectorAll('.file-table__download').forEach(function (btn) {
				btn.addEventListener('click', function () { download(btn.getAttribute('data-path')); });
			});
			container.querySelectorAll('.file-table__delete').forEach(function (btn) {
				btn.addEventListener('click', function () { remove(btn.getAttribute('data-path')); });
			});

			var newFolder = container.querySelector('.file-browser__new-folder');
			if (newFolder) newFolder.addEventListener('click', mkdir);

			var uploadBtn = container.querySelector('.file-browser__upload');
			var fileInput = container.querySelector('.file-browser__file-input');
			if (uploadBtn && fileInput) {
				uploadBtn.addEventListener('click', function () { fileInput.click(); });
				fileInput.addEventListener('change', function () {
					if (fileInput.files && fileInput.files.length) upload(fileInput.files);
					fileInput.value = '';
				});
			}

			var dz = container.querySelector('.file-browser__dropzone');
			if (dz) {
				['dragover', 'dragenter'].forEach(function (ev) {
					dz.addEventListener(ev, function (e) { e.preventDefault(); dz.classList.add('file-browser__dropzone--active'); });
				});
				['dragleave', 'drop'].forEach(function (ev) {
					dz.addEventListener(ev, function () { dz.classList.remove('file-browser__dropzone--active'); });
				});
				dz.addEventListener('drop', function (e) {
					e.preventDefault();
					if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) upload(e.dataTransfer.files);
				});
			}
		}

		function download(path) {
			doFetch('/api/files/download?path=' + encodeURIComponent(path))
				.then(function (resp) {
					if (!resp.ok) return resp.text().then(function (t) { throw apiError(resp.status, t); });
					return resp.blob();
				})
				.then(function (blob) {
					var url = URL.createObjectURL(blob);
					var a = document.createElement('a');
					a.href = url;
					a.download = path.split('/').pop() || 'download';
					document.body.appendChild(a);
					a.click();
					a.remove();
					setTimeout(function () { URL.revokeObjectURL(url); }, 0);
				})
				.catch(function (e) { showError('Download failed: ' + e.message); });
		}

		function remove(path) {
			if (!window.confirm('Delete "' + path + '"? This cannot be undone.')) return;
			fetchJSON('/api/files?path=' + encodeURIComponent(path), { method: 'DELETE' })
				.then(function () { load(state.path); })
				.catch(function (e) { showError('Delete failed: ' + e.message); });
		}

		function mkdir() {
			var name = window.prompt('New folder name:');
			if (!name) return;
			fetchJSON('/api/files/mkdir', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ path: cwdChild(name.trim()) }),
			})
				.then(function () { load(state.path); })
				.catch(function (e) { showError('Could not create the folder: ' + e.message); });
		}

		function upload(fileList) {
			var form = new FormData();
			for (var i = 0; i < fileList.length; i++) form.append('file', fileList[i], fileList[i].name);
			showError('');
			setBusy(true);
			doFetch('/api/files/upload?path=' + encodeURIComponent(state.path), { method: 'POST', body: form })
				.then(function (resp) {
					return resp.text().then(function (t) {
						if (!resp.ok) throw apiError(resp.status, t);
					});
				})
				.then(function () { setBusy(false); load(state.path); })
				.catch(function (e) { setBusy(false); showError('Upload failed: ' + e.message); });
		}

		load('');
	}

	if (typeof window !== 'undefined') {
		window.renderFileBrowser = render;
	}
})();
