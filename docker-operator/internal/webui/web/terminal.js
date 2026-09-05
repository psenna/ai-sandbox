// terminal.js -- the agent detail view: an editable name/description
// header, a delete button with a confirmation prompt, and an xterm.js
// terminal wired to the WebSocket terminal bridge (GET
// /ws/agents/{id}/terminal, issue #72's protocol: binary frames carry raw
// PTY bytes each way, a JSON text frame carries {"type":"resize",...}).
//
// Exposes window.renderAgentDetail(container, agentId), which app.js calls
// whenever an agent is selected. Each call tears down any previous
// terminal/WebSocket first, so switching agents (or re-selecting the same
// one) never leaks a connection or a duplicate xterm instance.
(function () {
	'use strict';

	// current holds the live view's teardown, so a new selection always
	// starts from a clean slate.
	var current = null;

	function teardownCurrent() {
		if (current) {
			current.teardown();
			current = null;
		}
	}

	async function fetchJSON(url, options) {
		var resp = await fetch(url, options);
		var text = await resp.text();
		if (!resp.ok) {
			var msg = 'request failed (' + resp.status + ')';
			try {
				var env = JSON.parse(text);
				if (env && env.error && env.error.message) msg = env.error.message;
			} catch (e) {
				// body wasn't JSON; keep the generic message.
			}
			throw new Error(msg);
		}
		return text ? JSON.parse(text) : null;
	}

	function wsURL(agentID) {
		var proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
		return proto + '//' + window.location.host + '/ws/agents/' + encodeURIComponent(agentID) + '/terminal';
	}

	// resizeFrame builds the TEXT control frame the server's
	// internal/wsbridge.handleControl expects on every terminal resize
	// (issue #72's protocol). Kept as a pure, DOM-free function so it can be
	// unit-tested directly, same as render.js.
	function resizeFrame(cols, rows) {
		return JSON.stringify({ type: 'resize', cols: cols, rows: rows });
	}

	// encodeKeystroke turns an xterm.js onData string into the BINARY frame
	// payload the server expects on its exec stdin. This is the fix for a
	// real bug: passing the string straight to WebSocket.send() sends a TEXT
	// frame, which the server routes to its JSON control-frame parser
	// instead of the shell -- every keystroke would be silently dropped.
	function encodeKeystroke(data) {
		return new TextEncoder().encode(data);
	}

	function renderAgentDetail(container, agentID) {
		teardownCurrent();

		container.innerHTML =
			'<div class="detail">' +
				'<div class="detail__header">' +
					'<input class="detail__name" type="text" placeholder="(unnamed)" aria-label="Agent name">' +
					'<input class="detail__description" type="text" placeholder="Add a description…" aria-label="Agent description">' +
					'<span class="detail__save-status" aria-live="polite"></span>' +
					'<button class="detail__delete-btn" type="button">Delete</button>' +
				'</div>' +
				'<div class="detail__terminal"></div>' +
			'</div>';

		var nameInput = container.querySelector('.detail__name');
		var descInput = container.querySelector('.detail__description');
		var saveStatus = container.querySelector('.detail__save-status');
		var deleteBtn = container.querySelector('.detail__delete-btn');
		var termEl = container.querySelector('.detail__terminal');

		var destroyed = false;

		fetchJSON('/api/agents/' + encodeURIComponent(agentID))
			.then(function (agent) {
				if (destroyed) return;
				nameInput.value = agent.name || '';
				descInput.value = agent.description || '';
			})
			.catch(function (e) {
				if (destroyed) return;
				saveStatus.textContent = 'Failed to load agent: ' + e.message;
			});

		function save(field, value) {
			var body = {};
			body[field] = value;
			saveStatus.textContent = 'Saving…';
			fetchJSON('/api/agents/' + encodeURIComponent(agentID), {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body),
			})
				.then(function () { if (!destroyed) saveStatus.textContent = 'Saved'; })
				.catch(function (e) { if (!destroyed) saveStatus.textContent = 'Save failed: ' + e.message; });
		}

		nameInput.addEventListener('blur', function () { save('name', nameInput.value); });
		descInput.addEventListener('blur', function () { save('description', descInput.value); });
		[nameInput, descInput].forEach(function (el) {
			el.addEventListener('keydown', function (ev) {
				if (ev.key === 'Enter') el.blur();
			});
		});

		deleteBtn.addEventListener('click', function () {
			var label = nameInput.value || agentID;
			var confirmed = window.confirm(
				'Delete agent "' + label + '"? This removes its container and all its data. This cannot be undone.'
			);
			if (!confirmed) return;

			deleteBtn.disabled = true;
			fetchJSON('/api/agents/' + encodeURIComponent(agentID), { method: 'DELETE' })
				.then(function () {
					if (typeof window.onAgentDeleted === 'function') window.onAgentDeleted(agentID);
				})
				.catch(function (e) {
					deleteBtn.disabled = false;
					window.alert('Could not delete agent: ' + e.message);
				});
		});

		// --- terminal ------------------------------------------------------

		var term = new window.Terminal({
			convertEol: true,
			cursorBlink: true,
			fontSize: 13,
			fontFamily: 'Menlo, Consolas, "DejaVu Sans Mono", monospace',
			theme: { background: '#1e1e1e' },
		});
		var fitAddon = new window.FitAddon.FitAddon();
		term.loadAddon(fitAddon);
		term.open(termEl);
		fitAddon.fit();

		var socket = new WebSocket(wsURL(agentID));
		socket.binaryType = 'arraybuffer';

		function sendResize() {
			if (socket.readyState !== WebSocket.OPEN) return;
			// A plain string argument to send() is a TEXT frame -- the
			// server's control-frame parser only looks at TEXT frames,
			// exactly as issue #72's protocol specifies.
			socket.send(resizeFrame(term.cols, term.rows));
		}

		socket.addEventListener('open', sendResize);

		socket.addEventListener('message', function (ev) {
			if (ev.data instanceof ArrayBuffer) {
				term.write(new Uint8Array(ev.data));
			} else {
				// The bridge never sends a TEXT message today (only TEXT
				// close-frame reasons, delivered via the close event below,
				// not 'message') -- written to the terminal rather than
				// silently dropped in case that ever changes.
				term.write(ev.data);
			}
		});
		socket.addEventListener('close', function (ev) {
			term.write('\r\n\x1b[90m[connection closed' + (ev.reason ? ': ' + ev.reason : '') + ']\x1b[0m\r\n');
		});
		socket.addEventListener('error', function () {
			term.write('\r\n\x1b[31m[connection error]\x1b[0m\r\n');
		});

		term.onData(function (data) {
			if (socket.readyState !== WebSocket.OPEN) return;
			socket.send(encodeKeystroke(data));
		});

		term.onResize(sendResize);

		var resizeObserver = new ResizeObserver(function () {
			fitAddon.fit();
		});
		resizeObserver.observe(termEl);

		current = {
			teardown: function () {
				destroyed = true;
				resizeObserver.disconnect();
				try { socket.close(); } catch (e) { /* already closed or never opened */ }
				term.dispose();
			},
		};
	}

	// In a browser, wire up the real entry point app.js calls into. Under
	// Node (terminal.test.js), `window` doesn't exist at all -- skip this
	// assignment rather than throw, since only the pure helpers below are
	// under test there.
	if (typeof window !== 'undefined') {
		window.renderAgentDetail = renderAgentDetail;
	}

	// Exported for terminal.test.js only -- renderAgentDetail itself needs a
	// real DOM/xterm/WebSocket and is verified by manual review plus the
	// integration test against a running backend instead (see PR notes).
	var TerminalProtocol = { resizeFrame: resizeFrame, encodeKeystroke: encodeKeystroke };
	if (typeof module !== 'undefined' && module.exports) {
		module.exports = TerminalProtocol;
	} else {
		window.TerminalProtocol = TerminalProtocol;
	}
})();
