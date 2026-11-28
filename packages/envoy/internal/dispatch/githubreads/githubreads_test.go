package githubreads

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// cutMarker is the marker Cause ends a cut text with, n being the storable text's length.
func cutMarker(n int) string { return " … (cut from " + strconv.Itoa(n) + " bytes)" }

// A cause is at most CauseLimit bytes of storable text. A text up to the limit is kept whole; a
// longer one is cut on a rune boundary and ends with a marker naming the length it was cut from,
// inside the limit. The text is made storable before it is measured, so U+0000 and invalid UTF-8
// become U+FFFD and count at U+FFFD's three bytes.
func TestCauseCutsAStorableTextToTheLimitOnARuneBoundary(t *testing.T) {
	ascii := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name  string
		value string
		want  string
	}{{
		name:  "1,023 bytes are kept whole",
		value: ascii(1023),
		want:  ascii(1023),
	}, {
		name:  "1,024 bytes are kept whole",
		value: ascii(1024),
		want:  ascii(1024),
	}, {
		name:  "1,025 bytes are cut",
		value: ascii(1025),
		want:  ascii(CauseLimit-len(cutMarker(1025))) + cutMarker(1025),
	}, {
		name:  "U+0000 becomes U+FFFD",
		value: "status 503: a\x00b",
		want:  "status 503: a\uFFFDb",
	}, {
		name:  "invalid UTF-8 becomes U+FFFD",
		value: "status 503: a\xffb\xc3",
		want:  "status 503: a\uFFFDb\uFFFD",
	}, {
		// 1,023 bytes and a U+0000 are 1,024 bytes as given and 1,026 once storable, so the
		// storable text is what is measured and cut.
		name:  "a U+0000 that makes the storable text pass the limit",
		value: ascii(1023) + "\x00",
		want:  ascii(CauseLimit-len(cutMarker(1026))) + cutMarker(1026),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Cause(tc.value)
			if got != tc.want {
				t.Fatalf("Cause = %q (%d bytes), want %q (%d bytes)", got, len(got), tc.want, len(tc.want))
			}
			if len(got) > CauseLimit {
				t.Fatalf("Cause is %d bytes, past the %d-byte limit", len(got), CauseLimit)
			}
		})
	}
}

// A multibyte character across the cut is left out whole: the cut falls before its first byte,
// whichever of its bytes the limit lands on, so the cause is valid UTF-8 and a head of the text.
func TestCauseLeavesOutWholeAMultibyteCharacterAcrossTheCut(t *testing.T) {
	const total = 4096
	marker := cutMarker(total)
	at := CauseLimit - len(marker)
	for _, character := range []string{"é", "€", "😀"} {
		for into := 1; into < len(character); into++ {
			t.Run(character+" cut after byte "+strconv.Itoa(into), func(t *testing.T) {
				// The character starts into bytes before the cut, so the cut lands inside it.
				head := strings.Repeat("a", at-into)
				value := head + character + strings.Repeat("b", total-len(head)-len(character))
				got := Cause(value)
				if want := head + marker; got != want {
					t.Fatalf("Cause = …%q (%d bytes), want …%q (%d bytes)", got[max(0, len(got)-40):], len(got), want[len(want)-40:], len(want))
				}
				if !utf8.ValidString(got) || len(got) > CauseLimit {
					t.Fatalf("Cause is %d bytes, valid UTF-8 %v; want valid UTF-8 within %d bytes", len(got), utf8.ValidString(got), CauseLimit)
				}
			})
		}
	}
}

// The cause is the first failure noted, however many follow, and a nil error is no failure. A
// sent request is marked apart from the failures, and a context with no attempt takes every note
// and changes nothing.
func TestTheCauseIsTheFirstFailureNoted(t *testing.T) {
	ctx, attempt := Begin(context.Background(), CIReconcile)
	if sent, failed, cause := attempt.Outcome(); sent || failed || cause != "" {
		t.Fatalf("a new attempt: sent %v, failed %v, cause %q; want nothing", sent, failed, cause)
	}
	NoteFailure(ctx, nil)
	NoteSent(ctx)
	if sent, failed, cause := attempt.Outcome(); !sent || failed || cause != "" {
		t.Fatalf("after a sent request and a nil error: sent %v, failed %v, cause %q; want sent and no failure", sent, failed, cause)
	}
	NoteFailure(ctx, errors.New("GET https://api.github.com/app/hook/deliveries: status 503: first"))
	NoteFailure(ctx, errors.New("GET https://api.github.com/app/hook/deliveries: status 502: second"))
	sent, failed, cause := attempt.Outcome()
	if !sent || !failed || cause != "GET https://api.github.com/app/hook/deliveries: status 503: first" {
		t.Fatalf("after two failures: sent %v, failed %v, cause %q; want the first", sent, failed, cause)
	}

	// No attempt: nothing to note on, and nothing panics.
	plain := context.Background()
	NoteSent(plain)
	NoteFailure(plain, errors.New("noted on nothing"))
	held, scope := Hold(plain)
	if held != plain || scope != nil {
		t.Fatalf("Hold on a context with no attempt = %v, %v; want the context itself and no scope", held, scope)
	}
	scope.Keep()
	scope.Drop()
}

// A held scope's failures reach the attempt when its caller keeps them and never when it drops
// them, while each request it sends is marked on the attempt at once either way. Scopes nest: a
// note goes to the innermost, and Keep passes it to the scope that encloses that one.
func TestAHeldScopesNotesReachTheAttemptOnlyWhenKept(t *testing.T) {
	failure := errors.New("POST https://api.github.com/graphql: status 502: bad gateway")
	t.Run("kept", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		held, scope := Hold(ctx)
		NoteSent(held)
		NoteFailure(held, failure)
		if sent, failed, _ := attempt.Outcome(); !sent || failed {
			t.Fatalf("before Keep: sent %v, failed %v; want sent and the failure still held", sent, failed)
		}
		scope.Keep()
		if sent, failed, cause := attempt.Outcome(); !sent || !failed || cause != failure.Error() {
			t.Fatalf("after Keep: sent %v, failed %v, cause %q; want sent and the held failure", sent, failed, cause)
		}
	})
	t.Run("dropped", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		held, scope := Hold(ctx)
		NoteSent(held)
		NoteFailure(held, failure)
		scope.Drop()
		// Notes after the scope settled follow its choice.
		NoteFailure(held, errors.New("after the drop"))
		scope.Keep()
		if sent, failed, cause := attempt.Outcome(); !sent || failed || cause != "" {
			t.Fatalf("after Drop: sent %v, failed %v, cause %q; want sent and no failure", sent, failed, cause)
		}
	})
	t.Run("a note after Keep goes straight on", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		held, scope := Hold(ctx)
		scope.Keep()
		NoteFailure(held, failure)
		if _, failed, cause := attempt.Outcome(); !failed || cause != failure.Error() {
			t.Fatalf("a note after Keep: failed %v, cause %q; want it on the attempt", failed, cause)
		}
	})
	t.Run("a failure that reached the attempt first stays the cause", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		held, scope := Hold(ctx)
		NoteFailure(held, failure)
		NoteFailure(ctx, errors.New("the loop's own, noted first on the attempt"))
		scope.Keep()
		if _, _, cause := attempt.Outcome(); cause != "the loop's own, noted first on the attempt" {
			t.Fatalf("cause %q, want the failure that reached the attempt first", cause)
		}
	})
	t.Run("nested", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		outer, outerScope := Hold(ctx)
		inner, innerScope := Hold(outer)
		NoteSent(inner)
		NoteFailure(inner, failure)
		innerScope.Keep()
		if sent, failed, _ := attempt.Outcome(); !sent || failed {
			t.Fatalf("the inner scope kept: sent %v, failed %v; want sent and the failure held by the outer scope", sent, failed)
		}
		outerScope.Keep()
		if _, failed, cause := attempt.Outcome(); !failed || cause != failure.Error() {
			t.Fatalf("both kept: failed %v, cause %q; want the inner scope's failure", failed, cause)
		}

		ctx, attempt = Begin(context.Background(), CIReconcile)
		outer, outerScope = Hold(ctx)
		inner, innerScope = Hold(outer)
		NoteFailure(inner, failure)
		innerScope.Keep()
		outerScope.Drop()
		if _, failed, _ := attempt.Outcome(); failed {
			t.Fatal("the inner scope kept and the outer dropped: the failure reached the attempt")
		}
	})
}

// A held scope reached GitHub once a request was sent in it or in a scope it encloses, or once a
// failure reached it, with no request too (a limit already recorded is noted though nothing was
// sent): a request sent in a nested scope marks every scope around it, a failure an inner scope
// holds reaches the outer one only when the inner keeps it, and neither a request sent outside
// the scope nor a note made after the attempt's first failure marks it. A nil scope reached
// nothing.
func TestAHeldScopeReachedGitHubBySendingOrByAFailure(t *testing.T) {
	failure := errors.New("GitHub rate limit on installation:7 recorded at 2026-11-18T08:00:00Z is in force; the request was not sent")
	t.Run("a request sent in a nested scope", func(t *testing.T) {
		ctx, _ := Begin(context.Background(), CIReconcile)
		outer, outerScope := Hold(ctx)
		inner, innerScope := Hold(outer)
		_, sibling := Hold(outer)
		NoteSent(ctx)
		if outerScope.Reached() || innerScope.Reached() {
			t.Fatalf("a request sent outside both scopes: outer reached %v, inner %v; want neither", outerScope.Reached(), innerScope.Reached())
		}
		NoteSent(inner)
		if !innerScope.Reached() || !outerScope.Reached() || sibling.Reached() {
			t.Fatalf("a request sent in the inner scope: inner reached %v, outer %v, the inner's sibling %v; want the inner and the outer alone",
				innerScope.Reached(), outerScope.Reached(), sibling.Reached())
		}
	})
	t.Run("a failure with no request", func(t *testing.T) {
		ctx, _ := Begin(context.Background(), CIReconcile)
		outer, outerScope := Hold(ctx)
		inner, innerScope := Hold(outer)
		NoteFailure(inner, failure)
		if !innerScope.Reached() || outerScope.Reached() {
			t.Fatalf("a failure held in the inner scope: inner reached %v, outer %v; want the inner alone", innerScope.Reached(), outerScope.Reached())
		}
		innerScope.Keep()
		if !outerScope.Reached() {
			t.Fatal("the inner scope kept its failure: the outer scope did not reach GitHub")
		}

		ctx, _ = Begin(context.Background(), CIReconcile)
		outer, outerScope = Hold(ctx)
		inner, innerScope = Hold(outer)
		NoteFailure(inner, failure)
		innerScope.Drop()
		if outerScope.Reached() {
			t.Fatal("the inner scope dropped its failure: the outer scope reached GitHub")
		}
	})
	t.Run("a note after the attempt's first failure", func(t *testing.T) {
		ctx, _ := Begin(context.Background(), CIReconcile)
		NoteFailure(ctx, failure)
		held, scope := Hold(ctx)
		NoteFailure(held, errors.New("a later failure"))
		if scope.Reached() {
			t.Fatal("a note made after the attempt's first failure reached the scope")
		}
	})
	t.Run("nothing", func(t *testing.T) {
		ctx, _ := Begin(context.Background(), CIReconcile)
		if _, scope := Hold(ctx); scope.Reached() {
			t.Fatal("a scope nothing was sent or noted in reached GitHub")
		}
		if _, scope := Hold(context.Background()); scope != nil || scope.Reached() {
			t.Fatalf("Hold on a context with no attempt answered %v, reached %v; want a nil scope that reached nothing", scope, scope.Reached())
		}
	})
}

// countedError is an error that counts the calls of its Error(), so a test sees whether a note
// made its text.
type countedError struct{ calls int }

func (e *countedError) Error() string {
	e.calls++
	return "a failure after the cause"
}

// A note made once the attempt holds its first failure can never be the cause: it leaves the
// cause as it was and makes no text, in a held scope or out of one, and NoteFailuref formats
// nothing then, nor on a ctx with no attempt. Nor does a held scope make the text of a note it
// forgets: one after the failure it holds, or one after it was dropped. Where its note can still
// be the cause, NoteFailuref notes the text fmt.Errorf makes.
func TestANoteThatCannotBeTheCauseMakesNoText(t *testing.T) {
	const first = "GET https://api.github.com/app/hook/deliveries: status 503: first"
	t.Run("outside a held scope", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		NoteFailure(ctx, errors.New(first))
		late := &countedError{}
		NoteFailure(ctx, late)
		NoteFailuref(ctx, "a step: %w", late)
		if _, failed, cause := attempt.Outcome(); !failed || cause != first || late.calls != 0 {
			t.Fatalf("failed %v, cause %q, the late note's Error() called %d times; want the first cause and no call", failed, cause, late.calls)
		}
	})
	t.Run("inside a held scope", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		held, scope := Hold(ctx)
		NoteFailure(ctx, errors.New(first))
		late := &countedError{}
		NoteFailure(held, late)
		NoteFailuref(held, "a step: %w", late)
		scope.Keep()
		if _, failed, cause := attempt.Outcome(); !failed || cause != first || late.calls != 0 {
			t.Fatalf("failed %v, cause %q, the late note's Error() called %d times; want the first cause and no call", failed, cause, late.calls)
		}
	})
	t.Run("a note a held scope forgets", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		held, scope := Hold(ctx)
		NoteFailure(held, errors.New(first))
		late := &countedError{}
		NoteFailure(held, late)
		scope.Drop()
		NoteFailure(held, late)
		if _, failed, _ := attempt.Outcome(); failed || late.calls != 0 {
			t.Fatalf("failed %v, the forgotten notes' Error() called %d times; want no failure and no call", failed, late.calls)
		}
	})
	t.Run("NoteFailuref on a ctx with no attempt", func(t *testing.T) {
		late := &countedError{}
		NoteFailuref(context.Background(), "a step: %w", late)
		if late.calls != 0 {
			t.Fatalf("the note's Error() called %d times on a ctx with no attempt; want none", late.calls)
		}
	})
	t.Run("NoteFailuref where its note can be the cause", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), CIReconcile)
		NoteFailuref(ctx, "re-read the head of %s PR #%d: %w", "acme/web", 7, errors.New("status 502"))
		if _, failed, cause := attempt.Outcome(); !failed || cause != "re-read the head of acme/web PR #7: status 502" {
			t.Fatalf("failed %v, cause %q; want the formatted note", failed, cause)
		}
	})
}

// recorded is one call of a test's Recorder.
type recorded struct {
	read      Read
	succeeded bool
	cause     string
	deadline  bool
}

// Finish records nothing without a recorder, for a caller that ended, or for an attempt that
// sent nothing and noted nothing; otherwise it records failed with the first cause, or succeeded.
// The write has a bound of its own, and one that fails is logged at ERROR and changes nothing
// else.
func TestFinishRecordsTheAttemptsOutcome(t *testing.T) {
	failure := errors.New("GET https://api.github.com/app/hook/deliveries: status 503: unavailable")
	cases := []struct {
		name   string
		note   func(ctx context.Context)
		ended  bool
		record bool
		want   *recorded
	}{{
		name:   "sent and nothing failed",
		note:   NoteSent,
		record: true,
		want:   &recorded{read: CIReconcile, succeeded: true, deadline: true},
	}, {
		name:   "sent and a failure noted",
		note:   func(ctx context.Context) { NoteSent(ctx); NoteFailure(ctx, failure) },
		record: true,
		want:   &recorded{read: CIReconcile, cause: failure.Error(), deadline: true},
	}, {
		// A request a recorded limit held is noted though nothing was sent.
		name:   "a failure noted with nothing sent",
		note:   func(ctx context.Context) { NoteFailure(ctx, failure) },
		record: true,
		want:   &recorded{read: CIReconcile, cause: failure.Error(), deadline: true},
	}, {
		name:   "nothing sent and nothing noted",
		note:   func(context.Context) {},
		record: true,
	}, {
		name:   "a caller that ended",
		note:   func(ctx context.Context) { NoteSent(ctx); NoteFailure(ctx, failure) },
		ended:  true,
		record: true,
	}, {
		name: "no recorder",
		note: func(ctx context.Context) { NoteSent(ctx); NoteFailure(ctx, failure) },
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller, end := context.WithCancel(context.Background())
			defer end()
			ctx, attempt := Begin(caller, CIReconcile)
			tc.note(ctx)
			if tc.ended {
				end()
			}
			var calls []recorded
			var record Recorder
			if tc.record {
				record = func(ctx context.Context, read Read, succeeded bool, cause string) error {
					deadline, ok := ctx.Deadline()
					calls = append(calls, recorded{read: read, succeeded: succeeded, cause: cause, deadline: ok && time.Until(deadline) <= recordTimeout})
					return ctx.Err()
				}
			}
			Finish(caller, attempt, record, nil)
			switch {
			case tc.want == nil && len(calls) != 0:
				t.Fatalf("recorded %+v, want nothing", calls)
			case tc.want != nil && (len(calls) != 1 || calls[0] != *tc.want):
				t.Fatalf("recorded %+v, want exactly %+v", calls, *tc.want)
			}
		})
	}

	t.Run("a recorder that fails", func(t *testing.T) {
		ctx, attempt := Begin(context.Background(), WebhookRedelivery)
		NoteSent(ctx)
		logs := &logtest.Recorder{}
		refused := errors.New("the database refused the write")
		Finish(context.Background(), attempt, func(context.Context, Read, bool, string) error { return refused }, slog.New(logs))
		records := logs.Records()
		if len(records) != 1 || records[0].Level != slog.LevelError {
			t.Fatalf("logged %d records (%v), want one at ERROR", len(records), records)
		}
		attributes := map[string]string{}
		records[0].Attrs(func(attribute slog.Attr) bool {
			attributes[attribute.Key] = attribute.Value.String()
			return true
		})
		if attributes["read"] != string(WebhookRedelivery) || attributes["error"] != refused.Error() {
			t.Fatalf("the ERROR line carries %v, want the read and the error", attributes)
		}
	})
}

// One Begin names a pass's read, and that one naming is the caller kind of every call the pass
// sends (githubrate.CallerOf), which the rate reading counts it under, and the read Finish records
// the attempt under: a pass cannot count under one read and be recorded under another.
func TestBeginNamesThePassesCallerKindAndTheReadFinishRecords(t *testing.T) {
	for _, read := range Reads() {
		ctx, attempt := Begin(context.Background(), read)
		if kind := githubrate.CallerOf(ctx); kind != string(read) {
			t.Errorf("a pass of %s calls GitHub as %q, want %q", read, kind, read)
		}
		held, _ := Hold(ctx)
		if kind := githubrate.CallerOf(held); kind != string(read) {
			t.Errorf("a held try of %s calls GitHub as %q, want %q", read, kind, read)
		}
		NoteSent(ctx)
		var recordedRead Read
		Finish(context.Background(), attempt, func(_ context.Context, read Read, _ bool, _ string) error {
			recordedRead = read
			return nil
		}, nil)
		if recordedRead != read {
			t.Errorf("a pass begun as %s was recorded as %q", read, recordedRead)
		}
	}
}

// loadAnswer is what one of the test's loads of the record answers.
type loadAnswer struct {
	rows []Recorded
	err  error
}

// The copy loads the record at start and again after each wake, each load under its own bound.
// It holds nothing until a load succeeds, and a failed load is logged at WARN and keeps the copy
// the last good load made.
func TestCopyLoadsAtStartAndAfterAWakeAndKeepsItsCopyOnAFailedLoad(t *testing.T) {
	var copied Copy
	if rows, loaded := copied.Rows(); loaded || rows != nil {
		t.Fatalf("the zero Copy holds %v, loaded %v; want nothing", rows, loaded)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	calls := make(chan bool)
	answers := make(chan loadAnswer)
	load := func(ctx context.Context) ([]Recorded, error) {
		deadline, ok := ctx.Deadline()
		calls <- ok && time.Until(deadline) <= loadTimeout
		answer := <-answers
		return answer.rows, answer.err
	}
	wake := make(chan struct{}, 1)
	logs := &logtest.Recorder{}
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		copied.Run(ctx, load, wake, slog.New(logs))
	}()
	// started waits for the next load, which begins only after the one before it has ended.
	started := func(what string) {
		t.Helper()
		select {
		case bounded := <-calls:
			if !bounded {
				t.Fatalf("%s: the load has no bound of %s", what, loadTimeout)
			}
		case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
			t.Fatalf("%s: no load started", what)
		}
	}
	warnings := func() int {
		n := 0
		for _, record := range logs.Records() {
			if record.Level == slog.LevelWarn {
				n++
			}
		}
		return n
	}
	first := Recorded{Read: WebhookRedelivery, At: time.Unix(1_800_000_000, 0).UTC(), Cause: "GET …: status 503: unavailable"}
	second := Recorded{Read: WebhookRedelivery, At: first.At.Add(time.Minute), Succeeded: true}
	holds := func(want Recorded) func() bool {
		return func() bool {
			rows, loaded := copied.Rows()
			return loaded && len(rows) == 1 && rows[want.Read] == want
		}
	}

	started("the load at start")
	answers <- loadAnswer{err: errors.New("the database is down")}
	wake <- struct{}{}
	started("the load after the first wake")
	if rows, loaded := copied.Rows(); loaded || rows != nil {
		t.Fatalf("after a failed first load the copy holds %v, loaded %v; want nothing", rows, loaded)
	}
	if got := warnings(); got != 1 {
		t.Fatalf("a failed load logged %d WARN lines, want 1", got)
	}
	answers <- loadAnswer{rows: []Recorded{first}}
	testwait.Eventually(t, "the copy to hold the first rows", holds(first))

	wake <- struct{}{}
	started("the load after the second wake")
	answers <- loadAnswer{err: errors.New("the database is down again")}
	wake <- struct{}{}
	started("the load after the third wake")
	if !holds(first)() {
		rows, loaded := copied.Rows()
		t.Fatalf("after a failed load the copy holds %v, loaded %v; want the rows of the load before it", rows, loaded)
	}
	if got := warnings(); got != 2 {
		t.Fatalf("two failed loads logged %d WARN lines, want 2", got)
	}
	answers <- loadAnswer{rows: []Recorded{second}}
	testwait.Eventually(t, "the copy to hold the newest rows", holds(second))

	stop()
	select {
	case <-ran:
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("Run did not return once its context ended")
	}
}
