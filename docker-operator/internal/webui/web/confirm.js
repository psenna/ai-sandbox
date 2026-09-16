// confirm.js -- a themed, promise-based replacement for window.confirm() for
// this app's destructive/consequential actions (delete agent/template,
// remove the stored Anthropic credential, recreate an agent on update).
// window.confirm() blocks the main thread, can't be styled to match the
// dark theme, and gives every action the same look regardless of severity.
//
// window.OperatorConfirm.show(message, opts) -> Promise<boolean>, resolving
// true on confirm and false on Cancel/Escape/backdrop click. This is pure
// DOM wiring (like app.js/terminal.js) with no jsdom/headless tooling in
// this project, so it is covered by manual review rather than a unit test --
// same rationale as auth.js's header.
(function () {
	'use strict';

	if (typeof window === 'undefined') return; // no-op under node --test

	// current holds the open overlay's { overlay, resolve, onKey } so a
	// second call in flight (should not happen in this app's UI) cancels the
	// first rather than leaving two overlays stacked.
	var current = null;

	function close(result) {
		if (!current) return;
		document.removeEventListener('keydown', current.onKey);
		current.overlay.parentNode.removeChild(current.overlay);
		var resolve = current.resolve;
		current = null;
		resolve(result);
	}

	// show(message, opts): opts.title labels the dialog (default "Confirm");
	// opts.confirmLabel/opts.cancelLabel override the button text (e.g.
	// "Delete"); opts.danger renders the confirm button as the destructive
	// (red) variant instead of the primary (blue) one.
	function show(message, opts) {
		opts = opts || {};
		if (current) close(false);

		return new Promise(function (resolve) {
			var overlay = document.createElement('div');
			overlay.className = 'confirm-overlay';
			overlay.innerHTML =
				'<div class="confirm-overlay__panel" role="alertdialog" aria-modal="true" aria-labelledby="confirm-overlay-title">' +
					'<h2 class="confirm-overlay__title" id="confirm-overlay-title"></h2>' +
					'<p class="confirm-overlay__message"></p>' +
					'<div class="confirm-overlay__actions">' +
						'<button class="btn btn--ghost confirm-overlay__cancel" type="button"></button>' +
						'<button class="btn confirm-overlay__confirm" type="button"></button>' +
					'</div>' +
				'</div>';

			overlay.querySelector('.confirm-overlay__title').textContent = opts.title || 'Confirm';
			overlay.querySelector('.confirm-overlay__message').textContent = message;
			var cancelBtn = overlay.querySelector('.confirm-overlay__cancel');
			var confirmBtn = overlay.querySelector('.confirm-overlay__confirm');
			cancelBtn.textContent = opts.cancelLabel || 'Cancel';
			confirmBtn.textContent = opts.confirmLabel || 'Confirm';
			confirmBtn.classList.add(opts.danger ? 'btn--danger' : 'btn--primary');

			document.body.appendChild(overlay);

			function onKey(ev) {
				if (ev.key === 'Escape') close(false);
			}
			cancelBtn.addEventListener('click', function () { close(false); });
			confirmBtn.addEventListener('click', function () { close(true); });
			overlay.addEventListener('mousedown', function (ev) {
				if (ev.target === overlay) close(false);
			});
			document.addEventListener('keydown', onKey);

			current = { overlay: overlay, resolve: resolve, onKey: onKey };
			confirmBtn.focus();
		});
	}

	window.OperatorConfirm = { show: show };
})();
