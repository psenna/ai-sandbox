// terminal.test.js -- unit-tests the two pure protocol helpers extracted
// from terminal.js (resizeFrame, encodeKeystroke). The rest of terminal.js
// (renderAgentDetail) needs a real DOM, xterm.js, and a WebSocket, which
// this project deliberately has no jsdom/headless-browser tooling for (see
// render.test.js's header) -- that half is covered by manual review plus an
// integration test against a real running backend instead.
//
// Run with: node --test web/terminal.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const { resizeFrame, encodeKeystroke } = require('./terminal.js');

test('resizeFrame: builds the exact JSON control frame internal/wsbridge expects', () => {
	assert.equal(resizeFrame(80, 24), '{"type":"resize","cols":80,"rows":24}');
});

test('resizeFrame: is a plain string (a TEXT frame when passed to WebSocket.send)', () => {
	assert.equal(typeof resizeFrame(80, 24), 'string');
});

test('encodeKeystroke: real bug fixed -- returns a Uint8Array (a BINARY frame), never a string', () => {
	// socket.send(str) sends a TEXT frame; the server's control-frame parser
	// would swallow every keystroke silently instead of writing it to the
	// exec's stdin. encodeKeystroke's return type is what keeps send() on
	// the binary path.
	const encoded = encodeKeystroke('a');
	assert.ok(encoded instanceof Uint8Array, 'expected a Uint8Array, got ' + encoded.constructor.name);
	assert.notEqual(typeof encoded, 'string');
});

test('encodeKeystroke: round-trips ASCII keystrokes byte-for-byte', () => {
	assert.deepEqual(Array.from(encodeKeystroke('\r')), [0x0d]);
	assert.deepEqual(Array.from(encodeKeystroke('ls\n')), [0x6c, 0x73, 0x0a]);
});

test('encodeKeystroke: multi-byte UTF-8 input (e.g. pasted non-ASCII text) encodes correctly', () => {
	// xterm.js delivers pasted/composed text through onData too, not just
	// single ASCII keystrokes.
	assert.deepEqual(Array.from(encodeKeystroke('é')), [0xc3, 0xa9]);
});
