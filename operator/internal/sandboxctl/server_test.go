package sandboxctl

import (
	"testing"
	"time"
)

func TestNewServer_HasNonZeroTimeouts(t *testing.T) {
	store := &fakeStore{}
	poll := NewPoller(store, time.Second, nil, nil)
	env := EnvironmentRef{Name: "e", Namespace: "ns"}
	srv := NewServer(Config{Listen: "127.0.0.1:0"}, store, poll, env, nil, time.Now, nil)

	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero")
	}
	if srv.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero")
	}
	if srv.WriteTimeout == 0 {
		t.Error("WriteTimeout is zero")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero")
	}
	if srv.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is zero")
	}
}
