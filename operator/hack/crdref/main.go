// Command crdref renders operator/docs/crd-reference.md from the
// controller-gen-generated CRD YAML in config/crd/bases. Run via
// `make crd-docs` from operator/; never built into the shipped image
// (operator/Dockerfile builds only ./cmd and ./cmd/sandboxctl).
package main

import (
	"fmt"
	"os"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/psenna/ai-sandbox/operator/internal/crdref"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: crdref <crd-yaml-path>...")
		os.Exit(2)
	}

	crds := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(os.Args)-1)
	for _, path := range os.Args[1:] {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: paths come from the Makefile's own CRD_FILES list, not untrusted input
		if err != nil {
			fmt.Fprintf(os.Stderr, "crdref: reading %s: %v\n", path, err)
			os.Exit(1)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			fmt.Fprintf(os.Stderr, "crdref: unmarshaling %s: %v\n", path, err)
			os.Exit(1)
		}
		crds = append(crds, crd)
	}

	out, err := crdref.Render(crds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crdref: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stdout.WriteString(out); err != nil {
		fmt.Fprintf(os.Stderr, "crdref: writing stdout: %v\n", err)
		os.Exit(1)
	}
}
