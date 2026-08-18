// The e2e suite is a separate Go module, never referenced from
// operator/go.mod, for exactly the reason hack/tools is: Ginkgo's reporters
// import golang.org/x/tools/go/packages -> golang.org/x/mod/semver, and
// DependaProxy's CVE gate denies golang.org/x/mod at EVERY released version.
// Adding ginkgo to operator/go.mod would push that unresolvable requirement
// onto the operator's own build, test, vet, lint, vuln and Docker build
// paths. Here it is contained: only `make e2e` ever resolves this module,
// through the same file-cache GOPROXY chain hack/tools already uses (see
// TOOLS_GOPROXY in operator/Makefile, and operator/README.md).
module github.com/psenna/ai-sandbox/operator/test/e2e

go 1.25.0

require (
	github.com/onsi/ginkgo/v2 v2.31.0
	github.com/onsi/gomega v1.41.0
	github.com/psenna/ai-sandbox/operator v0.0.0
	k8s.io/api v0.35.0
	k8s.io/apimachinery v0.35.0
	k8s.io/client-go v0.35.0
	sigs.k8s.io/controller-runtime v0.23.3
	sigs.k8s.io/yaml v1.6.0
)

require (
	github.com/Masterminds/semver/v3 v3.4.0 // indirect
	github.com/aws/aws-sdk-go-v2 v1.43.2 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.15 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.32 // indirect
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.22.37 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.33 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.33 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.34 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.26 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.33 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.34 // indirect
	github.com/aws/aws-sdk-go-v2/service/s3 v1.106.2 // indirect
	github.com/aws/smithy-go v1.27.5 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/emicklei/go-restful/v3 v3.12.2 // indirect
	github.com/evanphx/json-patch/v5 v5.9.11 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/jsonreference v0.20.2 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/google/gnostic-models v0.7.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20260402051712-545e8a4df936 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/mxk/go-flowrate v0.0.0-20140419014527-cca7078d478f // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/oauth2 v0.30.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/term v0.44.0 // indirect
	golang.org/x/time v0.9.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.13.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/klog/v2 v2.130.1 // indirect
	k8s.io/kube-openapi v0.0.0-20250910181357-589584f1c912 // indirect
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.3.2-0.20260122202528-d9cc6641c482 // indirect
)

// Explicit indirect pins matching operator/go.mod's own confirmed-passing
// versions (x/net, x/sys, x/text) and hack/tools/go.mod's x/mod / x/tools
// pins -- golang.org/x/mod is denied by DependaProxy's CVE gate at every
// released version, so it can only resolve from the local file-cache GOPROXY
// leg; the others are pinned to the same lowest-known-good versions the rest
// of this repo already uses, to avoid MVS pulling in a fresh version that
// hasn't been validated against DependaProxy.
require (
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
)

// github.com/moby/spdystream (pulled in transitively via
// k8s.io/client-go/tools/remotecommand, used by Harness.Exec) at v0.5.0 is
// denied by DependaProxy's CVE gate (GHSA-pc3f-x583-g7j2 / GO-2026-4958);
// v0.5.1 clears it.
require github.com/moby/spdystream v0.5.1 // indirect

replace github.com/psenna/ai-sandbox/operator => ../..
