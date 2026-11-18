package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A kept-lane walk's last cursor write, whose acknowledgement the connection's drop lost while the
// write itself was stored, is read back as landed (roleKeepLaneCursor.confirm): the write is sent
// once and answers bus.ErrConnectionLost (bus's sendStored), and confirm reads the cursor and finds
// its own write. So the walk reports the copy it forwarded or sent the dead letter of, with its
// count, its dedupe record and its line, and goes on from the sequence the write was stored at.
// The hold drops the connection and lets it reconnect (testnats.ReplyHold.Drop), so a write sent
// again would be.
//
// The control is the same row at 7a49d38c1c, whose publish sent the write again on the new
// connection: the stream refused the second send because the first had landed, the write came back
// wrapping bus.ErrCursorMoved, and confirm reported it not landed without reading the cursor.
func TestAKeptLaneCursorWriteWhoseAckTheDropLostIsReadBackAsLanded(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	const role = "lost-cursor-role"
	lane := &roleKeepLaneCursor{
		cfg:     listenerDeliveryHandlerConfig{client: client, logger: logging.NewWithWriter("test", &cmdtest.LockedBuffer{})},
		role:    role,
		subject: bus.KeptRoleCursorSubject(role),
	}
	cursor := bus.KeptRoleCursor{Kept: 7}

	held := hold.Hold(lane.subject)
	type answer struct {
		landed bool
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t)-5*time.Second)
		defer cancel()
		landed, err := lane.confirm(ctx, cursor)
		answered <- answer{landed, err}
	}()
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold withheld no acknowledgement of the cursor write by the test's deadline")
	}
	dropped := time.Now()
	hold.Drop()
	var got answer
	select {
	case got = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the cursor write whose acknowledgement the drop lost was still waiting at the test's deadline")
	}
	if !got.landed || got.err != nil {
		t.Fatalf("confirm answered landed %t with %v %s after the drop (ErrCursorMoved: %t), want the write read back as landed with no error",
			got.landed, got.err, time.Since(dropped), errors.Is(got.err, bus.ErrCursorMoved))
	}
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	stored, err := client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read the cursor back: %v", err)
	}
	if stored.Cursor != cursor || lane.seq != stored.Sequence {
		t.Fatalf("the stream holds cursor %+v at %d and the walk names %d next, want %+v at the sequence the walk names", stored.Cursor, stored.Sequence, lane.seq, cursor)
	}
	t.Logf("confirm read its own write back as landed %s after the drop, at sequence %d", time.Since(dropped), stored.Sequence)
}
