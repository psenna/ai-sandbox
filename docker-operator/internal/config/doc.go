// Package config loads and validates the docker-operator's runtime
// configuration from command-line flags and the environment: MAX_AGENTS,
// DOCKER_HOST, the shared-service URLs (ollama, git-proxy, postgres,
// dependaproxy) and the HTTP listen address.
//
// Scaffold only (issue #61); the loader, its defaults and its validation
// rules land in task 2.
package config
