package bus

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// sentAgainWhileNoStream is what send, one write to a stream (a key's put, create, update, delete
// or purge, or a publish to the notification stream), answers, sent again while that answer is
// nats.ErrNoStreamResponse, the stream is still there (gone answers false) and ctx lasts. Its
// callers are the bucket handle's writes (writtenOn) and the stored publish path (sendStored).
//
// That answer is nats.go's for a publish the server answered "no responders" (nats.go v1.55.0
// js.go:597-623): no subscription in the account took the message, so no stream stored it, and
// sending it again cannot store it twice. A stream that is there answers so for a moment after it
// gets a leader: its Raft group names the leader at once (nats-server v2.15.1 server/raft.go:6307),
// which is what a start waits for (awaitStreamLeader), but the member leads only once its first
// entries are processed (raft.go:1603-1613), its stream subscribes to its subjects after that
// (stream.go:1479), and every other member learns of the subscription over its route. A start that
// answered on a slow first election, its create's resend answered while the election ran, handed
// its caller a stream whose first write drew that answer: "nats: no response from stream".
// nats.go's own sends again exist for such a blip in a lead and end after half a second; these go
// on while the stream is there, until ctx, the write's own bound, ends.
//
// A stream that is gone answers the same, so after each such answer the stream is read (gone):
// when the read is answered that the stream is not there, the answer is returned at once, as it
// was before any send again, and a deleted stream costs a write no more than it did. A read that
// finds the stream, or draws no answer, says nothing against sending again.
//
// Between two sends it waits noStreamPause, which grows with each send, so the writes of every
// client a recovering cluster has made wait do not all come back at once. No send again starts
// once ctx is canceled or within noStreamMargin of its deadline (nearEnd), and the last send's
// answer is returned then, nats.ErrNoStreamResponse itself, so a caller still reads that the
// stream stored nothing. That is why this loop is the bus's and not nats.go's RetryAttempts(-1):
// nats.go's loop sends again with a ctx that ended in its pause and answers ctx's error
// (js.go:598-618), which the stored path must read as a send that may have stored the message
// (sendUnanswered); the bucket's legacy writes take no publish options at all; and a delete's
// options (nats.DeleteOpt) cannot be read outside nats.go to build the publish here. Any other
// answer is returned at once. A send already in flight when the bound ends is answered by its own
// wait, which the stored path reads as a send that may have stored the message, as it did before
// writes were sent again.
func sentAgainWhileNoStream[T any](ctx context.Context, send func() (T, error), gone func(context.Context) bool) (T, error) {
	for sent := 1; ; sent++ {
		value, err := send()
		if !errors.Is(err, nats.ErrNoStreamResponse) || gone(ctx) {
			return value, err
		}
		if nearEnd(ctx) || pause(ctx, noStreamPause(sent, rand.Int64N)) != nil || nearEnd(ctx) {
			return value, err
		}
	}
}

// noStreamMargin is how long before its context's deadline a write no stream took is no longer
// sent again (nearEnd), and how much earlier than its caller's context a bucket write the jetstream
// handle sends ends (writtenOn). A deadline's context reports its end only when its timer fires,
// which runs late under load: a probe of the stored path at 490 bounds on a loaded box sent again
// after the deadline, its context not yet ended, in 5 and 11 of them, and in none with a 100 ms
// margin.
const noStreamMargin = 100 * time.Millisecond

// nearEnd reports whether ctx has ended or its deadline is within noStreamMargin, read from the
// deadline itself rather than from ctx's end (noStreamMargin).
func nearEnd(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < noStreamMargin
}

// noStreamPause is the wait after the sent'th send of a write no stream took
// (sentAgainWhileNoStream): the waits of a prompt redial (promptRedialWait), at once but for up to
// 50 ms after the first, then drawn between half and all of a bound that starts at 250 ms, nats.go's
// own pause between its sends, and doubles with each send up to 2 s.
func noStreamPause(sent int, draw func(n int64) int64) time.Duration {
	return promptRedialWait(sent, draw)
}

// streamGone reports whether read, one read of a stream, made within ctx and its run's read limit
// (limitedRead), is answered that the stream is not there: the one answer that says a write no
// stream took will not be taken by sending it again (sentAgainWhileNoStream).
func streamGone(ctx context.Context, read func() error) bool {
	_, _, answered, err := limitedRead(ctx, readLimitOf(ctx), func() (struct{}, error) { return struct{}{}, read() })
	return answered && (errors.Is(err, nats.ErrStreamNotFound) || errors.Is(err, jetstream.ErrStreamNotFound))
}
