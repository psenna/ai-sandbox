// Package dockerclient is a narrow interface over the moby SDK covering
// only the operations the agent lifecycle needs: volumes, networks,
// containers, exec and the event stream. Narrowing it here keeps the
// lifecycle code unit-testable against a fake instead of a live daemon.
//
// Scaffold only (issue #61); the interface, its moby-backed implementation
// and the fake land in task 3.
package dockerclient
