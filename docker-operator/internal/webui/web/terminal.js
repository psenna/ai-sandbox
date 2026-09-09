// terminal.js -- the agent detail view: an editable name/description
// header, an options menu (View context / Agent info), a delete button
// with a confirmation prompt, and an xterm.js
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
	// starts from a clean slate. contextOverlay / infoOverlay hold the
	// "View context" / "Agent info" overlays' teardown while one is open --
	// they live on document.body, not inside the detail view, so they are
	// torn down explicitly here too.
	var current = null;
	var contextOverlay = null;
	var infoOverlay = null;

	function teardownCurrent() {
		closeContextOverlay();
		closeInfoOverlay();
		if (current) {
			current.teardown();
			current = null;
		}
	}

	async function fetchJSON(url, options) {
		var doFetch = (typeof window !== 'undefined' && window.OperatorAuth && window.OperatorAuth.fetch) || fetch;
		var resp = await doFetch(url, options);
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

	// fetchText is fetchJSON's sibling for endpoints that return raw text --
	// GET /api/agents/{id}/output serves text/plain (its bytes are not
	// guaranteed valid UTF-8-safe JSON), so it must not be JSON-parsed.
	async function fetchText(url) {
		var doFetch = (typeof window !== 'undefined' && window.OperatorAuth && window.OperatorAuth.fetch) || fetch;
		var resp = await doFetch(url);
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
		return text;
	}

	function wsURL(path) {
		// auth.js builds the URL and appends ?token= when the operator API is
		// authenticated (a browser cannot set a header on a WS handshake).
		if (typeof window !== 'undefined' && window.OperatorAuth && window.OperatorAuth.wsURL) {
			return window.OperatorAuth.wsURL(path);
		}
		var proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
		return proto + '//' + window.location.host + path;
	}

	// attachTerminal opens an xterm.js terminal in termEl bridged to the
	// WebSocket at wsPath (issue #72's protocol: binary frames = PTY bytes,
	// a JSON TEXT frame = resize). Returns { teardown } -- shared by the
	// agent detail view and the Anthropic login view.
	function attachTerminal(termEl, wsPath) {
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

		// A trackpad two-finger scroll (or a mouse wheel) over a full-screen
		// TUI -- tmux, and `claude` inside it -- gets turned by xterm.js into
		// arrow-key presses ("alternate scroll mode"), which the shell reads as
		// "walk my command history". That is never what the viewer wants, and
		// the alternate screen has no scrollback to move anyway. Swallow the
		// wheel in exactly that state; leave it alone when there IS scrollback
		// (normal buffer) or when the app is tracking the mouse itself and can
		// scroll on its own. See shouldForwardWheel.
		if (typeof term.attachCustomWheelEventHandler === 'function') {
			term.attachCustomWheelEventHandler(function () {
				var bufferType = term.buffer && term.buffer.active ? term.buffer.active.type : 'normal';
				var mouseMode = term.modes ? term.modes.mouseTrackingMode : 'none';
				return shouldForwardWheel(bufferType, mouseMode);
			});
		}

		var socket = new WebSocket(wsURL(wsPath));
		socket.binaryType = 'arraybuffer';

		function sendResize() {
			if (socket.readyState !== WebSocket.OPEN) return;
			socket.send(resizeFrame(term.cols, term.rows));
		}

		socket.addEventListener('open', sendResize);
		socket.addEventListener('message', function (ev) {
			if (ev.data instanceof ArrayBuffer) {
				term.write(new Uint8Array(ev.data));
			} else {
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

		var resizeObserver = new ResizeObserver(function () { fitAddon.fit(); });
		resizeObserver.observe(termEl);

		return {
			teardown: function () {
				resizeObserver.disconnect();
				try { socket.close(); } catch (e) { /* already closed or never opened */ }
				term.dispose();
			},
		};
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

	// shouldForwardWheel decides whether xterm.js should process a wheel event
	// (return true) or ignore it entirely (return false). It is ignored only on
	// the alternate screen with no mouse tracking active -- the one state where
	// xterm.js would otherwise synthesise history-walking arrow keys. Pure so
	// terminal.test.js can cover the truth table without a DOM.
	function shouldForwardWheel(bufferType, mouseTrackingMode) {
		return !(bufferType === 'alternate' && (mouseTrackingMode || 'none') === 'none');
	}

	var HTML_ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
	function escapeHTML(s) {
		return String(s).replace(/[&<>"']/g, function (c) { return HTML_ESCAPES[c]; });
	}

	// terminalTextToPlain turns the raw captured pane bytes (GET
	// /api/agents/{id}/output -- ANSI colours, cursor moves, OSC titles,
	// progress-bar carriage returns and all) into a plain-text transcript that
	// reads and searches cleanly in the View context overlay. It is a
	// best-effort flattening, not a terminal emulator: a redrawn TUI frame
	// still leaves its text behind, which is fine for reading and Ctrl-F.
	function terminalTextToPlain(raw) {
		var s = String(raw == null ? '' : raw);
		s = s.replace(/\r\n/g, '\n');
		// OSC (window title, etc.) and DCS/PM/APC strings: ESC ] / P / ^ / _
		// ... terminated by BEL or ESC \.
		s = s.replace(/\x1b[\]P^_][\s\S]*?(?:\x07|\x1b\\)/g, '');
		// CSI: ESC [ params intermediates final.
		s = s.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '');
		// Any other short ESC sequence (charset selection, save/restore, ...).
		s = s.replace(/\x1b[ -/]*[0-~]/g, '');
		s = s.replace(/\x1b/g, '');
		// Resolve carriage returns: what a viewer would have seen is whatever
		// was on the line after the last CR.
		s = s.split('\n').map(function (line) {
			var cr = line.lastIndexOf('\r');
			return cr === -1 ? line : line.slice(cr + 1);
		}).join('\n');
		// Drop the remaining C0 controls (keep TAB and NEWLINE) and DEL.
		s = s.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '');
		s = s.split('\n').map(function (l) { return l.replace(/[ \t]+$/, ''); }).join('\n');
		s = s.replace(/\n{3,}/g, '\n\n').replace(/^\n+/, '');
		return s.replace(/\s+$/, '');
	}

	// buildContextHTML renders text as escaped HTML with every case-insensitive
	// occurrence of query wrapped in <mark>. Pure -- the overlay then walks the
	// <mark> nodes for prev/next navigation.
	function buildContextHTML(text, query) {
		var escaped = escapeHTML(text);
		var q = String(query == null ? '' : query).trim();
		if (!q) return escaped;
		var needle = escapeHTML(q).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
		return escaped.replace(new RegExp(needle, 'gi'), function (m) { return '<mark>' + m + '</mark>'; });
	}

	// clampToLastLines keeps the last `max` lines of text, prefixing a one-line
	// notice when it had to drop earlier ones. The View context overlay is a
	// "what has this agent been doing lately" view, not a full archive, and a
	// bounded line count is what keeps the <pre> render (and the per-keystroke
	// search highlight) instant on a long-lived agent.
	function clampToLastLines(text, max) {
		var lines = String(text == null ? '' : text).split('\n');
		if (lines.length <= max) return String(text == null ? '' : text);
		var hidden = lines.length - max;
		return '[… ' + hidden + ' earlier line' + (hidden === 1 ? '' : 's') + ' hidden …]\n\n' +
			lines.slice(lines.length - max).join('\n');
	}

	// readBufferLines turns one xterm.js buffer (viewport + its scrollback) into
	// an array of trimmed text lines.
	function readBufferLines(buf) {
		var out = [];
		var n = buf.length;
		for (var i = 0; i < n; i++) {
			var ln = buf.getLine(i);
			out.push(ln ? ln.translateToString(true) : '');
		}
		return out;
	}

	// replayCaptureToText feeds the raw captured PTY bytes through a headless
	// xterm.js so the agent's TUI redraws resolve to their final on-screen
	// state -- and genuinely scrolled-off history lands in scrollback --
	// instead of piling up as tens of thousands of near-duplicate raw frames
	// (a busy agent's log is ~95% redraw) with cursor-addressed words run
	// together. The buffer is then read back as plain text: the normal buffer
	// (which carries the scrolled conversation history), plus the alternate
	// screen's current contents when the capture ends inside a full-screen
	// view. Falls back to terminalTextToPlain if xterm is unavailable or the
	// replay throws.
	function replayCaptureToText(raw) {
		return new Promise(function (resolve) {
			var s = typeof raw === 'string' ? raw : String(raw == null ? '' : raw);
			if (s === '') {
				resolve('');
				return;
			}
			if (typeof window === 'undefined' || !window.Terminal) {
				resolve(terminalTextToPlain(s));
				return;
			}
			var replay;
			try {
				replay = new window.Terminal({ cols: 200, rows: 50, scrollback: 50000 });
			} catch (e) {
				resolve(terminalTextToPlain(s));
				return;
			}
			var done = false;
			var safety = null;
			var finish = function () {
				if (done) return;
				done = true;
				if (safety) { clearTimeout(safety); safety = null; }
				var text = '';
				try {
					var ns = replay.buffer;
					var lines = readBufferLines(ns.normal);
					if (ns.active && ns.active.type === 'alternate') {
						lines.push('');
						lines = lines.concat(readBufferLines(ns.active));
					}
					text = lines.join('\n').replace(/\n{3,}/g, '\n\n').replace(/^\n+/, '').replace(/\s+$/, '');
				} catch (e) {
					text = '';
				}
				try { replay.dispose(); } catch (e) { /* already disposed */ }
				resolve(text || terminalTextToPlain(s));
			};
			try {
				replay.write(s, finish);
			} catch (e) {
				try { replay.dispose(); } catch (e2) { /* ignore */ }
				resolve(terminalTextToPlain(s));
				return;
			}
			// Safety net: if write's callback never fires, don't leave the
			// overlay stuck on "Loading…". Cleared by finish on the normal path
			// so it never keeps a timer (or the event loop) alive.
			safety = setTimeout(finish, 15000);
		});
	}

	function renderAgentDetail(container, agentID) {
		teardownCurrent();

		container.innerHTML =
			'<div class="detail">' +
				'<div class="detail__header">' +
					'<input class="detail__name" type="text" placeholder="(unnamed)" aria-label="Agent name">' +
					'<input class="detail__description" type="text" placeholder="Add a description…" aria-label="Agent description">' +
					'<span class="detail__id" title="agent id"></span>' +
					'<span class="detail__save-status" aria-live="polite"></span>' +
					'<span class="detail__menu">' +
						'<button class="detail__menu-btn" type="button" aria-haspopup="menu" aria-expanded="false" title="Options">⋮</button>' +
						'<div class="detail__menu-panel" role="menu" hidden>' +
							'<button class="detail__menu-item" type="button" role="menuitem" data-action="view-context">View context</button>' +
							'<button class="detail__menu-item" type="button" role="menuitem" data-action="agent-info">Agent info</button>' +
						'</div>' +
					'</span>' +
					'<button class="detail__delete-btn" type="button">Delete</button>' +
				'</div>' +
				'<div class="detail__terminal"></div>' +
			'</div>';

		var nameInput = container.querySelector('.detail__name');
		var descInput = container.querySelector('.detail__description');
		var idEl = container.querySelector('.detail__id');
		var saveStatus = container.querySelector('.detail__save-status');
		var menuBtn = container.querySelector('.detail__menu-btn');
		var menuPanel = container.querySelector('.detail__menu-panel');
		var deleteBtn = container.querySelector('.detail__delete-btn');
		var termEl = container.querySelector('.detail__terminal');

		var destroyed = false;

		fetchJSON('/api/agents/' + encodeURIComponent(agentID))
			.then(function (agent) {
				if (destroyed) return;
				nameInput.value = agent.name || '';
				descInput.value = agent.description || '';
				idEl.textContent = agent.id;
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

		// --- options menu ----------------------------------------------------
		// Press the ⋮ button to open; press it again, click outside, or press
		// Esc to close. The document listeners live only while the menu is
		// open (like the context overlay's) so a swapped view never leaves a
		// stale listener behind.
		var menuListeners = null;

		function closeMenu() {
			menuPanel.hidden = true;
			menuBtn.setAttribute('aria-expanded', 'false');
			if (menuListeners) {
				document.removeEventListener('mousedown', menuListeners.onDown);
				document.removeEventListener('keydown', menuListeners.onKey);
				menuListeners = null;
			}
		}

		function openMenu() {
			menuPanel.hidden = false;
			menuBtn.setAttribute('aria-expanded', 'true');
			menuListeners = {
				// A mousedown anywhere that is not the panel or its trigger
				// button closes the menu (the click on the trigger itself is
				// what toggles it, so the button is excluded here).
				onDown: function (ev) {
					if (!menuPanel.contains(ev.target) && ev.target !== menuBtn) closeMenu();
				},
				onKey: function (ev) {
					if (ev.key === 'Escape') closeMenu();
				},
			};
			document.addEventListener('mousedown', menuListeners.onDown);
			document.addEventListener('keydown', menuListeners.onKey);
		}

		menuBtn.addEventListener('click', function () {
			if (menuPanel.hidden) openMenu();
			else closeMenu();
		});

		menuPanel.querySelectorAll('.detail__menu-item').forEach(function (item) {
			item.addEventListener('click', function () {
				closeMenu();
				var action = item.getAttribute('data-action');
				if (action === 'view-context') openContextOverlay(agentID);
				else if (action === 'agent-info') openInfoOverlay(agentID);
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

		var t = attachTerminal(termEl, '/ws/agents/' + encodeURIComponent(agentID) + '/terminal');
		current = {
			teardown: function () {
				destroyed = true;
				closeMenu();
				t.teardown();
			},
		};
	}

	// --- View context overlay --------------------------------------------------

	// MAX_CONTEXT_LINES caps what the overlay renders (see clampToLastLines).
	var MAX_CONTEXT_LINES = 8000;

	// openContextOverlay shows the agent's whole captured transcript (GET
	// /api/agents/{id}/output) in a searchable full-screen overlay: type to
	// filter/highlight, Enter / Shift+Enter (or the arrows) to walk matches,
	// Esc or Close to dismiss. Only one is ever open; it is torn down on close
	// and whenever the detail view is swapped (teardownCurrent).
	function openContextOverlay(agentID) {
		closeContextOverlay();

		var overlay = document.createElement('div');
		overlay.className = 'context-overlay';
		overlay.innerHTML =
			'<div class="context-overlay__panel" role="dialog" aria-label="Agent context">' +
				'<div class="context-overlay__bar">' +
					'<strong class="context-overlay__title">Context</strong>' +
					'<input class="context-overlay__search" type="search" placeholder="Search transcript…" aria-label="Search transcript">' +
					'<span class="context-overlay__count" aria-live="polite"></span>' +
					'<button class="context-overlay__prev" type="button" title="Previous match (Shift+Enter)" disabled>▲</button>' +
					'<button class="context-overlay__next" type="button" title="Next match (Enter)" disabled>▼</button>' +
					'<button class="context-overlay__refresh" type="button">Refresh</button>' +
					'<button class="context-overlay__close" type="button">Close</button>' +
				'</div>' +
				'<pre class="context-overlay__body" tabindex="0">Loading…</pre>' +
			'</div>';
		document.body.appendChild(overlay);

		var bodyEl = overlay.querySelector('.context-overlay__body');
		var searchEl = overlay.querySelector('.context-overlay__search');
		var countEl = overlay.querySelector('.context-overlay__count');
		var prevBtn = overlay.querySelector('.context-overlay__prev');
		var nextBtn = overlay.querySelector('.context-overlay__next');

		var plain = '';
		var matches = [];
		var currentMatch = -1;
		var searchTimer = null;

		function renderBody(preserveBottom) {
			var atBottom = bodyEl.scrollHeight - bodyEl.scrollTop - bodyEl.clientHeight < 4;
			var q = searchEl.value.trim();
			if (q) {
				// innerHTML only while searching -- it is what lets us wrap and
				// walk <mark> nodes. Bounded by clampToLastLines, so the escape
				// + highlight pass stays cheap per keystroke.
				bodyEl.innerHTML = buildContextHTML(plain, q);
				matches = Array.prototype.slice.call(bodyEl.getElementsByTagName('mark'));
			} else {
				// The common case: a plain text node renders an order of
				// magnitude faster than the equivalent innerHTML and never
				// blocks the tab, however long the transcript is.
				bodyEl.textContent = plain;
				matches = [];
			}
			currentMatch = matches.length ? 0 : -1;
			prevBtn.disabled = nextBtn.disabled = matches.length === 0;
			updateCount();
			if (matches.length) paintCurrent(true);
			else if (preserveBottom && atBottom) bodyEl.scrollTop = bodyEl.scrollHeight;
		}

		function updateCount() {
			if (!searchEl.value.trim()) { countEl.textContent = ''; return; }
			countEl.textContent = matches.length ? (currentMatch + 1) + ' / ' + matches.length : 'no matches';
		}

		function paintCurrent(scroll) {
			for (var i = 0; i < matches.length; i++) {
				matches[i].className = i === currentMatch ? 'is-current' : '';
			}
			if (scroll && currentMatch >= 0) {
				matches[currentMatch].scrollIntoView({ block: 'center' });
			}
		}

		function step(delta) {
			if (!matches.length) return;
			currentMatch = (currentMatch + delta + matches.length) % matches.length;
			paintCurrent(true);
			updateCount();
		}

		function load() {
			bodyEl.textContent = 'Loading…';
			var mine = load.token = {};
			fetchText('/api/agents/' + encodeURIComponent(agentID) + '/output')
				.then(function (raw) { return replayCaptureToText(raw); })
				.then(function (text) {
					if (load.token !== mine || !overlay.parentNode) return; // superseded / closed
					plain = clampToLastLines(text, MAX_CONTEXT_LINES) || '(no output captured yet)';
					renderBody(true);
					if (!searchEl.value) bodyEl.scrollTop = bodyEl.scrollHeight;
				})
				.catch(function (e) {
					if (load.token !== mine || !overlay.parentNode) return;
					bodyEl.textContent = 'Failed to load context: ' + (e && e.message ? e.message : e);
				});
		}

		function onKeydown(ev) {
			if (ev.key === 'Escape') {
				closeContextOverlay();
			} else if ((ev.ctrlKey || ev.metaKey) && (ev.key === 'f' || ev.key === 'F')) {
				ev.preventDefault();
				searchEl.focus();
				searchEl.select();
			}
		}

		searchEl.addEventListener('input', function () {
			clearTimeout(searchTimer);
			searchTimer = setTimeout(function () { searchTimer = null; renderBody(false); }, 120);
		});
		searchEl.addEventListener('keydown', function (ev) {
			if (ev.key !== 'Enter') return;
			ev.preventDefault();
			if (searchTimer) { clearTimeout(searchTimer); searchTimer = null; renderBody(false); }
			else step(ev.shiftKey ? -1 : 1);
		});
		prevBtn.addEventListener('click', function () { step(-1); });
		nextBtn.addEventListener('click', function () { step(1); });
		overlay.querySelector('.context-overlay__refresh').addEventListener('click', load);
		overlay.querySelector('.context-overlay__close').addEventListener('click', closeContextOverlay);
		overlay.addEventListener('mousedown', function (ev) {
			if (ev.target === overlay) closeContextOverlay();
		});
		document.addEventListener('keydown', onKeydown);

		contextOverlay = {
			teardown: function () {
				load.token = null;
				clearTimeout(searchTimer);
				document.removeEventListener('keydown', onKeydown);
				if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
				contextOverlay = null;
			},
		};

		load();
		searchEl.focus();
	}

	function closeContextOverlay() {
		if (contextOverlay) contextOverlay.teardown();
	}

	// --- Agent info overlay ----------------------------------------------------

	// openInfoOverlay shows the agent's create-time parameters and the
	// operator-level defaults (GET /api/agents/{id}/info) in a small
	// full-screen overlay: one request, two sections, rendered by
	// render.js's pure renderAgentInfo. Esc, an outside mousedown, or
	// Close dismisses; it is torn down on close and whenever the detail
	// view is swapped (teardownCurrent), same as the context overlay.
	function openInfoOverlay(agentID) {
		closeInfoOverlay();

		var overlay = document.createElement('div');
		overlay.className = 'info-overlay';
		overlay.innerHTML =
			'<div class="info-overlay__panel" role="dialog" aria-label="Agent info">' +
				'<div class="info-overlay__bar">' +
					'<strong class="info-overlay__title">Agent info</strong>' +
					'<button class="info-overlay__close" type="button">Close</button>' +
				'</div>' +
				'<div class="info-overlay__body">Loading…</div>' +
			'</div>';
		document.body.appendChild(overlay);

		var bodyEl = overlay.querySelector('.info-overlay__body');
		var mine = { token: true };

		fetchJSON('/api/agents/' + encodeURIComponent(agentID) + '/info')
			.then(function (info) {
				if (!mine.token || !overlay.parentNode) return; // superseded / closed
				bodyEl.innerHTML = window.Render.renderAgentInfo(info);
			})
			.catch(function (e) {
				if (!mine.token || !overlay.parentNode) return;
				bodyEl.textContent = 'Failed to load agent info: ' + (e && e.message ? e.message : e);
			});

		function onKeydown(ev) {
			if (ev.key === 'Escape') closeInfoOverlay();
		}
		overlay.querySelector('.info-overlay__close').addEventListener('click', closeInfoOverlay);
		overlay.addEventListener('mousedown', function (ev) {
			if (ev.target === overlay) closeInfoOverlay();
		});
		document.addEventListener('keydown', onKeydown);

		infoOverlay = {
			teardown: function () {
				mine.token = false;
				document.removeEventListener('keydown', onKeydown);
				if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
				infoOverlay = null;
			},
		};
	}

	function closeInfoOverlay() {
		if (infoOverlay) infoOverlay.teardown();
	}

	// renderAnthropicLogin renders the "Log in with your Claude subscription"
	// view: a terminal attached to /ws/anthropic/login/terminal (running
	// `claude setup-token`), plus a field to paste the token it prints.
	// opts.submitToken(token) returns a promise; opts.onClose() is called
	// after a successful submit or a Cancel.
	function renderAnthropicLogin(container, opts) {
		teardownCurrent();
		opts = opts || {};

		container.innerHTML =
			'<div class="detail">' +
				'<div class="detail__header">' +
					'<strong>Anthropic login</strong>' +
					'<span class="detail__save-status" aria-live="polite"></span>' +
					'<button class="login__close" type="button">Cancel</button>' +
				'</div>' +
				'<p class="login__hint">Run <code>claude setup-token</code> in the terminal below, complete the sign-in in your browser, then paste the token it prints here. It is stored once and used by every agent set to the Anthropic backend.</p>' +
				'<div class="detail__terminal login__terminal"></div>' +
				'<form class="login__form">' +
					'<input class="login__token" type="text" placeholder="Paste the token (starts with sk-ant-oat…)" aria-label="Anthropic OAuth token">' +
					'<button class="login__save" type="submit">Save token</button>' +
				'</form>' +
			'</div>';

		var termEl = container.querySelector('.login__terminal');
		var status = container.querySelector('.detail__save-status');
		var tokenInput = container.querySelector('.login__token');
		var saveBtn = container.querySelector('.login__save');

		var t = attachTerminal(termEl, '/ws/anthropic/login/terminal');
		current = { teardown: t.teardown };

		container.querySelector('.login__close').addEventListener('click', function () {
			if (typeof opts.onClose === 'function') opts.onClose();
		});

		container.querySelector('.login__form').addEventListener('submit', function (ev) {
			ev.preventDefault();
			var token = tokenInput.value.trim();
			if (!token) return;
			saveBtn.disabled = true;
			status.textContent = 'Saving…';
			Promise.resolve(opts.submitToken ? opts.submitToken(token) : null)
				.then(function () {
					status.textContent = 'Saved';
					if (typeof opts.onClose === 'function') opts.onClose();
				})
				.catch(function (e) {
					saveBtn.disabled = false;
					status.textContent = 'Save failed: ' + (e && e.message ? e.message : e);
				});
		});
	}

	// In a browser, wire up the real entry point app.js calls into. Under
	// Node (terminal.test.js), `window` doesn't exist at all -- skip this
	// assignment rather than throw, since only the pure helpers below are
	// under test there.
	if (typeof window !== 'undefined') {
		window.renderAgentDetail = renderAgentDetail;
		window.renderAnthropicLogin = renderAnthropicLogin;
		// So app.js can tear down a terminal/WebSocket when it swaps the
		// main area back to the placeholder without rendering a new view.
		window.teardownActiveView = teardownCurrent;
	}

	// Exported for terminal.test.js only -- renderAgentDetail itself needs a
	// real DOM/xterm/WebSocket and is verified by manual review plus the
	// integration test against a running backend instead (see PR notes).
	var TerminalProtocol = {
		resizeFrame: resizeFrame,
		encodeKeystroke: encodeKeystroke,
		shouldForwardWheel: shouldForwardWheel,
		terminalTextToPlain: terminalTextToPlain,
		buildContextHTML: buildContextHTML,
		clampToLastLines: clampToLastLines,
		replayCaptureToText: replayCaptureToText,
	};
	if (typeof module !== 'undefined' && module.exports) {
		module.exports = TerminalProtocol;
	} else {
		window.TerminalProtocol = TerminalProtocol;
	}
})();
