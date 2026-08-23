// Command doubles runs one of four fixture HTTP services used by the
// operator's kind-based e2e suite (operator/test/e2e): a fake git-proxy
// broker (agent-facing REST API: PRs, checks, issues), a stub model
// endpoint (Anthropic Messages API shape), a programmable reverse proxy in
// front of the e2e MinIO (#28's fault-injection double, s3proxy.go), or a
// minimal npm registry (#24's npmregistry.go, serving one canned package).
// All four are dependency-free (stdlib only, see go.mod) so this module
// builds fully offline (GOPROXY=off) -- no DependaProxy involvement.
//
// The broker and model doubles are not consumed by the operator today (see
// #22's non-goals); they exist so the e2e suite has real, controllable HTTP
// endpoints to program and assert against, in front of future issues
// (#30 wait-probes, agent PR/issue workflows) that will. s3proxy IS
// consumed today: freeze_test.go points a SandboxClass's storage.backend.s3
// endpoint at it to inject a mid-upload backend failure. npmregistry IS
// consumed today: engine_test.go's `installs an npm package through
// dependaproxy from inside a workload container` spec points a class's
// services.dependaProxy.npmURL at it.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: doubles <broker|model|s3proxy|npmregistry>")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "broker":
		err = runBroker()
	case "model":
		err = runModel()
	case "s3proxy":
		err = runS3Proxy()
	case "npmregistry":
		err = runNpmRegistry()
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: usage: doubles <broker|model|s3proxy|npmregistry>\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "doubles: %v\n", err)
		os.Exit(1)
	}
}
