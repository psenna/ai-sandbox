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
	isCopyShortcut,
	isPasteShortcut,
	terminalTextToPlain,
	buildContextHTML,
	clampToLastLines,
	replayCaptureToText,
} = require('./terminal.js');

// replayCaptureToText uses window.Terminal when present. The vendored xterm.js
// bundle runs headless under Node with only a `self` global (verified: no DOM
// is touched until .open(), which the replay never calls). Shim it so the
// replay path -- the actual fix for the "overlay is blank" bug -- is covered,
// not just its terminalTextToPlain fallback.
global.self = global.self || global;
global.window = global.window || {};
try {
	global.window.Terminal = require('./vendor/xterm/xterm.js').Terminal;
} catch (e) {
	// bundle not loadable here -- the replay tests below will skip.
}

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

test('isCopyShortcut / isPasteShortcut: Ctrl/⌘+Shift+C/V only', () => {
	assert.equal(isCopyShortcut({ code: 'KeyC', shiftKey: true, ctrlKey: true }), true);
	assert.equal(isCopyShortcut({ code: 'KeyC', shiftKey: true, metaKey: true }), true);
	assert.equal(isPasteShortcut({ code: 'KeyV', shiftKey: true, ctrlKey: true }), true);
	// Bare Ctrl+C must stay SIGINT to the pty, not a copy.
	assert.equal(isCopyShortcut({ code: 'KeyC', ctrlKey: true }), false);
	// Shift+C alone is just a capital C.
	assert.equal(isCopyShortcut({ code: 'KeyC', shiftKey: true }), false);
	// A modifier soup that isn't the combo.
	assert.equal(isCopyShortcut({ code: 'KeyC', shiftKey: true, ctrlKey: true, altKey: true }), false);
	assert.equal(isPasteShortcut({ code: 'KeyC', shiftKey: true, ctrlKey: true }), false);
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

test('clampToLastLines: passes short text through untouched', () => {
	assert.equal(clampToLastLines('a\nb\nc', 10), 'a\nb\nc');
	assert.equal(clampToLastLines('', 10), '');
	assert.equal(clampToLastLines(null, 10), '');
});

test('clampToLastLines: keeps the tail and prefixes a count when it truncates', () => {
	const text = Array.from({ length: 20 }, (_, i) => 'line' + i).join('\n');
	const out = clampToLastLines(text, 5);
	assert.match(out, /^\[… 15 earlier lines hidden …\]\n\n/);
	assert.match(out, /line15\nline16\nline17\nline18\nline19$/);
	assert.doesNotMatch(out, /line14/);
});

test('replayCaptureToText: resolves cursor-addressed words into spaced, single-frame text', async () => {
	if (!global.window.Terminal) return; // xterm bundle unavailable in this env
	// Same frame written twice with cursor-home in between (a TUI redraw), and
	// words placed by column rather than with spaces between them.
	const frame = '\x1b[H\x1b[2J\x1b[1;1HWelcome\x1b[1;12Hto\x1b[1;20HClaude Code\x1b[2;1Hready';
	const out = await replayCaptureToText(frame + frame);
	assert.match(out, /Welcome {2,}to {2,}Claude Code/); // spacing reconstructed from columns
	assert.match(out, /\nready/);
	// The redraw collapsed: "Welcome" appears once, not once per frame.
	assert.equal((out.match(/Welcome/g) || []).length, 1);
});

test('replayCaptureToText: keeps scrolled-off history from the normal buffer', async () => {
	if (!global.window.Terminal) return;
	let raw = '';
	for (let i = 0; i < 200; i++) raw += 'history-line-' + i + '\r\n';
	const out = await replayCaptureToText(raw);
	assert.match(out, /history-line-0\b/);   // scrolled far off the 50-row viewport
	assert.match(out, /history-line-199\b/);
});

test('replayCaptureToText: tolerates empty / nullish input', async () => {
	assert.equal(await replayCaptureToText(''), '');
	assert.equal(await replayCaptureToText(null), '');
});
