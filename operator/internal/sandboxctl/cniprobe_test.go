package sandboxctl

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// TestProbeTCPReachesProbeListen exercises the CNI probe's server/client pair
// (#31): ProbeListen binds a listener that stays up, ProbeTCP dials it and
// succeeds, and a dial to a port nothing listens on fails. This is the same
// pod-to-pod measurement the operator's controller probe performs (the
// probe pods run this binary; see internal/controller/cniprobe.go).
func TestProbeTCPReachesProbeListen(t *testing.T) {
	// Reserve an ephemeral port, then hand it to ProbeListen.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	done := make(chan error, 1)
	go func() { done <- ProbeListen(port) }()
	defer func() {
		// ProbeListen must stay bound for the whole probe pass; a return is
		// a test failure. The goroutine is left to the test's process.
		select {
		case err := <-done:
			t.Errorf("ProbeListen returned (must stay bound): %v", err)
		default:
		}
	}()

	// Poll-dial until the listener is bound; the positive direction must
	// succeed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := ProbeTCP("127.0.0.1:" + port); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("ProbeTCP never reached ProbeListen: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The negative direction: a dial to a closed port must fail (this is
	// what the default-deny measurement relies on).
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving closed port: %v", err)
	}
	closedPort := strconv.Itoa(ln2.Addr().(*net.TCPAddr).Port)
	_ = ln2.Close()
	if err := ProbeTCP("127.0.0.1:" + closedPort); err == nil {
		t.Error("ProbeTCP to a closed port: expected error, got nil")
	}
}
