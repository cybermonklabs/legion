package bus

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A core publish whose flush window was spent before the flush began is answered with the text a
// spent window has always had, which deployed log filters match. The negative control is the same
// publish and flush with time left, which the server confirms.
func TestASpentFlushWindowKeepsItsText(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	const subject = "notifications.team.spent-flush-window"
	w := client.WindowUntil(nil).(*window)
	if err := w.pin(publishAcknowledgementTimeout); err != nil {
		t.Fatalf("pin the window's connection: %v", err)
	}
	flushWithin := func(deadline time.Time) error {
		t.Helper()
		before := observe(w.conn)
		if err := w.conn.Publish(subject, []byte("{}")); err != nil {
			t.Fatalf("publish: %v", err)
		}
		return confirmFlushed(w.conn, subject, before, deadline)
	}
	// No span of its own: the server's answer can come late on a loaded machine with nothing wrong,
	// so the control's window is the test binary's deadline less testwait's margin (30 s).
	if err := flushWithin(time.Now().Add(testwait.Remaining(t, 30*time.Second))); err != nil {
		t.Fatalf("control: a flush with its window to spend answered %v", err)
	}

	spent := flushWithin(time.Now().Add(-time.Second))
	if spent == nil {
		t.Fatal("a flush whose window was spent answered nil")
	}
	if got, want := spent.Error(), errWindowElapsed(subject).Error(); got != want {
		t.Errorf("a spent flush window reads %q, want %q", got, want)
	}
}
