package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// recheckRecorder is a document service that records the access re-check the server starts on it
// (docs.Service.RunAccessRecheck): the interval it was started with, sent once the run begins, and
// it runs until the server's lifetime ends, as the real one does. Every other method is the nil
// docs.API's, which register calls none of.
type recheckRecorder struct {
	docs.API
	started chan time.Duration
}

func (r *recheckRecorder) RunAccessRecheck(ctx context.Context, every time.Duration) {
	r.started <- every
	<-ctx.Done()
}

// TestTheServerStartsTheDocumentSocketsAccessRecheck: register starts the document service's access
// re-check (RunAccessRecheck) on the sockets' heartbeat (Deps.StreamHeartbeat), so an access change
// made by SQL alone, which announces nothing, still closes a socket whose person lost its room a
// heartbeat at most after it committed. The re-check runs until the server's lifetime ends.
//
// Control: register mounting the document socket and starting no re-check, as it did, under which
// the recorder is never started and the wait gives up.
func TestTheServerStartsTheDocumentSocketsAccessRecheck(t *testing.T) {
	t.Parallel()
	const heartbeat = 3 * time.Second
	recorder := &recheckRecorder{started: make(chan time.Duration, 1)}
	lifetime, end := context.WithCancel(t.Context())
	served := register(http.NewServeMux(), lifetime, Deps{Docs: recorder, StreamHeartbeat: heartbeat})
	if every := testwait.Receive(t, "the document service's access re-check to start", recorder.started); every != heartbeat {
		t.Errorf("the access re-check started every %s, want the sockets' heartbeat %s", every, heartbeat)
	}
	end()
	waitBackgroundOf(t, served)
}
