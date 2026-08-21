package controller

import (
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// eventCapture captures every Eventf call, so tests can assert reason/
// type/action/note without a real cluster. Preferred over
// events.NewFakeRecorder for two reasons: FakeRecorder's buffered channel
// can silently DROP events once full (a slow-draining or unread test would
// lose events with no signal), and its Eventf implementation formats only
// eventtype+reason+note into the channel string -- action is discarded
// entirely, and this package's #33 Event table asserts on action too.
//
// eventCapture implements only events.EventRecorder's single Eventf method
// (verified against k8s.io/client-go@v0.35.0/tools/internal/events:
// EventRecorder has just Eventf; WithLogger belongs to the separate
// EventRecorderLogger interface that extends it, which Reconciler.Recorder/
// SlotScheduler.Recorder's declared field type does not require).
type eventCapture struct {
	mu     sync.Mutex
	events []capturedEvent
}

// capturedEvent is one Eventf call, with Note already formatted (args
// applied) for easy substring assertions.
type capturedEvent struct {
	Regarding       runtime.Object
	EventType       string
	Reason          string
	Action          string
	Note            string
	FormatArgsCount int
}

var _ events.EventRecorder = (*eventCapture)(nil)

func newEventCapture() *eventCapture {
	return &eventCapture{}
}

func (c *eventCapture) Eventf(regarding, _ runtime.Object, eventtype, reason, action, note string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{
		Regarding:       regarding,
		EventType:       eventtype,
		Reason:          reason,
		Action:          action,
		Note:            fmt.Sprintf(note, args...),
		FormatArgsCount: len(args),
	})
}

// Events returns a snapshot copy of every captured event, in call order.
func (c *eventCapture) Events() []capturedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedEvent, len(c.events))
	copy(out, c.events)
	return out
}

// String joins every captured event's "Reason: Note" onto its own line, for
// simple substring leak-detection assertions (mirrors logCapture.String).
func (c *eventCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := make([]string, len(c.events))
	for i, e := range c.events {
		lines[i] = e.Reason + ": " + e.Note
	}
	return strings.Join(lines, "\n")
}

// Contains reports whether s appears in any captured event's Reason or Note.
func (c *eventCapture) Contains(s string) bool {
	return strings.Contains(c.String(), s)
}

// ByReason returns every captured event with the given Reason, in call
// order.
func (c *eventCapture) ByReason(reason string) []capturedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedEvent
	for _, e := range c.events {
		if e.Reason == reason {
			out = append(out, e)
		}
	}
	return out
}
