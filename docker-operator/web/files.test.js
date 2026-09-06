// files.test.js -- issue #122's file-browser rendering tests. Exercises
// render.js's new pure functions (formatBytes / formatModTime /
// renderBreadcrumb / renderFileTable / renderFileBrowser) with Node's
// built-in test runner. No jsdom, no build step -- same shape as
// render.test.js. files.js itself is DOM wiring, reviewed by hand.
//
// Run with: node --test web/files.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const Render = require('./render.js');

test('formatBytes: units and edge cases', () => {
	assert.equal(Render.formatBytes(0), '0 B');
	assert.equal(Render.formatBytes(1), '1 B');
	assert.equal(Render.formatBytes(1023), '1023 B');
	assert.equal(Render.formatBytes(1024), '1.0 KiB');
	assert.equal(Render.formatBytes(1536), '1.5 KiB');
	assert.equal(Render.formatBytes(1 << 20), '1.0 MiB');
	assert.equal(Render.formatBytes(100 << 20), '100.0 MiB');
	assert.equal(Render.formatBytes(-1), '—');
	assert.equal(Render.formatBytes(NaN), '—');
	assert.equal(Render.formatBytes(undefined), '—');
});

test('formatModTime: valid ISO and garbage', () => {
	assert.equal(Render.formatModTime('2026-09-05T12:34:56Z'), '2026-09-05 12:34');
	assert.equal(Render.formatModTime(''), '—');
	assert.equal(Render.formatModTime('not a date'), '—');
});

test('renderBreadcrumb: root has one current crumb, no buttons', () => {
	const html = Render.renderBreadcrumb('');
	assert.match(html, /crumb--current">Files</);
	assert.doesNotMatch(html, /<button/);
});

test('renderBreadcrumb: nested path accumulates data-path', () => {
	const html = Render.renderBreadcrumb('agents/agt_1/sub');
	const buttons = html.match(/<button /g) || [];
	assert.equal(buttons.length, 3);
	assert.equal((html.match(/crumb--current/g) || []).length, 1);
	assert.match(html, /data-path="agents"/);
	assert.match(html, /data-path="agents\/agt_1"/);
});

test('renderBreadcrumb: escapes HTML in segments', () => {
	const html = Render.renderBreadcrumb('a/<script>alert(1)</script>');
	assert.doesNotMatch(html, /<script>/);
	assert.match(html, /&lt;script&gt;/);
});

test('renderFileTable: empty state', () => {
	assert.match(Render.renderFileTable([]), /file-table__empty/);
});

test('renderFileTable: dir row has no download, em-dash size', () => {
	const html = Render.renderFileTable([{ name: 'sub', path: 'a/sub', is_dir: true, size: 0, mod_time: '' }]);
	assert.match(html, /data-is-dir="true"/);
	assert.doesNotMatch(html, /file-table__download/);
	assert.match(html, /<td class="file-table__size">—<\/td>/);
	assert.match(html, /data-path="a\/sub"/);
});

test('renderFileTable: file row has download + formatted size', () => {
	const html = Render.renderFileTable([{ name: 'a.txt', path: 'a/a.txt', is_dir: false, size: 2048, mod_time: '2026-09-05T00:00:00Z' }]);
	assert.match(html, /file-table__download/);
	assert.match(html, /2\.0 KiB/);
	assert.match(html, /data-is-dir="false"/);
	assert.match(html, /data-path="a\/a.txt"/);
});

test('renderFileTable: escapes hostile names, no raw markup', () => {
	const html = Render.renderFileTable([{ name: '"><img src=x onerror=1>', path: '"><img src=x>', is_dir: false, size: 1, mod_time: '' }]);
	assert.doesNotMatch(html, /<img/);
});

test('renderFileBrowser: has breadcrumb, dropzone, toolbar controls, multiple input', () => {
	const html = Render.renderFileBrowser('agents', []);
	assert.match(html, /file-browser__breadcrumb/);
	assert.match(html, /file-browser__dropzone/);
	assert.match(html, /file-browser__new-folder/);
	assert.match(html, /file-browser__upload/);
	assert.match(html, /class="file-browser__file-input" multiple/);
});
