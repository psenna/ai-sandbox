// terminal.test.js -- unit-tests the pure helpers extracted from terminal.js
// (resizeFrame, encodeKeystroke, shouldForwardWheel, terminalTextToPlain,
// buildContextHTML). The rest of terminal.js (renderAgentDetail, the View
// context overlay) needs a real DOM, xterm.js, and a WebSocket, which this
// project deliberately has no jsdom/headless-browser tooling for (see
// render.test.js's header) -- that half is covered by manual review plus an
// integration test against a real running backend instead.
//
// Run with: node --test web/terminal.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const {
	resizeFrame,
	encodeKeystroke,
	shouldForwardWheel,
	terminalTextToPlain,
	buildContextHTML,
} = require('./terminal.js');

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

test('shouldForwardWheel: swallowed only on the alternate screen with no mouse tracking', () => {
	// The bug: over tmux/claude the wheel becomes history-walking arrow keys.
	assert.equal(shouldForwardWheel('alternate', 'none'), false);
	assert.equal(shouldForwardWheel('alternate', undefined), false);
	// Normal buffer has real scrollback to move -- leave it to xterm.js.
	assert.equal(shouldForwardWheel('normal', 'none'), true);
	// The app is tracking the mouse: forward, so it can scroll itself.
	assert.equal(shouldForwardWheel('alternate', 'vt200'), true);
	assert.equal(shouldForwardWheel('alternate', 'any'), true);
});

test('terminalTextToPlain: strips ANSI colour/cursor sequences and OSC titles', () => {
	const raw = '\x1b]0;claude\x07\x1b[32mhello\x1b[0m \x1b[1mworld\x1b[0m\r\n';
	assert.equal(terminalTextToPlain(raw), 'hello world');
});

test('terminalTextToPlain: resolves carriage-return redraws to the final line content', () => {
	const raw = 'downloading 10%\rdownloading 100%\r\ndone\n';
	assert.equal(terminalTextToPlain(raw), 'downloading 100%\ndone');
});

test('terminalTextToPlain: collapses runs of blank lines and trims edges', () => {
	assert.equal(terminalTextToPlain('\n\n\nalpha\n\n\n\nbeta\n\n\n'), 'alpha\n\nbeta');
});

test('terminalTextToPlain: tolerates empty / nullish input', () => {
	assert.equal(terminalTextToPlain(''), '');
	assert.equal(terminalTextToPlain(null), '');
	assert.equal(terminalTextToPlain(undefined), '');
});

test('buildContextHTML: escapes HTML so a transcript can never inject markup', () => {
	const html = buildContextHTML('<script>alert(1)</script>', '');
	assert.doesNotMatch(html, /<script>/);
	assert.match(html, /&lt;script&gt;/);
});

test('buildContextHTML: wraps every case-insensitive match in <mark>', () => {
	const html = buildContextHTML('Error: an error occurred', 'error');
	assert.equal((html.match(/<mark>/g) || []).length, 2);
	assert.match(html, /<mark>Error<\/mark>/);
	assert.match(html, /<mark>error<\/mark>/);
});

test('buildContextHTML: a query with regex/HTML metacharacters is matched literally', () => {
	const html = buildContextHTML('cost is $5 (approx.)', '$5 (approx.)');
	assert.match(html, /<mark>\$5 \(approx\.\)<\/mark>/);
});

test('buildContextHTML: an empty query highlights nothing', () => {
	assert.doesNotMatch(buildContextHTML('plain text', '   '), /<mark>/);
});
