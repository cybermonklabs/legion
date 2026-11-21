package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A fixed-mode start reads the selection bucket's marker before it runs today's names. A listener
// whose grant holds no read of the bucket, as the listener beside Dispatch and one whose hub never
// gave it the window's grants hold none, starts in the fixed mode even over a converted bucket it
// cannot see. The control is that read itself: it answers an error, so a build that ended its start
// on any failed read would stop the Dispatch listener. The same converted bucket read with a grant
// that allows the read refuses the start.
func TestAFixedModeStartRefusedTheBucketsReadStartsAndAGrantedReadOfAConversionStops(t *testing.T) {
	seed, public := testnats.User(t)
	stream := "KV_" + consumernames.SessionBucket
	uri := testnats.StartNkeyDenied(t, public, "$JS.API.STREAM.MSG.GET."+stream, "$JS.API.DIRECT.GET."+stream+".>")
	ctx := context.Background()
	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	if _, err := consumernames.CreateSessionBucket(ctx, admin, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	logger := logging.NewWithWriter("test", io.Discard)

	granted, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(granted.Close)
	if err := awaitFixedNaming(ctx, granted, logger, time.Second); !errors.Is(err, bus.ErrConvertedStore) {
		t.Fatalf("a granted fixed-mode start over a converted bucket = %v, want bus.ErrConvertedStore", err)
	}

	t.Setenv("NATS_NKEY_SEED", seed)
	refused, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(refused.Close)
	// Control: the refused read is an error, so a start that ended on every failed read would stop.
	if err := refused.RefuseConvertedStore(ctx); err == nil || !consumerGrantRefused(err) {
		t.Fatalf("the refused read = %v, want the grant's refusal", err)
	}
	if err := awaitFixedNaming(ctx, refused, logger, time.Second); err != nil {
		t.Fatalf("a fixed-mode start whose grant refuses the bucket's read = %v, want the start to go on", err)
	}
}

// A fixed-mode start over a server with no selection bucket starts at once; one whose server does
// not answer waits, reading the marker again, and never starts unread.
func TestAFixedModeStartThatCannotReadTheMarkerWaits(t *testing.T) {
	srv, uri := testnats.Start(t)
	consumers, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(consumers.Close)
	logger := logging.NewWithWriter("test", io.Discard)
	// Control: with the server answering and no bucket on it, the start goes on.
	if err := awaitFixedNaming(context.Background(), consumers, logger, 100*time.Millisecond); err != nil {
		t.Fatalf("a fixed-mode start with no bucket = %v, want the start to go on", err)
	}
	srv.Stop(t)
	const wait = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	started := time.Now()
	err = awaitFixedNaming(ctx, consumers, logger, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) < wait-100*time.Millisecond {
		t.Fatalf("a fixed-mode start with no server = %v after %s, want it to wait out %s unread", err, time.Since(started), wait)
	}
}
