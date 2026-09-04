// Command docker-operator is the entrypoint for the Docker-backed agent
// operator: a single local daemon that will create, delete and reconcile
// per-agent Docker containers and serve the REST API, WebSocket terminal
// bridge and web UI for them.
//
// This is the scaffold from issue #61: it prints its version and exits.
// Later issues wire in configuration loading, the Docker client, the BoltDB
// store, the reconcile loop and the HTTP server.
package main

import (
	"fmt"
	"os"
)

// version is the build stamp reported on startup. It is a var, not a const,
// so a future release build can override it with
// -ldflags "-X main.version=$(git describe --tags)"; nothing injects it yet.
var version = "dev"

func main() {
	_, _ = fmt.Fprintln(os.Stdout, "docker-operator "+version)
}
