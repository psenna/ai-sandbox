# docker-operator

## Committed-copy gotchas: run `make all` before pushing

Two files/directories in this module are the single source of truth for a
**committed copy** elsewhere, because Go's `go:embed` cannot reach outside its
own package directory (and refuses a symlinked directory). Editing the source
without regenerating the copy compiles and tests fine locally but **fails CI**
(`web-embed-check` / `dind-init-check`), because the copy is what actually
gets embedded into the running binary/image.

| Source (edit this) | Committed copy (embedded) | After editing the source, run |
|---|---|---|
| `web/*` (the whole frontend: `app.js`, `render.js`, `terminal.js`, `auth.js`, `files.js`, `index.html`, `style.css`, …) | `internal/webui/web/*` | `make sync-web-embed` |
| `../scripts/dind-init.sh` | `internal/agent/dind-init.sh` | `cp ../scripts/dind-init.sh internal/agent/dind-init.sh` |

Rules of thumb:

- **Never hand-edit a committed copy directly.** Always edit the source
  (`web/*` or `../scripts/dind-init.sh`) and regenerate/copy from there — a
  copy edited in isolation just drifts again the next time the source changes.
- `web/*.test.js` files are deliberately **excluded** from the embedded copy
  (`sync-web-embed` deletes them after copying) — nothing at runtime serves
  test files over HTTP.
- Before finishing any task that touches `web/` or `scripts/dind-init.sh`,
  run `make all` (or at least the specific check: `make web-embed-check` /
  `make dind-init-check`) from `docker-operator/` — this catches drift
  *before* it reaches CI, not after.

`make all` runs every one of these checks (`vet fmt-check lint test
skill-check dind-init-check web-test web-embed-check`) — running it once
before pushing is cheaper than a red CI run and a follow-up commit.
