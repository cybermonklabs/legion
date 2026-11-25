package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A takeover record whose previous holder's subject has nothing new is written again only once
// its last write is takeoverRecordRefresh old, however many listeners' sweeps run its passes: two
// listeners, each sweeping every roleWakeInterval for an hour, write it 60m / 15m = 4 times, not
// once per pass (2 listeners x 12 sweeps = 24, which every pass writing gives). Each listener's
// pass reads the record as the other last wrote it, as a sweep lists it from the stream, with the
// write's time on the row's clock (runTakeoverPass's own), since the stream stamps a record with the
// server's clock and the row's clock is moved.
//
// The control is a pass that copies: a forward stored on the previous holder's subject after the
// last write is copied by the next pass, which writes the record at once with the sequence it
// reached, young as the last write is.
func TestATakeoverRecordThatNothingMovesIsWrittenOncePerRefresh(t *testing.T) {
	t.Parallel()
	rig := newCrashRig(t)
	const a, b, role = "ses_rw_a", "ses_rw_b", "rw"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	first := rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	rig.claim(rig.mux, b, role)
	crashAwaitEvents(t, next, first, nil, "the new holder, from the claim's own pass")
	rig.idle(rig.cfg(), role)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	records, err := rig.harness.client.TakeoverRecords(ctx, role)
	if err != nil || len(records) != 1 {
		t.Fatalf("the takeover records of %s: %d, %v; want one", role, len(records), err)
	}
	latest := records[0]
	latest.Written = rig.clock.Now()
	var writes atomic.Int32
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{beforeRecordWrite: func(r, previous string, _ uint64, _ bool) {
		if r == role && previous == a {
			writes.Add(1)
		}
	}})
	listeners := []listenerDeliveryHandlerConfig{rig.cfg(), rig.anotherListener()}
	pass := func(cfg listenerDeliveryHandlerConfig) {
		t.Helper()
		stored, _, err := runTakeoverPass(ctx, cfg, latest)
		if err != nil {
			t.Fatalf("a pass of %s's record: %v", role, err)
		}
		latest = stored
	}

	t.Run("a record nothing moves, two listeners sweeping every five minutes for an hour: four writes", func(t *testing.T) {
		const hour = time.Hour
		for elapsed := time.Duration(0); elapsed < hour; elapsed += roleWakeInterval {
			rig.clock.advance(roleWakeInterval)
			for _, cfg := range listeners {
				pass(cfg)
			}
		}
		if got, want := writes.Load(), int32(hour/takeoverRecordRefresh); got != want {
			t.Fatalf("the record was written %d times in the hour; want %d, once per takeoverRecordRefresh", got, want)
		}
	})

	t.Run("control: a pass that copies writes the record at once", func(t *testing.T) {
		before, at := writes.Load(), latest.Record.Next
		rig.lateForward(a, role, "stored after the record's last write")
		pass(listeners[0])
		if got := writes.Load() - before; got != 1 {
			t.Fatalf("the pass that copied wrote the record %d times; want 1", got)
		}
		if latest.Record.Next <= at {
			t.Fatalf("the record's sequence is %d after the copy; want past %d", latest.Record.Next, at)
		}
	})
}
