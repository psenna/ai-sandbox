---
name: store-file
description: Use when asked to save/store a file somewhere permanent, persist an artifact, put something in the store, recover/load a previously stored file, or pick up a shared input the operator left for you — anything that must outlive this agent or is shared across agents. /workspace/store ($AGENT_STORE_DIR) is a private central file store on a shared Docker volume that survives this agent being deleted; /workspace/shared ($AGENT_SHARED_DIR) is a read-only common area the human operator fills. Both are browsable through the operator web UI. Covers the cp commands to save and recover, the shared area, what does and does not persist, and when NOT to use it.
---

# Store a file

`/workspace/store` (also `$AGENT_STORE_DIR`) is a **centralized file store**: a
per-agent directory (`agents/<your-id>/`) inside the shared
`docker-operator-filestore` Docker volume, mounted into your container by the
docker-operator. Per-agent isolation is enforced by Docker — you only ever see
your own subtree.

`/workspace/shared` (also `$AGENT_SHARED_DIR`) is a **read-only common area** in
the same volume, mounted into **every** agent. Only the human operator writes to
it (through the web UI); you can read shared inputs from it but cannot modify it.
To share a file with other agents, ask the human to put it there (or to move it
from your `/workspace/store`).

## What persists, and what does not

- **`/workspace` itself is DESTROYED when this agent is deleted.** Anything you
  leave in `/workspace` (your clone, build output, notes) is gone.
- **`/workspace/store` is NOT destroyed.** Files here survive this agent being
  deleted, the operator restarting, and a browser disconnecting. They are
  removed only if the human explicitly purges them (delete with
  `?purge_files=true`, or the operator web UI's file browser).
- The human operator can **download and upload** the same files from the
  operator web UI ("Files" in the sidebar).

## Save a file (into the store)

```sh
mkdir -p "$AGENT_STORE_DIR"
cp -a report.md "$AGENT_STORE_DIR/"              # one file
cp -a build/artifacts "$AGENT_STORE_DIR/"        # a whole directory
```

## Recover a file (out of the store)

```sh
ls -la "$AGENT_STORE_DIR"                         # see what's there
cp -a "$AGENT_STORE_DIR/report.md" ./             # bring one back
cp -a "$AGENT_STORE_DIR/artifacts" ./build/       # bring a directory back
```

## Read a shared input (from the common area)

`/workspace/shared` (`$AGENT_SHARED_DIR`) is **read-only** — the human operator
puts files there for every agent to use.

```sh
ls -la "$AGENT_SHARED_DIR"                        # what the operator shared
cp -a "$AGENT_SHARED_DIR/dataset.csv" ./          # copy it into your workspace
# Writing straight into it fails (read-only mount). To share something of your
# own, put it in $AGENT_STORE_DIR and ask the human to move it to shared/.
```

## When the store is not available

The operator can run with the file store **disabled**, in which case
`$AGENT_STORE_DIR` is unset and `/workspace/store` does not exist. Check first:

```sh
if [ -n "$AGENT_STORE_DIR" ]; then
  cp -a result.tar.gz "$AGENT_STORE_DIR/"
else
  echo "no central file store on this operator; ask the human where to put it"
fi
```

(`$AGENT_SHARED_DIR` is unset in the same situation — the whole file store is
off.)

## When NOT to use it

- **Not a scratch directory.** Use `/workspace` for working files; only copy
  the finished artifact into the store.
- **Not for secrets.** The human can browse and download everything here, and
  `/workspace/shared` is visible to every other agent.
- Not a substitute for `git` — push code through git-proxy as usual; the store
  is for artifacts that do not belong in a repo.
