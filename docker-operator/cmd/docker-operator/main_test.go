package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

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
