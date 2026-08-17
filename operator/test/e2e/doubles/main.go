// Command doubles runs one of two fixture HTTP services used by the
// operator's kind-based e2e suite (operator/test/e2e): a fake git-proxy
// broker (agent-facing REST API: PRs, checks, issues) or a stub model
// endpoint (Anthropic Messages API shape). Both are dependency-free
// (stdlib only, see go.mod) so this module builds fully offline
// (GOPROXY=off) -- no DependaProxy involvement.
//
// Nothing in the operator consumes either service today (see #22's
// non-goals); they exist so the e2e suite has real, controllable HTTP
// endpoints to program and assert against, in front of future issues
// (#30 wait-probes, agent PR/issue workflows) that will.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: doubles <broker|model>")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "broker":
		err = runBroker()
	case "model":
		err = runModel()
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: usage: doubles <broker|model>\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "doubles: %v\n", err)
		os.Exit(1)
	}
}
