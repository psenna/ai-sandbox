package agent

import (
	"context"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ListAnthropicAccounts, CreateAnthropicAccount, DeleteAnthropicAccount,
// DefaultAnthropicAccountID and SetDefaultAnthropicAccount are thin
// pass-throughs to the store -- none of them touch Docker, the same shape
// AnthropicAuthStatus/SetAnthropicAuth/ClearAnthropicAuth had before this
// feature replaced the single shared credential with named accounts.

func (m *Manager) ListAnthropicAccounts(ctx context.Context) ([]store.AnthropicAccount, error) {
	return m.store.ListAnthropicAccounts(ctx)
}

func (m *Manager) CreateAnthropicAccount(ctx context.Context, name, kind, value string) (store.AnthropicAccount, error) {
	return m.store.CreateAnthropicAccount(ctx, name, kind, value)
}

func (m *Manager) DeleteAnthropicAccount(ctx context.Context, id string) error {
	return m.store.DeleteAnthropicAccount(ctx, id)
}

func (m *Manager) DefaultAnthropicAccountID(ctx context.Context) (string, error) {
	return m.store.DefaultAnthropicAccountID(ctx)
}

func (m *Manager) SetDefaultAnthropicAccount(ctx context.Context, id string) error {
	return m.store.SetDefaultAnthropicAccount(ctx, id)
}
