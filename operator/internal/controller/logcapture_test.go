package controller

import (
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// logCapture captures every log line written through the logr.Logger it
// vends, so secretleak_test.go can assert a sentinel value never appears in
// any of them. Verbosity is set high enough to capture V(1) logs (the level
// this reconciler uses for "not implemented"/permanent-misconfiguration
// lines).
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func newLogCapture() (*logCapture, logr.Logger) {
	c := &logCapture{}
	logger := funcr.New(func(prefix, args string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 10})
	return c, logger
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func (c *logCapture) Contains(s string) bool {
	return strings.Contains(c.String(), s)
}
