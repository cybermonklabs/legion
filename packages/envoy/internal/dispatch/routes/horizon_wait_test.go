package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/logtest"
)

// Waiting on work that waits on the event horizon.
//
// The event horizon's proof is instance-wide (internal/dispatch/events/horizon.go): any
// transaction holding a transaction id anywhere on the Postgres instance holds it back until it
// ends, and CI's unit step runs every package of the module against one Postgres, so another
// package's test is such a holder. Two waits here stand behind it: the event feed's events_start,
// which feedEvents writes only once the relay's first cursor has awaited the horizon
// (firstRelayCursor in internal/dispatch/api/event_stream_relay.go), and a horizon proven and
// persisted above the log's newest event. No fixed deadline bounds either (docs/testing.md, "A test
// that waits for the event feed's first frame"): the 10 s and 30 s these tests had failed whenever
// another client of the instance held a transaction for longer.
//
// So the wait keys on the horizon's own account of itself, as cmd/dispatch's waitEventsStart does
// for a Dispatch process: while the horizon keeps logging events.StallWarning it is saying it is
// held back by an open transaction, and the wait goes on for as long as that keeps arriving. It
// fails, naming the newest warning, when the horizon never said it was held, when it stopped saying
// so, and when the hold is in the test's own database and has stood horizonWaitOwnHoldBound: a
// Dispatch write left open across the horizon's proof is the defect (dispatch://INFRA-636), not a
// stall to wait out. The horizon runs in this process, so a warning is a record of the default
// logger rather than a line of a process's output, and its pending_for and database are the values
// warnStall logged, read with no parse.
//
// The bounds are cmd/dispatch's, since the horizon and its thresholds are the same code there and
// here; cmd/dispatch/live_events_wait_test.go carries their derivations in full.
const (
	// testHorizonStallWarning is the stall and repeat threshold a horizon these tests wait on
	// warns at, cmd/dispatch's testHorizonStallWarning: the signal must arrive inside the wait's
	// patience rather than at the production five and thirty seconds.
	testHorizonStallWarning = 250 * time.Millisecond
	// horizonWaitBase is how long the wait gives a horizon that has said nothing. The soonest a
	// held horizon can have said so is its stallAfter plus its warnEvery (testHorizonStallWarning
	// each) plus horizonDiagnosisTimeout (2 s), the bound on the diagnosis warnStall runs before
	// it logs: 2.5 s, rounded up to a whole second.
	horizonWaitBase = 3 * time.Second
	// horizonWaitExtension is how long the wait gives a horizon that has said it is held, since
	// its newest warning: two warnings stand at most warnEvery plus horizonBackoffMax (250 ms)
	// plus horizonDiagnosisTimeout apart, 2.5 s, rounded up to a whole second.
	horizonWaitExtension = 3 * time.Second
	// horizonWaitPoll is how often the wait reads the records: a sampling rate below a tenth of
	// the extension window, so it costs that window nothing, not a bound.
	horizonWaitPoll = 250 * time.Millisecond
	// horizonWaitDrain is what the wait leaves of the run's own deadline, so its failure prints
	// rather than being cut off by the package timeout, which would name no test.
	horizonWaitDrain = 30 * time.Second
	// horizonWaitOwnHoldBound is how long, by a warning's own pending_for, a hold in the test's
	// own database may stand before the wait fails on it. Any client of that database holds the
	// horizon as a foreign one does - the test's storetest pool, the router's own pools - and an
	// ordinary short transaction of one is no defect; cmd/dispatch measured the worst age of one
	// at 1.449 s under load, and this is three times that, rounded up.
	horizonWaitOwnHoldBound = 5 * time.Second
)

// waitBehindHorizon returns the one result arrived delivers, the outcome of work that waits on the
// event horizon, which what names for a failure. logged is the default logger's recorder,
// installed before the horizon was built, and ownDatabase the database the horizon's pool reads.
func waitBehindHorizon[T any](t *testing.T, what string, arrived <-chan T, logged *logtest.Recorder, ownDatabase string) T {
	t.Helper()
	began := time.Now()
	// Warnings already recorded belong to a hold before this wait, perhaps one long over, so the
	// wait counts only what the horizon says from here on.
	before := len(stallWarnings(logged))
	ticker := time.NewTicker(horizonWaitPoll)
	defer ticker.Stop()
	// A run given no deadline (go test -timeout 0) has no drain arm to fail on: a nil channel.
	var runOut <-chan time.Time
	if deadline, ok := t.Deadline(); ok {
		drain := time.NewTimer(time.Until(deadline.Add(-horizonWaitDrain)))
		defer drain.Stop()
		runOut = drain.C
	}
	state := horizonWaitState{ownDatabase: ownDatabase}
	rose := began
	for {
		select {
		case result := <-arrived:
			return result
		case <-runOut:
			return failHorizonWait(t, arrived, "%s did not arrive in the %v the run had left; the horizon's newest stall warning was %s",
				what, time.Since(began).Round(time.Millisecond), stallWarningText(state))
		case now := <-ticker.C:
			// A count of the warnings since the wait began, never whether one was seen: a
			// check that latched on the first warning would extend the wait for ever on a
			// horizon that has stopped warning.
			if since := stallWarnings(logged)[before:]; len(since) > state.warnings {
				state.warnings = len(since)
				state.lastWarning = since[len(since)-1]
				rose = now
			}
			state.elapsed = now.Sub(began)
			state.sinceNewWarning = now.Sub(rose)
			switch horizonWaitVerdictOf(state) {
			case horizonWaitNoSignal:
				return failHorizonWait(t, arrived, "%s did not arrive in %v, and the event horizon never said it was held back by an open transaction",
					what, state.elapsed.Round(time.Millisecond))
			case horizonWaitSignalStopped:
				return failHorizonWait(t, arrived, "%s did not arrive in %v, and the event horizon stopped saying it was held back %v ago; its newest stall warning was %s",
					what, state.elapsed.Round(time.Millisecond), state.sinceNewWarning.Round(time.Millisecond), stallWarningText(state))
			case horizonWaitOwnHold:
				return failHorizonWait(t, arrived, "%s did not arrive in %v, and the event horizon says a client of the test's own database %s has held it back for at least %v; its newest stall warning was %s",
					what, state.elapsed.Round(time.Millisecond), ownDatabase, horizonWaitOwnHoldBound, stallWarningText(state))
			}
		}
	}
}

// failHorizonWait is the one place the wait fails from. A result already delivered wins over any
// verdict: the drain timer and the ticker can be ready at the same select, which chooses among
// ready arms at random, so a result delivered in the same tick as a failing verdict would
// otherwise fail a run that had it.
func failHorizonWait[T any](t *testing.T, arrived <-chan T, format string, args ...any) T {
	t.Helper()
	select {
	case result := <-arrived:
		return result
	default:
	}
	t.Fatalf(format, args...)
	var none T
	return none
}

// horizonWaitState is what the wait knows at one poll.
type horizonWaitState struct {
	elapsed         time.Duration // since the wait began
	sinceNewWarning time.Duration // since the stall-warning count last rose; meaningless while warnings is 0
	warnings        int           // stall warnings recorded since the wait began
	lastWarning     slog.Record   // the newest of them; the zero Record while warnings is 0
	ownDatabase     string        // the database the horizon's pool reads
}

// horizonWaitVerdict is what the wait does with one horizonWaitState: wait on, or fail with the
// reason the state gives.
type horizonWaitVerdict int

const (
	horizonWaitOn            horizonWaitVerdict = iota
	horizonWaitNoSignal                         // nothing arrived and the horizon never said it was held
	horizonWaitSignalStopped                    // the horizon stopped saying it was held, and nothing arrived
	horizonWaitOwnHold                          // the stall is a hold in the test's own database, not a foreign one
)

// horizonWaitVerdictOf judges one poll of the wait, as a function of the state alone.
//
// A warning whose diagnosis failed carries no database (warnStall's diagnosis_error branch), and
// neither does one that found no holder (holder "none open now"); naming no database, each counts
// as a stall and extends the wait. pending_for is the age of the oldest pending observation, which
// the named holder has been open at least as long as unless a foreign holder released in between,
// so the own-hold rule fires no sooner than one on the holder's own age would.
func horizonWaitVerdictOf(state horizonWaitState) horizonWaitVerdict {
	pendingFor, _ := stallWarningAttr(state.lastWarning, "pending_for")
	database, named := stallWarningAttr(state.lastWarning, "database")
	switch {
	case state.warnings > 0 && pendingFor.Kind() == slog.KindDuration && pendingFor.Duration() >= horizonWaitOwnHoldBound &&
		named && database.Kind() == slog.KindString && database.String() == state.ownDatabase:
		return horizonWaitOwnHold
	case state.warnings == 0 && state.elapsed >= horizonWaitBase:
		return horizonWaitNoSignal
	case state.warnings > 0 && state.sinceNewWarning >= horizonWaitExtension:
		return horizonWaitSignalStopped
	default:
		return horizonWaitOn
	}
}

// stallWarnings is every events.StallWarning record logged so far, oldest first.
func stallWarnings(logged *logtest.Recorder) []slog.Record {
	var warnings []slog.Record
	for _, record := range logged.Records() {
		if record.Message == events.StallWarning {
			warnings = append(warnings, record)
		}
	}
	return warnings
}

// stallWarningAttr is the value warning carries under key, and whether it carries key at all.
func stallWarningAttr(warning slog.Record, key string) (slog.Value, bool) {
	var value slog.Value
	found := false
	warning.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			value, found = attr.Value.Resolve(), true
		}
		return !found
	})
	return value, found
}

// stallWarningText is the newest warning as the deployment's text handler writes it, which names
// the holder's kind, pid, database and query, for a failure to print.
func stallWarningText(state horizonWaitState) string {
	if state.warnings == 0 {
		return "(none since the wait began)"
	}
	var text strings.Builder
	_ = slog.NewTextHandler(&text, nil).Handle(context.Background(), state.lastWarning)
	return strings.TrimSuffix(text.String(), "\n")
}

// waitEventsStart returns the first frame but a heartbeat that conn receives once the caller has
// subscribed it to the event feed: the events_start, which the caller checks. The caller reads conn
// again only once this has returned.
func waitEventsStart(t *testing.T, conn *gws.Conn, logged *logtest.Recorder, ownDatabase string) []byte {
	t.Helper()
	result := waitBehindHorizon(t, "the event feed's events_start", readFirstFrame(conn), logged, ownDatabase)
	if result.err != nil {
		t.Fatalf("read the event feed's events_start: %v", result.err)
	}
	return result.frame
}

// firstFrameRead is the one result readFirstFrame sends: the frame, or the read that failed.
type firstFrameRead struct {
	frame []byte
	err   error
}

// readFirstFrame reads conn on a goroutine of its own until its first frame but a heartbeat, and
// sends exactly one result. It sets no read deadline, because a gorilla connection whose read has
// timed out is corrupt and the test reads this one afterwards. The channel is buffered, so the
// goroutine leaves even when the test has failed and nobody receives - the test's cleanup closes
// conn, which ends the read - and it touches conn no more once it has sent.
func readFirstFrame(conn *gws.Conn) <-chan firstFrameRead {
	read := make(chan firstFrameRead, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				read <- firstFrameRead{err: err}
				return
			}
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &frame); err != nil || frame.Type != "heartbeat" {
				read <- firstFrameRead{frame: data}
				return
			}
		}
	}()
	return read
}

// persistedThrough proves horizon's H on a goroutine of its own, as often as it can, until the H
// some process persisted reaches newest, and then sends nil, or sends the error that stopped it.
// It stops when ctx ends, which t.Context does as the test ends, failed or not.
func persistedThrough(ctx context.Context, horizon *events.Horizon, newest int64) <-chan error {
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := horizon.Safe(ctx); err != nil {
				done <- fmt.Errorf("observe the horizon: %w", err)
				return
			}
			persisted, err := horizon.Persisted(ctx)
			if err != nil {
				done <- fmt.Errorf("read the persisted horizon: %w", err)
				return
			}
			if persisted >= newest {
				done <- nil
				return
			}
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return done
}

// TestHorizonWaitVerdictOfKeysTheWaitOnTheStallWarning drives each rule of the wait's judgement
// either side of its bound, with no server and no Postgres: the judgement is a function of its
// state alone.
func TestHorizonWaitVerdictOfKeysTheWaitOnTheStallWarning(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	warning := func(attrs ...any) slog.Record {
		record := slog.NewRecord(time.Now(), slog.LevelWarn, events.StallWarning, 0)
		record.Add(attrs...)
		return record
	}
	held := func(pendingFor time.Duration, database string) slog.Record {
		return warning("pending_for", pendingFor, "observed_id", int64(2), "safe_horizon", int64(0),
			"holder", "backend", "pid", int32(86), "database", database, "query", "select 1")
	}
	for _, row := range []struct {
		name  string
		state horizonWaitState
		want  horizonWaitVerdict
	}{
		{"no warning yet, inside the base", horizonWaitState{elapsed: horizonWaitBase - time.Millisecond}, horizonWaitOn},
		{"no warning at the base", horizonWaitState{elapsed: horizonWaitBase}, horizonWaitNoSignal},
		{"a foreign hold still warning", horizonWaitState{elapsed: time.Minute, sinceNewWarning: horizonWaitExtension - time.Millisecond, warnings: 9, lastWarning: held(time.Minute, "postgres")}, horizonWaitOn},
		{"a foreign hold that stopped warning", horizonWaitState{elapsed: time.Minute, sinceNewWarning: horizonWaitExtension, warnings: 9, lastWarning: held(time.Minute, "postgres")}, horizonWaitSignalStopped},
		{"an own-database hold inside its bound", horizonWaitState{elapsed: time.Second, warnings: 1, lastWarning: held(horizonWaitOwnHoldBound-time.Millisecond, ownDatabase)}, horizonWaitOn},
		{"an own-database hold at its bound", horizonWaitState{elapsed: time.Second, warnings: 1, lastWarning: held(horizonWaitOwnHoldBound, ownDatabase)}, horizonWaitOwnHold},
		{"another database whose name the own one prefixes", horizonWaitState{elapsed: time.Second, warnings: 1, lastWarning: held(time.Minute, ownDatabase+"cd")}, horizonWaitOn},
		{"a failed diagnosis, which names no database", horizonWaitState{elapsed: time.Second, warnings: 1, lastWarning: warning("pending_for", time.Minute, "diagnosis_error", "timeout")}, horizonWaitOn},
		{"no holder open now", horizonWaitState{elapsed: time.Second, warnings: 1, lastWarning: warning("pending_for", time.Minute, "holder", "none open now")}, horizonWaitOn},
		{"a pending_for that is no duration", horizonWaitState{elapsed: time.Second, warnings: 1, lastWarning: warning("pending_for", "a minute", "database", ownDatabase)}, horizonWaitOn},
	} {
		row.state.ownDatabase = ownDatabase
		if got := horizonWaitVerdictOf(row.state); got != row.want {
			t.Errorf("%s: verdict %d, want %d", row.name, got, row.want)
		}
	}
}
