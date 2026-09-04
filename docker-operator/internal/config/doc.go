// Package config loads and validates the docker-operator's runtime
// configuration from command-line flags and the environment, with
// flag > env > built-in default precedence -- the same idiom as
// operator/internal/config.
//
// Two values are REQUIRED and have no default: GITHUB_REPO and AGENT_TOKEN,
// the shared repository and Bearer every agent uses against git-proxy (they
// are also the only two values ai-sandbox/.env.example demands). Everything
// else defaults to something usable on a single local host: the agent cap
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
// There is deliberately no DockerHost field. The moby SDK's client.FromEnv
// resolves DOCKER_HOST together with DOCKER_TLS_VERIFY, DOCKER_CERT_PATH and
// DOCKER_API_VERSION as one set; re-parsing only DOCKER_HOST here would
// invite a caller to pass it to client.WithHost and silently drop the TLS
// settings. internal/dockerclient (issue #63) calls client.FromEnv directly
// and owns the fail-fast Ping, which is also why this package -- like
// operator/internal/config -- validates the shape of its values and never
// the reachability of anything.
package config
