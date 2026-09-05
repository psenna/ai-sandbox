// auth.test.js -- the one pure helper in auth.js (appendTokenParam). The rest
// of auth.js is browser wiring (localStorage, window.location, fetch, prompt)
// that this project has no jsdom/headless tooling for -- same rationale as
// render.test.js's header -- and is covered by the Go-side authmw tests plus
// manual review.
//
// Run with: node --test web/auth.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const { appendTokenParam } = require('./auth.js');

test('appendTokenParam: no token leaves the URL untouched', () => {
	assert.equal(appendTokenParam('/ws/agents/agt_1/terminal', ''), '/ws/agents/agt_1/terminal');
	assert.equal(appendTokenParam('/ws/agents/agt_1/terminal', null), '/ws/agents/agt_1/terminal');
});

test('appendTokenParam: adds ?token= to a URL with no query', () => {
	assert.equal(
		appendTokenParam('ws://host/ws/agents/agt_1/terminal', 'abc'),
		'ws://host/ws/agents/agt_1/terminal?token=abc'
	);
});

test('appendTokenParam: adds &token= to a URL that already has a query', () => {
	assert.equal(appendTokenParam('ws://host/x?a=1', 'abc'), 'ws://host/x?a=1&token=abc');
});

test('appendTokenParam: percent-encodes the token', () => {
	assert.equal(appendTokenParam('ws://host/x', 'a/b c+d'), 'ws://host/x?token=a%2Fb%20c%2Bd');
});
