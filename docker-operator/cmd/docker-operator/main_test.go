package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
)

const (
	testAgentNet    = "docker-operator-proxynet"
	testOperatorNet = "docker-operator-operatornet"
)

// startContainer creates + starts a fake container named name, attached to
// nets, and returns the fake. ContainerStart only records a network the fake
// already knows, so every net is created first.
func startContainer(t *testing.T, name string, nets ...string) *dockerclienttest.Fake {
	t.Helper()
	f := dockerclienttest.New()
	ctx := context.Background()
	attach := make([]dockerclient.NetworkAttachment, 0, len(nets))
	for _, n := range nets {
		if _, err := f.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: n}); err != nil {
			t.Fatalf("NetworkCreate(%q): %v", n, err)
		}
		attach = append(attach, dockerclient.NetworkAttachment{Name: n})
	}
	id, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: name, Networks: attach})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := f.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return f
}

func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestWarnIfReachableFromAgents(t *testing.T) {
	ctx := context.Background()

	t.Run("on the agent network: warns", func(t *testing.T) {
		f := startContainer(t, "docker-operator", testOperatorNet, testAgentNet)
		log, buf := capturingLogger()
		warnIfReachableFromAgents(ctx, f, "docker-operator", testAgentNet, log)
		if !strings.Contains(buf.String(), "SECURITY") {
			t.Errorf("no SECURITY warning logged; got: %s", buf.String())
		}
	})

	t.Run("only on its own network: silent", func(t *testing.T) {
		f := startContainer(t, "docker-operator", testOperatorNet)
		log, buf := capturingLogger()
		warnIfReachableFromAgents(ctx, f, "docker-operator", testAgentNet, log)
		if strings.Contains(buf.String(), "SECURITY") {
			t.Errorf("warned about an isolated operator; got: %s", buf.String())
		}
	})

	t.Run("no hostname: skipped, no panic", func(t *testing.T) {
		f := startContainer(t, "docker-operator", testAgentNet)
		log, buf := capturingLogger()
		warnIfReachableFromAgents(ctx, f, "", testAgentNet, log)
		if strings.Contains(buf.String(), "SECURITY") {
			t.Errorf("warned with no hostname to check; got: %s", buf.String())
		}
	})

	t.Run("own container not found: skipped, no warning", func(t *testing.T) {
		f := startContainer(t, "some-other-container", testAgentNet)
		log, buf := capturingLogger()
		warnIfReachableFromAgents(ctx, f, "docker-operator", testAgentNet, log)
		if strings.Contains(buf.String(), "SECURITY") {
			t.Errorf("warned despite not finding its own container; got: %s", buf.String())
		}
	})
}

// --- agent-image tag refresher -------------------------------------------

type countingRefresher struct {
	mu     sync.Mutex
	calls  int
	err    error
	notify chan struct{}
}

func (c *countingRefresher) RefreshAgentImageTags(ctx context.Context) error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return c.err
}

func (c *countingRefresher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestClampDuration(t *testing.T) {
	min := time.Minute
	if got := clampDuration(time.Second, min); got != min {
		t.Errorf("clampDuration(1s, 1m) = %s, want 1m", got)
	}
	if got := clampDuration(time.Hour, min); got != time.Hour {
		t.Errorf("clampDuration(1h, 1m) = %s, want 1h", got)
	}
	if got := clampDuration(min, min); got != min {
		t.Errorf("clampDuration(1m, 1m) = %s, want 1m", got)
	}
}

func TestStartAgentImageRefresher_PollsOnceThenTicks(t *testing.T) {
	log, _ := capturingLogger()
	r := &countingRefresher{notify: make(chan struct{}, 8)}

	stop := startAgentImageRefresher(r, 20*time.Millisecond, log)

	// The immediate poll plus at least one tick.
	for i := 0; i < 2; i++ {
		select {
		case <-r.notify:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for poll %d; count=%d", i+1, r.count())
		}
	}
	stop()

	stable := r.count()
	if stable < 2 {
		t.Fatalf("refresher ran %d times, want >= 2 (one immediate + at least one tick)", stable)
	}
	// No further polls after stop.
	time.Sleep(60 * time.Millisecond)
	if got := r.count(); got != stable {
		t.Errorf("refresher ran %d more times after stop", got-stable)
	}
}

func TestStartAgentImageRefresher_StopIsClean(t *testing.T) {
	log, _ := capturingLogger()
	r := &countingRefresher{notify: make(chan struct{}, 1), err: errors.New("boom")}

	stop := startAgentImageRefresher(r, time.Hour, log)
	// Returns promptly even though the interval is an hour: the one immediate
	// poll has run (and its error was swallowed), and stop just cancels.
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return promptly")
	}
	if r.count() < 1 {
		t.Errorf("refresher ran %d times, want >= 1 (the immediate poll)", r.count())
	}
}
