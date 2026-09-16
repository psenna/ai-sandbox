package wsbridge

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestActivityReadCmd(t *testing.T) {
	cmd := activityReadCmd()
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("activityReadCmd() = %v, want a 3-element sh -c script", cmd)
	}
	script := cmd[2]
	if !strings.Contains(script, "cat '"+ActivityLogPath+"'") {
		t.Errorf("activityReadCmd() script = %q, want it to cat %q", script, ActivityLogPath)
	}
	if !strings.Contains(script, "if [ ! -f '"+ActivityLogPath+"' ]; then exit 0; fi") {
		t.Errorf("activityReadCmd() script = %q, missing the missing-file guard", script)
	}
}

func TestReadActivityWorking(t *testing.T) {
	f, id := newFakeWithContainer(t)
	key := strings.Join(activityReadCmd(), " ")
	f.ExecOutput[key] = []byte("working 1700000000\n")
	f.ExecExit[key] = 0

	act, at, err := ReadActivity(context.Background(), f, id)
	if err != nil {
		t.Fatalf("ReadActivity: %v", err)
	}
	if act != ActivityWorking {
		t.Errorf("Activity = %q, want %q", act, ActivityWorking)
	}
	if want := time.Unix(1700000000, 0); !at.Equal(want) {
		t.Errorf("timestamp = %v, want %v", at, want)
	}
}

func TestReadActivityWaiting(t *testing.T) {
	f, id := newFakeWithContainer(t)
	key := strings.Join(activityReadCmd(), " ")
	f.ExecOutput[key] = []byte("waiting 1700000042\n")
	f.ExecExit[key] = 0

	act, at, err := ReadActivity(context.Background(), f, id)
	if err != nil {
		t.Fatalf("ReadActivity: %v", err)
	}
	if act != ActivityWaiting {
		t.Errorf("Activity = %q, want %q", act, ActivityWaiting)
	}
	if want := time.Unix(1700000042, 0); !at.Equal(want) {
		t.Errorf("timestamp = %v, want %v", at, want)
	}
}

func TestReadActivityMissingFile(t *testing.T) {
	f, id := newFakeWithContainer(t)
	// Nothing seeded: the fake's default ExecOutput/ExecExit for an unseeded
	// key are "" and 0 -- exactly the missing-file guard's `exit 0` with no
	// output, same as ReadOutput's own TestReadOutputMissingLog.

	act, at, err := ReadActivity(context.Background(), f, id)
	if err != nil {
		t.Fatalf("ReadActivity on a container that has never signaled: %v, want no error", err)
	}
	if act != "" || !at.IsZero() {
		t.Errorf("ReadActivity = (%q, %v), want (\"\", zero)", act, at)
	}
}

func TestReadActivityMalformedLine(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"no timestamp field", "working\n"},
		{"unrecognised status", "bogus 1700000000\n"},
		{"non-numeric timestamp", "working not-a-number\n"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, id := newFakeWithContainer(t)
			key := strings.Join(activityReadCmd(), " ")
			f.ExecOutput[key] = []byte(tc.line)
			f.ExecExit[key] = 0

			act, at, err := ReadActivity(context.Background(), f, id)
			if err != nil {
				t.Fatalf("ReadActivity(%q): %v, want no error (degrade to unknown)", tc.line, err)
			}
			if act != "" || !at.IsZero() {
				t.Errorf("ReadActivity(%q) = (%q, %v), want (\"\", zero)", tc.line, act, at)
			}
		})
	}
}

func TestReadActivityNonZeroExit(t *testing.T) {
	f, id := newFakeWithContainer(t)
	key := strings.Join(activityReadCmd(), " ")
	f.ExecOutput[key] = []byte("cat: permission denied")
	f.ExecExit[key] = 1

	_, _, err := ReadActivity(context.Background(), f, id)
	if err == nil {
		t.Fatal("ReadActivity = nil error, want one naming the exit code and message")
	}
}
