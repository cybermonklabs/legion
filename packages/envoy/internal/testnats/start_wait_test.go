package testnats

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
)

// A server that cannot open a listener its configuration asks for logs a fatal error and will
// never accept connections, and its start fails at once with that log rather than waiting for the
// test's deadline (awaitReady). The row starts one whose leafnode port another listener holds,
// through a t with no deadline (fatalRecorder has no Deadline method), so a wait that missed the
// fatal error would last as long as the binary.
func TestAServerThatCannotOpenAListenerFailsItsStartAtOnce(t *testing.T) {
	held, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("hold a port for the server's leafnode listener: %v", err)
	}
	defer held.Close()
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		start(recorder, fmt.Sprintf("leafnodes {\n  listen: %q\n}\n", held.Addr().String()), nil)
	}()
	<-done
	if !strings.Contains(recorder.message, "logged a fatal error") || !strings.Contains(recorder.message, "leafnode") {
		t.Fatalf("a start whose leafnode port %s is held failed with %q, want the fatal error naming the leafnode listener", held.Addr(), recorder.message)
	}
}
