package agent

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
)

func countOp(f *dockerclienttest.Fake, op dockerclienttest.Op) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Op == op {
			n++
		}
	}
	return n
}

func loginContainer(f *dockerclienttest.Fake) (dockerclient.Container, bool) {
	for _, c := range f.Containers() {
		if c.Name == AnthropicLoginContainerName {
			return c, true
		}
	}
	return dockerclient.Container{}, false
}

func TestStartAnthropicLogin(t *testing.T) {
	ctx := context.Background()
	m, f, _ := newTestManager(t, 5)

	if err := m.StartAnthropicLogin(ctx); err != nil {
		t.Fatalf("StartAnthropicLogin: %v", err)
	}

	c, ok := loginContainer(f)
	if !ok {
		t.Fatal("no login container was created")
	}
	if c.Labels[LabelManaged] != LabelManagedValue || c.Labels[LabelRole] != string(RoleAnthropicLogin) {
		t.Errorf("login container labels = %v, want managed=true role=anthropic-login", c.Labels)
	}
	if c.Labels[LabelAgentID] != "" {
		t.Errorf("login container has an agent-id label %q, want none (it belongs to no agent)", c.Labels[LabelAgentID])
	}
	if c.Labels[labelLoginStartedAt] == "" {
		t.Errorf("login container has no %s label", labelLoginStartedAt)
	}

	active, err := m.AnthropicLoginActive(ctx)
	if err != nil || !active {
		t.Fatalf("AnthropicLoginActive = (%v, %v), want (true, nil)", active, err)
	}

	// Idempotent: a second Start creates nothing new.
	createsBefore := countOp(f, dockerclienttest.OpContainerCreate)
	if err := m.StartAnthropicLogin(ctx); err != nil {
		t.Fatalf("second StartAnthropicLogin: %v", err)
	}
	if got := countOp(f, dockerclienttest.OpContainerCreate); got != createsBefore {
		t.Errorf("second Start made %d new ContainerCreate calls, want 0", got-createsBefore)
	}
}

func TestStopAnthropicLogin_Idempotent(t *testing.T) {
	ctx := context.Background()
	m, f, _ := newTestManager(t, 5)

	// Stop before start: no error.
	if err := m.StopAnthropicLogin(ctx); err != nil {
		t.Fatalf("StopAnthropicLogin with nothing running: %v", err)
	}

	if err := m.StartAnthropicLogin(ctx); err != nil {
		t.Fatalf("StartAnthropicLogin: %v", err)
	}
	if err := m.StopAnthropicLogin(ctx); err != nil {
		t.Fatalf("StopAnthropicLogin: %v", err)
	}
	if _, ok := loginContainer(f); ok {
		t.Error("login container still present after StopAnthropicLogin")
	}
	if active, _ := m.AnthropicLoginActive(ctx); active {
		t.Error("AnthropicLoginActive = true after Stop")
	}
	// Second Stop: still fine.
	if err := m.StopAnthropicLogin(ctx); err != nil {
		t.Fatalf("second StopAnthropicLogin: %v", err)
	}
}

func TestReapStaleAnthropicLogin(t *testing.T) {
	ctx := context.Background()

	t.Run("no container: no-op", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		if err := m.ReapStaleAnthropicLogin(ctx); err != nil {
			t.Fatalf("ReapStaleAnthropicLogin: %v", err)
		}
	})

	t.Run("fresh container is left alone", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		if err := m.StartAnthropicLogin(ctx); err != nil {
			t.Fatalf("StartAnthropicLogin: %v", err)
		}
		if err := m.ReapStaleAnthropicLogin(ctx); err != nil {
			t.Fatalf("ReapStaleAnthropicLogin: %v", err)
		}
		if _, ok := loginContainer(f); !ok {
			t.Error("a fresh login container was reaped")
		}
	})

	t.Run("an aged container is torn down", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		old := time.Now().Add(-AnthropicLoginIdleTimeout - time.Minute).Unix()
		if _, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{
			Name:   AnthropicLoginContainerName,
			Image:  "test-agent-image",
			Labels: map[string]string{labelLoginStartedAt: strconv.FormatInt(old, 10)},
		}); err != nil {
			t.Fatalf("seeding an old login container: %v", err)
		}
		if err := m.ReapStaleAnthropicLogin(ctx); err != nil {
			t.Fatalf("ReapStaleAnthropicLogin: %v", err)
		}
		if _, ok := loginContainer(f); ok {
			t.Error("an aged login container survived the reap")
		}
	})

	t.Run("a container with no start-time label is treated as stale", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		if _, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{
			Name:  AnthropicLoginContainerName,
			Image: "test-agent-image",
		}); err != nil {
			t.Fatalf("seeding a label-less login container: %v", err)
		}
		if err := m.ReapStaleAnthropicLogin(ctx); err != nil {
			t.Fatalf("ReapStaleAnthropicLogin: %v", err)
		}
		if _, ok := loginContainer(f); ok {
			t.Error("a label-less login container survived the reap")
		}
	})
}
