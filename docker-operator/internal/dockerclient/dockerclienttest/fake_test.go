package dockerclienttest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

func TestFailSticky(t *testing.T) {
	f := New()
	ctx := context.Background()
	boom := errors.New("boom")

	if err := f.Ping(ctx); err != nil {
		t.Fatalf("Ping (before Fail) = %v, want nil", err)
	}

	f.Fail(OpPing, boom)
	if err := f.Ping(ctx); !errors.Is(err, boom) {
		t.Errorf("Ping (after Fail) = %v, want %v", err, boom)
	}
	if err := f.Ping(ctx); !errors.Is(err, boom) {
		t.Errorf("Ping (still failing) = %v, want %v", err, boom)
	}

	f.Fail(OpPing, nil)
	if err := f.Ping(ctx); err != nil {
		t.Errorf("Ping (after clearing Fail) = %v, want nil", err)
	}
}

func TestFailOnceOrderingAndPriority(t *testing.T) {
	f := New()
	ctx := context.Background()
	first := errors.New("first")
	second := errors.New("second")
	sticky := errors.New("sticky")

	f.Fail(OpPing, sticky)
	f.FailOnce(OpPing, first)
	f.FailOnce(OpPing, second)

	if err := f.Ping(ctx); !errors.Is(err, first) {
		t.Errorf("Ping #1 = %v, want %v (FailOnce takes priority over Fail)", err, first)
	}
	if err := f.Ping(ctx); !errors.Is(err, second) {
		t.Errorf("Ping #2 = %v, want %v (FailOnce queue is FIFO)", err, second)
	}
	if err := f.Ping(ctx); !errors.Is(err, sticky) {
		t.Errorf("Ping #3 = %v, want %v (queue drained, sticky Fail resumes)", err, sticky)
	}
}

func TestCallsOrdering(t *testing.T) {
	f := New()
	ctx := context.Background()

	_ = f.Ping(ctx)
	_, _ = f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: "v1"})
	_, _ = f.VolumeInspect(ctx, "v1")
	_ = f.VolumeRemove(ctx, "v1")

	calls := f.Calls()
	want := []Call{
		{Op: OpPing, Target: ""},
		{Op: OpVolumeCreate, Target: "v1"},
		{Op: OpVolumeInspect, Target: "v1"},
		{Op: OpVolumeRemove, Target: "v1"},
	}
	if len(calls) != len(want) {
		t.Fatalf("Calls() = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("Calls()[%d] = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestEmitFilterMatchAndDrop(t *testing.T) {
	f := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, errs := f.Events(ctx, dockerclient.EventFilter{
		Types:  []dockerclient.EventType{dockerclient.EventTypeContainer},
		Labels: map[string]string{"want": "yes"},
	})

	// Wait for the subscriber goroutine to register before emitting, so
	// neither Emit call races the subscription.
	waitForSubscriber(t, f)

	// Does not match: wrong type.
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeNetwork, Attributes: map[string]string{"want": "yes"}})
	// Does not match: label mismatch.
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeContainer, Attributes: map[string]string{"want": "no"}})
	// Matches.
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeContainer, ActorID: "abc", Attributes: map[string]string{"want": "yes"}})

	select {
	case ev := <-events:
		if ev.ActorID != "abc" {
			t.Errorf("received event ActorID = %q, want %q (dropped events must not arrive)", ev.ActorID, "abc")
		}
	case err := <-errs:
		t.Fatalf("unexpected error on errs channel: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the matching event")
	}

	select {
	case ev, ok := <-events:
		if ok {
			t.Errorf("received a second event %+v, want only the one matching event", ev)
		}
	case <-time.After(100 * time.Millisecond):
		// No second event arrived promptly, as expected.
	}
}

func TestEventsClosesOnContextCancel(t *testing.T) {
	f := New()
	ctx, cancel := context.WithCancel(context.Background())

	events, errs := f.Events(ctx, dockerclient.EventFilter{})
	waitForSubscriber(t, f)

	cancel()

	select {
	case err, ok := <-errs:
		if !ok {
			t.Fatal("errs closed with no value, want ctx.Err()")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errs value = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for errs")
	}

	select {
	case _, ok := <-errs:
		if ok {
			t.Error("errs yielded a second value, want it closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for errs to close")
	}

	select {
	case _, ok := <-events:
		if ok {
			t.Error("events yielded a value, want it closed with no events")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for events to close")
	}
}

func TestExecInputCapturesMultipleWrites(t *testing.T) {
	f := New()
	ctx := context.Background()

	if _, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "c1", Image: "alpine:latest"}); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	execID, err := f.ExecCreate(ctx, "c1", dockerclient.ExecSpec{Cmd: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}
	stream, err := f.ExecAttach(ctx, execID)
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write #1: %v", err)
	}
	if _, err := stream.Write([]byte("world")); err != nil {
		t.Fatalf("Write #2: %v", err)
	}

	got := f.ExecInput(execID)
	if string(got) != "hello world" {
		t.Errorf("ExecInput = %q, want %q", got, "hello world")
	}
}

func TestExecOutputAndExitSeeding(t *testing.T) {
	f := New()
	ctx := context.Background()
	f.ExecOutput["echo hi"] = []byte("hi\n")
	f.ExecExit["echo hi"] = 3

	if _, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "c1", Image: "alpine:latest"}); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	execID, err := f.ExecCreate(ctx, "c1", dockerclient.ExecSpec{Cmd: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	status, err := f.ExecInspect(ctx, execID)
	if err != nil {
		t.Fatalf("ExecInspect (before attach/read): %v", err)
	}
	if !status.Running {
		t.Errorf("Running = false before output is drained, want true")
	}

	stream, err := f.ExecAttach(ctx, execID)
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	buf := make([]byte, 64)
	n, _ := readAll(t, stream, buf)
	if string(buf[:n]) != "hi\n" {
		t.Errorf("drained output = %q, want %q", buf[:n], "hi\n")
	}

	status, err = f.ExecInspect(ctx, execID)
	if err != nil {
		t.Fatalf("ExecInspect (after drain): %v", err)
	}
	if status.Running {
		t.Errorf("Running = true after EOF, want false")
	}
	if status.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", status.ExitCode)
	}
}

func TestImageListScopedByRepo(t *testing.T) {
	f := New()
	ctx := context.Background()

	f.AddImage("myapp:v1")
	f.AddImage("myapp:v2")
	f.AddImage("myapp-other:v1") // same-named prefix, different repo

	list, err := f.ImageList(ctx, "myapp")
	if err != nil {
		t.Fatalf("ImageList(myapp): %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ImageList(myapp) = %v, want exactly 2 entries", list)
	}
	if list[0].RepoTags[0] != "myapp:v1" || list[1].RepoTags[0] != "myapp:v2" {
		t.Errorf("ImageList(myapp) = %v, want [myapp:v1 myapp:v2] (sorted)", list)
	}
	for _, img := range list {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, "myapp-other") {
				t.Errorf("ImageList(myapp) leaked %q from a different repository", tag)
			}
		}
	}
}

func TestImageListUnknownRepoIsEmpty(t *testing.T) {
	f := New()
	ctx := context.Background()
	f.AddImage("myapp:v1")

	list, err := f.ImageList(ctx, "unknown")
	if err != nil {
		t.Fatalf("ImageList(unknown) = err %v, want nil", err)
	}
	if len(list) != 0 {
		t.Errorf("ImageList(unknown) = %v, want empty", list)
	}
}

func TestImageListFailInjection(t *testing.T) {
	f := New()
	ctx := context.Background()
	boom := errors.New("boom")

	f.Fail(OpImageList, boom)
	if _, err := f.ImageList(ctx, "myapp"); !errors.Is(err, boom) {
		t.Errorf("ImageList (after Fail) = %v, want %v", err, boom)
	}
}

// TestContainerStatsLifecycle pins the fake's model of ContainerStats: the
// same lifecycle shape the real client's error conversion gives it (missing
// -> IsNotFound, not running -> error, running -> a sample), plus the shared
// call/Fail/Calls plumbing every Fake method goes through.
func TestContainerStatsLifecycle(t *testing.T) {
	f := New()
	ctx := context.Background()

	runningID, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "running", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("ContainerCreate(running): %v", err)
	}
	if err := f.ContainerStart(ctx, runningID); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	stoppedID, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "stopped", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("ContainerCreate(stopped): %v", err)
	}

	t.Run("unseeded running container returns the canned sample", func(t *testing.T) {
		got, err := f.ContainerStats(ctx, runningID)
		if err != nil {
			t.Fatalf("ContainerStats: %v", err)
		}
		if got.Read.IsZero() {
			t.Errorf("Read = zero, want a real sample time")
		}
		if got.CPUPercent != 12.5 || got.MemoryUsed != 340<<20 || got.MemoryLimit != 2<<30 || got.Pids != 3 {
			t.Errorf("canned sample = %+v, want the documented plausible values", got)
		}
	})

	t.Run("seeded stats are returned verbatim", func(t *testing.T) {
		seed := dockerclient.Stats{
			Read:        time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
			CPUPercent:  42.25,
			MemoryUsed:  7 << 20,
			MemoryLimit: 1 << 30,
			Pids:        11,
		}
		if err := f.SetStats(runningID, seed); err != nil {
			t.Fatalf("SetStats: %v", err)
		}
		got, err := f.ContainerStats(ctx, runningID)
		if err != nil {
			t.Fatalf("ContainerStats: %v", err)
		}
		if got != seed {
			t.Errorf("ContainerStats = %+v, want the seeded %+v verbatim", got, seed)
		}
	})

	t.Run("stopped and created containers are an error, not zeros", func(t *testing.T) {
		if _, err := f.ContainerStats(ctx, stoppedID); err == nil {
			t.Errorf("ContainerStats(created-never-started) = nil error, want a not-running error")
		}
		if err := f.ContainerStop(ctx, stoppedID, 0); err != nil {
			t.Fatalf("ContainerStop: %v", err)
		}
		if _, err := f.ContainerStats(ctx, stoppedID); err == nil {
			t.Errorf("ContainerStats(stopped) = nil error, want a not-running error")
		}
	})

	t.Run("missing container satisfies IsNotFound", func(t *testing.T) {
		if _, err := f.ContainerStats(ctx, "nope"); !dockerclient.IsNotFound(err) {
			t.Errorf("ContainerStats(missing) = %v, want IsNotFound", err)
		}
	})

	t.Run("Fail and FailOnce take effect through the shared call path", func(t *testing.T) {
		boom := errors.New("boom")
		f.FailOnce(OpContainerStats, boom)
		if _, err := f.ContainerStats(ctx, runningID); !errors.Is(err, boom) {
			t.Errorf("ContainerStats (FailOnce) = %v, want %v", err, boom)
		}
		f.Fail(OpContainerStats, boom)
		if _, err := f.ContainerStats(ctx, runningID); !errors.Is(err, boom) {
			t.Errorf("ContainerStats (Fail) = %v, want %v", err, boom)
		}
		f.Fail(OpContainerStats, nil)
	})

	t.Run("the call is recorded with the container id as Target", func(t *testing.T) {
		var seen bool
		for _, c := range f.Calls() {
			if c.Op == OpContainerStats && c.Target == runningID {
				seen = true
			}
		}
		if !seen {
			t.Errorf("Calls() did not record OpContainerStats with Target %q", runningID)
		}
	})
}

// readAll reads from r into buf until EOF (buf must be large enough) and
// returns the number of bytes read.
func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	t.Helper()
	total := 0
	for {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
}

// waitForSubscriber polls until f has at least one live Events subscriber,
// so a test's first Emit is never lost to a goroutine that has not started
// selecting yet.
func waitForSubscriber(t *testing.T, f *Fake) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.subs)
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for a live Events subscriber")
}
