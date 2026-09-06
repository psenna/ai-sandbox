// Package config loads and validates the docker-operator's runtime
// configuration from command-line flags and the environment, with
// flag > env > built-in default precedence -- the same idiom as
// operator/internal/config.
//
// One value is REQUIRED and has no default: AGENT_TOKEN, the Bearer every
// agent presents to git-proxy. GITHUB_REPO is optional -- it is the repo a
// create request that names no per-agent repo falls back to, and an agent
// with no repo at all boots as a bare Claude terminal that clones on demand.
// Everything else defaults to something usable on a single local host: the
// agent cap
// (MAX_AGENTS), the HTTP listen address (LISTEN_ADDR), the BoltDB path
// (STATE_DB_PATH), the agent image (AGENT_IMAGE), the shared network names
// (PROXYNET_NAME/DBNET_NAME), the DinD sidecar's runtime (DOCKER_RUNTIME),
// and the shared-service URLs (GIT_PROXY_*, DEPENDAPROXY_*) that
// internal/agent templates into every agent container's environment.
//
// Unlike operator/internal/config, the environment variables here are NOT
// prefixed. Most of them (GITHUB_REPO, AGENT_TOKEN, GIT_PROXY_*,
// DEPENDAPROXY_*) are the exact names the agent container itself consumes
// and that ai-sandbox/.env.example already defines, so prefixing them would
// break the property that one .env file drives both the compose stack and
// the docker-operator.
//
// The centralized file store (issue #122) adds three variables:
// FILESTORE_DIR (the path the operator sees the shared store at) and
// FILESTORE_VOLUME (the Docker volume name the daemon resolves each agent's
// per-agent subpath mount against) are two names for the same storage, and
// FILESTORE_MAX_UPLOAD_BYTES caps a single upload (default 100 MiB). An empty
// FILESTORE_DIR disables the whole feature.
//
// There is deliberately no DockerHost field. The moby SDK's client.FromEnv
// resolves DOCKER_HOST together with DOCKER_TLS_VERIFY, DOCKER_CERT_PATH and
// DOCKER_API_VERSION as one set; re-parsing only DOCKER_HOST here would
// invite a caller to pass it to client.WithHost and silently drop the TLS
// settings. internal/dockerclient (issue #63) calls client.FromEnv directly
// and owns the fail-fast Ping, which is also why this package -- like
// operator/internal/config -- validates the shape of its values and never
// the reachability of anything.
package config
