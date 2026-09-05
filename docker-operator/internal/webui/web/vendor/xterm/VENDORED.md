# Vendored xterm.js

This project has no frontend build pipeline, so the terminal library is
vendored directly rather than referenced from a CDN or installed via
`node_modules` at runtime.

Fetched through DependaProxy's `/npm` endpoint (never the public npm
registry directly), from the exact package files each package ships
prebuilt — no bundling or minification was performed here beyond what the
upstream package already includes.

| File | Package | Version | Source path |
|---|---|---|---|
| `xterm.js` | [`@xterm/xterm`](https://www.npmjs.com/package/@xterm/xterm) | 5.5.0 | `lib/xterm.js` |
| `xterm.css` | `@xterm/xterm` | 5.5.0 | `css/xterm.css` |
| `addon-fit.js` | [`@xterm/addon-fit`](https://www.npmjs.com/package/@xterm/addon-fit) | 0.10.0 | `lib/addon-fit.js` |

Both are UMD bundles (`window.Terminal`, `window.FitAddon`) loadable via a
plain `<script>` tag -- no module loader needed.

To refresh: `npm install @xterm/xterm@<version> @xterm/addon-fit@<version>`
(routed through DependaProxy, per `use-dependaproxy/SKILL.md`) in a scratch
directory, then copy the files above from `node_modules/@xterm/*` back into
this directory. Source maps (`.js.map`) are intentionally not vendored.
