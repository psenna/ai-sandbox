// The platform-service doubles (fake git-proxy broker + stub model
// endpoint) are a separate Go module with ZERO third-party requires, so
// this module builds with GOPROXY=off -- no DependaProxy involvement at
// all. It exists purely as an e2e test fixture: a tiny in-memory HTTP
// server the e2e suite programs via its /_control/* routes, never a
// product artifact.
module github.com/psenna/ai-sandbox/operator/test/e2e/doubles

go 1.25.0
