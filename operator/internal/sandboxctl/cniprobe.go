package sandboxctl

import (
	"fmt"
	"net"
	"time"
)

// probeTCPTimeout is how long a probe-tcp dial may take before it is treated
// as a failure. A CNI that drops the SYN (default-deny egress) never answers,
// so the dial must time out rather than hang the probe pod forever.
const probeTCPTimeout = 3 * time.Second

// ProbeTCP implements the `sandboxctl probe-tcp` subcommand: dial
// host:port and return nil on a successful TCP connection, an error otherwise
// (main maps that to exit code 0/1). It is the CNI enforcement probe's client
// (#31): the probe pods run the operator's own distroless image, which has no
// shell, so the connectivity check must be this binary -- exactly the same
// reasoning as Healthcheck.
func ProbeTCP(hostport string) error {
	conn, err := net.DialTimeout("tcp", hostport, probeTCPTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", hostport, err)
	}
	_ = conn.Close()
	return nil
}

// ProbeListen implements the `sandboxctl probe-listen` command: listen on
// TCP 0.0.0.0:port forever, accepting and immediately closing every
// connection. It is the CNI enforcement probe's server (#31): the negative
// (default-deny) measurement must be pod-to-pod -- dialing the API server
// instead hits the host-network blind spot where several CNIs (kindnetd
// among them) do not enforce egress. A probe pod that merely dials needs a
// reachable in-cluster peer to dial, and the distroless image has no shell
// to run `nc -l`, so the listening end is this binary. Each accepted
// connection is closed at once: the client's dial (ProbeTCP) has already
// succeeded on the SYN-ACK, so all that matters is that the listener stays
// bound for the whole probe pass. Accept is never reached on a blocking
// default-deny; only a signal that kills the process ends the loop.
func ProbeListen(port string) error {
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		return fmt.Errorf("listen on :%s: %w", port, err)
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept on :%s: %w", port, err)
		}
		_ = conn.Close()
	}
}
