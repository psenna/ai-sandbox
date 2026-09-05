package wsbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
)

// newFakeWithContainer returns a Fake holding one running container, so
// ExecCreate's own existence check passes.
func newFakeWithContainer(t *testing.T) (*dockerclienttest.Fake, string) {
	t.Helper()
	f := dockerclienttest.New()
	ctx := context.Background()
	id, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := f.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return f, id
}

// seed keys the Fake's ExecOutput/ExecExit off the exact command ReadOutput
// would run for tailLines, so a test follows readCmd instead of drifting from
// it if readCmd's shape ever changes.
func seed(f *dockerclienttest.Fake, tailLines int, output []byte, exitCode int) {
	key := strings.Join(readCmd(tailLines), " ")
	f.ExecOutput[key] = output
	f.ExecExit[key] = exitCode
}

func TestReadCmd(t *testing.T) {
	cases := []struct {
		name      string
		tailLines int
		wantRead  string // the substring that must appear as the actual read command
	}{
		{"TailAll", TailAll, "cat '" + OutputLogPath + "'"},
		{"negative means all", -7, "cat '" + OutputLogPath + "'"},
		{"tail 1", 1, "tail -n 1 '" + OutputLogPath + "'"},
		{"tail 500", 500, "tail -n 500 '" + OutputLogPath + "'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := readCmd(tc.tailLines)
			if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
				t.Fatalf("readCmd(%d) = %v, want a 3-element sh -c script", tc.tailLines, cmd)
			}
			script := cmd[2]
			if !strings.Contains(script, tc.wantRead) {
				t.Errorf("readCmd(%d) script = %q, want it to contain %q", tc.tailLines, script, tc.wantRead)
			}
			if !strings.Contains(script, "if [ ! -f '"+OutputLogPath+"' ]; then exit 0; fi") {
				t.Errorf("readCmd(%d) script = %q, missing the missing-file guard", tc.tailLines, script)
			}
			if !strings.HasSuffix(script, "2>&1") {
				t.Errorf("readCmd(%d) script = %q, want it to end in the stderr redirect", tc.tailLines, script)
			}
		})
	}
}

func TestReadOutputAll(t *testing.T) {
	f, id := newFakeWithContainer(t)
	seed(f, TailAll, []byte("line one\r\nline two\r\n"), 0)

	got, err := ReadOutput(context.Background(), f, id, TailAll)
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "line one\r\nline two\r\n" {
		t.Errorf("ReadOutput = %q, want the seeded content", got)
	}
}

func TestReadOutputTail(t *testing.T) {
	f, id := newFakeWithContainer(t)
	// Seed BOTH commands with different content, so a bug that always falls
	// back to `cat` (or vice versa) fails loudly instead of coincidentally
	// passing.
	seed(f, TailAll, []byte("the whole log\r\n"), 0)
	seed(f, 5, []byte("just the tail\r\n"), 0)

	got, err := ReadOutput(context.Background(), f, id, 5)
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "just the tail\r\n" {
		t.Errorf("ReadOutput(5) = %q, want the tail-seeded content, not the TailAll one", got)
	}
}

func TestReadOutputNegativeTailMeansAll(t *testing.T) {
	f, id := newFakeWithContainer(t)
	seed(f, TailAll, []byte("everything\r\n"), 0)

	got, err := ReadOutput(context.Background(), f, id, -3)
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "everything\r\n" {
		t.Errorf("ReadOutput(-3) = %q, want the same as ReadOutput(TailAll)", got)
	}
}

func TestReadOutputMissingLog(t *testing.T) {
	f, id := newFakeWithContainer(t)
	// Nothing seeded: the fake's default ExecOutput/ExecExit for an unseeded
	// key are "" and 0, which is exactly what the missing-file guard's
	// `exit 0` with no output looks like.

	got, err := ReadOutput(context.Background(), f, id, TailAll)
	if err != nil {
		t.Fatalf("ReadOutput on a container with no capture log: %v, want no error", err)
	}
	if got != nil {
		t.Errorf("ReadOutput = %q, want nil", got)
	}
}

func TestReadOutputNonZeroExit(t *testing.T) {
	f, id := newFakeWithContainer(t)
	seed(f, TailAll, []byte("tail: some error message"), 1)

	_, err := ReadOutput(context.Background(), f, id, TailAll)
	if err == nil {
		t.Fatal("ReadOutput = nil error, want one naming the exit code and message")
	}
	if !strings.Contains(err.Error(), "exit code 1") || !strings.Contains(err.Error(), "some error message") {
		t.Errorf("ReadOutput error = %v, want it to carry the exit code and the command's own message", err)
	}
}

func TestReadOutputUnknownContainer(t *testing.T) {
	f := dockerclienttest.New()

	_, err := ReadOutput(context.Background(), f, "does-not-exist", TailAll)
	if err == nil {
		t.Fatal("ReadOutput = nil error, want one satisfying dockerclient.IsNotFound")
	}
	if !dockerclient.IsNotFound(err) {
		t.Errorf("ReadOutput error = %v, want it to satisfy dockerclient.IsNotFound (internal/api's 404 path depends on this)", err)
	}
}

func TestReadOutputExecFailure(t *testing.T) {
	cases := []dockerclienttest.Op{
		dockerclienttest.OpExecCreate,
		dockerclienttest.OpExecAttach,
		dockerclienttest.OpExecInspect,
	}
	for _, op := range cases {
		t.Run(string(op), func(t *testing.T) {
			f, id := newFakeWithContainer(t)
			sentinel := errors.New("injected failure")
			f.Fail(op, sentinel)

			_, err := ReadOutput(context.Background(), f, id, TailAll)
			if err == nil {
				t.Fatalf("ReadOutput with %s failing: nil error, want one wrapping the injected failure", op)
			}
			if !errors.Is(err, sentinel) {
				t.Errorf("ReadOutput error = %v, want it to wrap %v", err, sentinel)
			}
		})
	}
}

// frame builds one real Docker multiplexed stream frame for stream id
// (1=stdout, 2=stderr).
func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload))) //nolint:gosec // G115: every payload here is a short string literal
	return append(h, []byte(payload)...)
}

func TestReadOutputMultiplexed(t *testing.T) {
	f, id := newFakeWithContainer(t)
	raw := append(frame(1, "stdout line\r\n"), frame(2, "stderr line\r\n")...)
	seed(f, TailAll, raw, 0)

	got, err := ReadOutput(context.Background(), f, id, TailAll)
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	want := "stdout line\r\nstderr line\r\n"
	if string(got) != want {
		t.Errorf("ReadOutput = %q, want the demultiplexed %q", got, want)
	}
}

func TestReadOutputRawIsNotMangled(t *testing.T) {
	f, id := newFakeWithContainer(t)
	// ANSI escapes and invalid UTF-8, neither of which resembles a stream
	// frame header -- must round-trip byte for byte.
	raw := []byte("\x1b[31mred\x1b[0m text\r\n\xff\xfe garbage\r\n")
	seed(f, TailAll, raw, 0)

	got, err := ReadOutput(context.Background(), f, id, TailAll)
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("ReadOutput = %q, want the raw bytes unmodified: %q", got, raw)
	}
}

func TestIsMultiplexed(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"empty", nil, false},
		{"one well-formed frame", frame(1, "hello"), true},
		{"two well-formed frames", append(frame(1, "a"), frame(2, "b")...), true},
		{"zero-length frame", frame(1, ""), true},
		{"too short for a header", []byte{0, 0, 0}, false},
		{"header claims more than is present", func() []byte {
			h := frame(1, "hello")
			h[7] = 100 // claim a 100-byte payload we don't have
			return h
		}(), false},
		{"stream id out of range", func() []byte {
			h := frame(1, "hello")
			h[0] = 9
			return h
		}(), false},
		{"raw ANSI text", []byte("\x1b[31mred\x1b[0m\r\n"), false},
		{"raw text starting with a NUL byte", []byte("\x00not a frame"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMultiplexed(tc.raw); got != tc.want {
				t.Errorf("isMultiplexed(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
