---
name: implement-issue
description: Implement one or more GitHub issues end-to-end on a repo served by git-proxy. For each issue, run a PLAN with an Opus subagent to define the implementation, IMPLEMENT with a Sonnet subagent, then VALIDATE (vet/fmt/lint/vuln/test) and FIX major flaws with an Opus subagent; finish with a feat/* branch, PR, CI wait, and optional merge. Use when the user asks to "implement issue #N", "implement the issues", or to drive the plan→implement→validate loop on tracked GitHub work.
---

# implement-issue

Drive a GitHub issue (or a list) from spec to merged PR using a tiered-model
pipeline: **Opus plans, Sonnet implements, Opus validates & fixes.** Each issue
gets its own `feat/*` branch and PR.

## Inputs

- `args` = issue number(s) and options, e.g. `implement-issue 61` or
  `implement-issue 61 62 --repo owner/repo --merge`. Parse:
  - issue numbers: the positional integers.
  - `--repo <owner/repo>` (default: the `GITHUB_REPO` environment variable, if
    set; if both are unset, ask the user which repo).
  - `--merge` (wait for CI green, then squash-merge the PR).
  - `--base <branch>` (PR base; default `main`).
- If no issue numbers are given, ask the user which issues to implement.

## Per-issue pipeline (run sequentially for each issue)

### 0. Fetch the issue
Use the `use-git-proxy` broker: `GET /<owner%2Frepo.git>/issues/<N>` with the
`Authorization: Bearer ${AGENT_TOKEN}` header. Capture `{title, body}`. Note the
repo root on disk (e.g. `/workspace/<repo-name>`). If the repo's own
conventions (language, build tool, test command, branch/PR conventions) are not
already known, read its README / CONTRIBUTING / CLAUDE.md and skim its existing
code structure now — do NOT assume a stack. The only conventions that hold
across every repo served through git-proxy are: `feat/*` branches, and pushes/
PRs go through the `use-git-proxy` broker.

### 1. PLAN — Opus
Launch a **Plan** subagent with **model: opus**:
```
Agent({
  subagent_type: "Plan",
  model: "opus",
  description: "plan issue #N",
  prompt: "<issue title + body verbatim> + the repo's layout and its OWN
           conventions (from its README/CONTRIBUTING/CLAUDE.md and existing
           code -- its language, build tool, module/package layout, and how it
           runs tests; do not assume a stack this repo hasn't shown you).
           Produce a concrete, file-level implementation plan: files to
           create/modify, the approach, the build sequence, and the
           verification steps (the exact command(s) to build/lint/test, and
           any service or env var the tests need)."
})
```
The Plan agent is read-only — it returns the plan text. Read it; if it's missing
load-bearing detail or conflicts with the codebase, run it again with the gap
called out. Do NOT edit during planning.

### 2. IMPLEMENT — Sonnet
Create the branch first (in the repo on disk): `git checkout -b feat/<slug>`
off the configured base. Then launch a **general-purpose** subagent with
**model: sonnet** to implement the plan:
```
Agent({
  subagent_type: "general-purpose",
  model: "sonnet",
  description: "implement issue #N",
  prompt: "<the Opus plan> + 'Implement this on the current feat/<slug> branch
           in <repo dir>. Follow TDD where the plan says so. Run the repo's own
           build/lint/test commands (per the plan's verification steps) --
           inside a DinD container matching the repo's language, per the
           use-docker skill, never directly on this container -- to confirm it
           builds and the new tests pass. Apply the repo's own formatter if it
           has one. Do NOT push or open a PR yet. Report what you changed and
           the test result.'"
})
```
After it returns, verify the working tree has the expected changes and that the
tests it claimed pass actually do (spot-check by re-running the targeted tests).
Restore file ownership if container runs left repo files root-owned:
`docker run --rm -v /workspace:/work alpine chown -R 1000:1000 /work/<repo>`.

### 3. VALIDATE & FIX — Opus
Run the repo's own full validation gate yourself (not in a subagent) so you see
the raw output: whatever combination of vet/format-check/lint/vulnerability-scan/
test the repo defines (its README, Makefile, `package.json` scripts, or CI config
say what these are), starting any service the tests need first (e.g. a database
or object store, via the use-docker skill). Collect any failures.

Then launch an **Opus** subagent to review the diff and fix **major** flaws:
```
Agent({
  subagent_type: "general-purpose",   // or "feature-dev:code-reviewer" to review only
  model: "opus",
  description: "validate+fix issue #N",
  prompt: "<git diff of the branch vs base> + <gate failures, if any> + the
           issue requirements. Fix only MAJOR flaws (correctness, security,
           broken tests, lint/vet/fmt failures, missing acceptance criteria).
           Skip nits. Re-run the gate after fixes. Report what changed and the
           final gate result."
})
```
Iterate (Opus fix → re-run gate) until the gate is green. Apply the same
ownership-restore step if it ran container commands.

### 4. PUSH + PR + (optional) CI + merge
- Commit (signed-off / `Co-Authored-By: Claude` per repo convention) on the
  `feat/*` branch.
- Push via git-proxy: `git -c "http.extraheader=Authorization: Bearer ${AGENT_TOKEN}" push origin feat/<slug>`.
  If the push is rejected by the high-entropy secret scan, fix the offending
  value (use a low-entropy placeholder) and re-push — do NOT try to circumvent it.
- Open the PR via the broker: `POST /<repo>/prs` `{head, base, title, body}`.
  The body MUST include a description of what was done on the PR:
  - `## What was done` — a plain-language summary of the implementation: what
    the change does, how it meets the issue's requirements, the key files
    modified/created, and any notable design choices.
  - `## Verification` — the gate result (vet/fmt/lint/vuln/test) and the
    relevant test output.
  - `Closes #N` to link the issue.
- If `--merge`: poll `GET /<repo>/checks/<head>` until `overall` is `success` or
  `failure` (background poll, ~25s interval). On success, verify `mergeable=true`
  via `GET /<repo>/prs/<N>` then `POST /<repo>/prs/<N>/merge?method=squash`. On
  failure, diagnose (the broker exposes only `name/status/conclusion` — if you
  can't see the failing step from local reproduction, ask the user to check the
  PR checks page) and fix → push → re-poll.
- Reference the issue in the PR body (e.g. "Closes #N") so merging closes it.

## Conventions & guardrails
- Branches: `feat/*` only (git-proxy rejects pushes outside `main`/`feat/*`).
- Whatever language the repo uses, run its build/lint/test toolchain inside a
  DinD container matching that language (see the use-docker skill), never
  directly on this agent's own container.
- No tokens in commits/files (git-proxy `secret_scan` rejects them; reasons are
  redacted and safe to repeat).
- Default path / existing behavior must stay green: run the existing suite, not
  just the new tests.
- One issue = one branch = one PR. For a list of issues, loop the pipeline per
  issue (each its own branch/PR), respecting inter-issue dependencies noted in
  the issue bodies (implement dependents after their dependencies land).

## When to deviate
- If an issue is tiny (one-line fix, typo), skip the Opus plan subagent and
  implement directly, but still run the full gate + an Opus review.
- If the Plan agent's output is clearly wrong vs the code, re-plan rather than
  implement a bad plan.
- Surface environment/credential blockers to the user (e.g. git-proxy write
  denied, missing `projects` label) instead of retrying blindly.