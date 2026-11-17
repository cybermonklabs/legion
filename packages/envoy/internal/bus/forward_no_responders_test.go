package bus

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A forward to a subject nothing captures - no stream and no subscriber - is answered by the
// server's no-responders status, and Wait answers ErrForwardNotStored wrapping
// nats.ErrNoResponders, so its caller learns that nothing took the forward rather than that its
// window ran out. The window is the test's own deadline, which no answer here comes near.
//
// The control is Wait at 190f7cb177. It read whether the forward's window had ended after finish,
// which ends that window's context (bindRequest), so every error but a lost connection ended the
// wait as a window that ran out: the answer was ErrForwardNotStored alone, with the server's
// no-responders answer dropped.
func TestAForwardNothingCapturesAnswersNoResponders(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	const subject = "envoy.test.uncaptured.forward"
	forward, err := client.StartRequestStoredTo(subject, contracts.Envelope{EventID: "uncaptured", Topic: "notifications.role.r"},
		ForwardMsgID("uncaptured", subject), testnats.Remaining(t))
	if err != nil {
		t.Fatalf("the forward was refused before it was sent: %v", err)
	}
	err = forward.Wait()
	if !errors.Is(err, ErrForwardNotStored) || !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("a forward nothing captures answered %v, want ErrForwardNotStored wrapping nats.ErrNoResponders", err)
	}
	t.Logf("the forward answered %v", err)
}
