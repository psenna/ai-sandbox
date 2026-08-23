package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

// The one package this double ever serves: enough for #24's `npm install
// @e2e/hello@1.0.0 && npm ci` e2e spec to prove a workload container reaches
// dependaproxy's npm route end to end, with nothing pulled from the real
// npm registry (network-blocked in this sandbox anyway -- see the repo's
// CLAUDE.md).
const (
	npmPackageName    = "@e2e/hello"
	npmPackageVersion = "1.0.0"
	npmTarballName    = "hello-1.0.0.tgz"
	npmPackageJSON    = `{"name":"@e2e/hello","version":"1.0.0","main":"index.js"}`
	npmIndexJS        = "module.exports = 'hello from the e2e npm registry double';\n"
)

// buildNpmTarball builds, in memory, a gzipped tar of the one-file @e2e/hello
// package (package/package.json + package/index.js, the exact layout `npm
// pack`/`npm install` expect to unpack from a tarball dist), and returns the
// tarball bytes plus its subresource-integrity string (npm validates
// dist.integrity against the downloaded tarball before unpacking it).
func buildNpmTarball() ([]byte, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	files := []struct {
		name string
		body string
	}{
		{"package/package.json", npmPackageJSON},
		{"package/index.js", npmIndexJS},
	}
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(fmt.Sprintf("npmregistry: writing tar header for %s: %v", f.name, err))
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			panic(fmt.Sprintf("npmregistry: writing tar body for %s: %v", f.name, err))
		}
	}
	if err := tw.Close(); err != nil {
		panic(fmt.Sprintf("npmregistry: closing tar writer: %v", err))
	}
	if err := gz.Close(); err != nil {
		panic(fmt.Sprintf("npmregistry: closing gzip writer: %v", err))
	}

	tarball := buf.Bytes()
	sum := sha512.Sum512(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	return tarball, integrity
}

// runNpmRegistry starts the minimal npm registry double and blocks until it
// exits. It serves exactly two GETs under /npm/: the packument for
// @e2e/hello (npm percent-encodes the scoped package's "/" as "%2f" when
// requesting metadata; net/http's URL.Path is already the DECODED form, so
// both "/npm/@e2e%2fhello" and "/npm/@e2e/hello" arrive here as the same
// r.URL.Path and need no special-casing) and the package tarball itself.
//
// NPM_PUBLIC_BASE supplies the host npm's SECOND request (the tarball
// download named by dist.tarball in the packument) must resolve -- set by
// test/e2e/manifests/doubles.yaml to the in-cluster platform-doubles Service
// DNS name, since a workload container cannot resolve this process's own
// pod IP or "localhost".
func runNpmRegistry() error {
	tarball, integrity := buildNpmTarball()

	base := strings.TrimSuffix(os.Getenv("NPM_PUBLIC_BASE"), "/")
	if base == "" {
		base = "http://localhost:8083"
	}
	packagePath := "/npm/" + npmPackageName
	tarballPath := packagePath + "/-/" + npmTarballName
	tarballURL := base + tarballPath

	packument := map[string]any{
		"name":      npmPackageName,
		"dist-tags": map[string]string{"latest": npmPackageVersion},
		"versions": map[string]any{
			npmPackageVersion: map[string]any{
				"name":    npmPackageName,
				"version": npmPackageVersion,
				"main":    "index.js",
				"dist": map[string]string{
					"tarball":   tarballURL,
					"integrity": integrity,
				},
			},
		},
	}
	packumentJSON, err := json.Marshal(packument)
	if err != nil {
		return fmt.Errorf("npmregistry: marshaling packument: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /npm/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tarballPath:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tarball)
		case packagePath:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(packumentJSON)
		default:
			http.NotFound(w, r)
		}
	})

	addr := ":8083"
	log.Printf("npmregistry: listening on %s (public base %s)", addr, base)
	return http.ListenAndServe(addr, mux)
}
