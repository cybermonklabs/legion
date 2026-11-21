package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestStartHubAdmissionReadsANilProofAsNone: main hands startHubAdmission what the proof's
// constructor answers, which is a nil pointer for a deployment with no hub. Held in the
// hubAdmission interface, that pointer is not nil, so startHubAdmission reads a nil interface and a
// nil pointer alike as no proof: it answers no gate and no hold, starts no loop, and, with
// DISPATCH_PRIVATE_HUBS set, logs the WARN naming the hubs. Control: check only the interface for
// nil, and the nil pointer's row answers a gate and logs no WARN, and the test names it.
func TestStartHubAdmissionReadsANilProofAsNone(t *testing.T) {
	boot := bootConfig{PrivateHubs: []string{"nats://hub:4222"}}
	for name, admission := range map[string]hubAdmission{"a nil interface": nil, "a nil pointer": (*fakeHubAdmission)(nil)} {
		t.Run(name, func(t *testing.T) {
			var logged cmdtest.LockedBuffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			gate, ready := startHubAdmission(t.Context(), boot, admission)
			if gate != nil || ready != nil {
				t.Errorf("%s: startHubAdmission answered a gate %v and a hold %v, want neither", name, gate, ready != nil)
			}
			if line := logged.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, "hubs=nats://hub:4222") {
				t.Errorf("%s over DISPATCH_PRIVATE_HUBS=nats://hub:4222 logged %q, want the WARN naming the hub", name, line)
			}
		})
	}
}

// fakeHubAdmission is a private-hub admission proof main can run (hubAdmission): it admits every
// proof, answers ready as its last result, closes started once its loop runs, and keeps the
// listener inventory and client SetListeners gave it.
type fakeHubAdmission struct {
	ready   bool
	started chan struct{}
	once    sync.Once

	listeners []busprojects.ListenerTarget
	client    *http.Client
}

func newFakeHubAdmission(ready bool) *fakeHubAdmission {
	return &fakeHubAdmission{ready: ready, started: make(chan struct{})}
}

func (a *fakeHubAdmission) Prove(context.Context) error { return nil }

func (a *fakeHubAdmission) Ready() bool { return a.ready }

func (a *fakeHubAdmission) Run(ctx context.Context) {
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
}

func (a *fakeHubAdmission) SetListeners(listeners []busprojects.ListenerTarget, client *http.Client) {
	a.listeners, a.client = listeners, client
}

// ReportHandler serves the report of a proof with no hub and no listener read.
func (a *fakeHubAdmission) ReportHandler() http.Handler {
	return (*busprojects.HubAdmission)(nil).ReportHandler()
}

// waitRunning fails the test unless the proof's loop was started; the bound only ends a test whose
// loop never starts, and decides nothing that passes.
func (a *fakeHubAdmission) waitRunning(t *testing.T) {
	t.Helper()
	select {
	case <-a.started:
	case <-time.After(30 * time.Second):
		t.Fatal("startHubAdmission never started the proof's loop (Run)")
	}
}
