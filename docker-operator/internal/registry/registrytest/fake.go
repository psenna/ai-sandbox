// Package registrytest provides a test double for registry.Client.
package registrytest

import (
	"context"
	"sync"

	"github.com/psenna/ai-sandbox/docker-operator/internal/registry"
)

// Fake is an in-memory registry.Client. ListTags returns a copy of Tags, or
// Err when it is set (after honouring a cancelled context). Calls counts every
// invocation.
type Fake struct {
	Tags []string
	Err  error

	mu    sync.Mutex
	calls int
}

var _ registry.Client = (*Fake)(nil)

// ListTags implements registry.Client.
func (f *Fake) ListTags(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Err != nil {
		return nil, f.Err
	}
	out := make([]string, len(f.Tags))
	copy(out, f.Tags)
	return out, nil
}

// Calls reports how many times ListTags has been called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
