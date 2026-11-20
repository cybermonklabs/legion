//go:build odatrace

package testnats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/testnatsinprocess"
)

// The daemon's forced-order tests hold the server StartInProcess runs, so its build must carry
// the hooks: a JetStream request held at api.worker gets no answer until the release, after an
// unheld one, the control, is answered. Without the hook (forced-order.sh -drop api.worker) the
// hold is never reached and this test fails.
func TestForcedOrderTheInProcessServerIsHooked(t *testing.T) {
	server, url := StartInProcess(t)
	hooks := testnatsinprocess.Hook(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	js := inProcessJetStream(t, url)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "HOOKED", Subjects: []string{"hooked.>"}}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	conn := js.Conn()
	const subject = "$JS.API.STREAM.INFO.HOOKED"
	info := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := conn.Request(subject, nil, 30*time.Second)
			done <- err
		}()
		return done
	}
	answered := func(done <-chan error, what string) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: no answer within 30s", what)
		}
	}
	answered(info(), "the unheld stream info")

	hold := hooks.Hold(testnatsinprocess.APIWorker, testnatsinprocess.OnServer(server), testnatsinprocess.Subject(subject))
	done := info()
	t.Logf("held %s", hold.Reached(t))
	select {
	case err := <-done:
		t.Fatalf("the held stream info answered (%v) while held", err)
	case <-time.After(500 * time.Millisecond):
	}
	hold.Release()
	answered(done, "the released stream info")
}
