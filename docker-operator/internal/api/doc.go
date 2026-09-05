// Package api serves the docker-operator's REST surface: the agent
// collection endpoint (GET/POST /api/agents), the agent item endpoints
// (GET/PATCH/DELETE /api/agents/{id}), the output endpoint
// (GET /api/agents/{id}/output), and the shared JSON error envelope every
// handler returns on failure.
//
// Handlers depend on the AgentManager interface, not *agent.Manager
// directly, so they can be tested against a fake -- *agent.Manager satisfies
// it via its own Create/Delete plus the thin Get/List/MaxAgents/Rename
// wrappers it adds over internal/store. The output endpoint additionally
// takes a dockerclient.ExecClient, the same narrow interface
// internal/wsbridge.ReadOutput itself requires.
package api
