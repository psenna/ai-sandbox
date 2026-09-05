// auth.js -- operator API token handling for the browser.
//
// The operator's REST API and terminal WebSockets require a static Bearer
// token whenever OPERATOR_API_TOKEN is set. The browser gets that token
// out-of-band -- it is deliberately NOT embedded in any page the operator
// serves unauthenticated -- in one of two ways:
//   1. ?token=<token> in the URL on first load (then stripped from the bar)
//   2. a value the user pastes when a request first comes back 401
// and it is kept in localStorage after that. When the operator runs with no
// token, every request simply carries no header and none of this matters.
//
// Loaded before render.js/app.js/terminal.js so window.OperatorAuth exists by
// the time they make their first request.
(function () {
	'use strict';

	// appendTokenParam returns url with token=<token> added to its query
	// string (a browser cannot set an Authorization header on a WebSocket
	// handshake, so the terminal routes accept the token this way). Pure and
	// DOM-free so auth.test.js can exercise it directly.
	function appendTokenParam(url, token) {
		if (!token) return url;
		return url + (url.indexOf('?') === -1 ? '?' : '&') + 'token=' + encodeURIComponent(token);
	}

	if (typeof window !== 'undefined') {
		var KEY = 'operator-api-token';

		var readStored = function () {
			try { return window.localStorage.getItem(KEY) || ''; } catch (e) { return ''; }
		};
		var writeStored = function (t) {
			try {
				if (t) { window.localStorage.setItem(KEY, t); } else { window.localStorage.removeItem(KEY); }
			} catch (e) { /* private mode / storage disabled -- keep it in memory only */ }
		};

		var token = readStored();

		// Adopt a token passed in the URL, then scrub it from the address bar
		// so it is not left in history or copied onward.
		try {
			var u = new URL(window.location.href);
			var fromURL = u.searchParams.get('token');
			if (fromURL) {
				token = fromURL.trim();
				writeStored(token);
				u.searchParams.delete('token');
				window.history.replaceState(null, '', u.pathname + (u.search || '') + (u.hash || ''));
			}
		} catch (e) { /* URL unavailable -- ignore */ }

		var setToken = function (t) { token = (t || '').trim(); writeStored(token); };

		var prompting = false;
		var promptForToken = function () {
			if (prompting) return;
			prompting = true;
			// Deferred so the current call stack unwinds (and the caller sees
			// its 401) before prompt() blocks the thread.
			window.setTimeout(function () {
				var t = window.prompt('Operator API token required (the value of OPERATOR_API_TOKEN):');
				prompting = false;
				if (t && t.trim()) { setToken(t); window.location.reload(); }
			}, 0);
		};

		// authFetch is fetch() with the Bearer header attached when a token is
		// known. A 401 clears the stored token and prompts (once) for a new
		// one; the caller still sees the rejection.
		var authFetch = function (url, options) {
			options = options || {};
			if (token) {
				options.headers = Object.assign({}, options.headers, { Authorization: 'Bearer ' + token });
			}
			return fetch(url, options).then(function (resp) {
				if (resp.status === 401) {
					setToken('');
					promptForToken();
				}
				return resp;
			});
		};

		// wsURL builds a ws(s):// URL for path against the current origin,
		// carrying the token as a query parameter when one is known.
		var wsURL = function (path) {
			var proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
			return appendTokenParam(proto + '//' + window.location.host + path, token);
		};

		window.OperatorAuth = {
			fetch: authFetch,
			wsURL: wsURL,
			setToken: setToken,
			hasToken: function () { return !!token; },
		};
	}

	// Exported for auth.test.js only.
	if (typeof module !== 'undefined' && module.exports) {
		module.exports = { appendTokenParam: appendTokenParam };
	}
})();
