package identity

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// lateActive is the people store with the write of each Active outcome held until release is
// called: entered closes as the write is reached, after the check's Directory read has answered.
// The write runs without the check's 15-second deadline, so how long the test holds it decides
// only the order of the writes, never whether this one misses its deadline.
type lateActive struct {
	*store.PgPeopleStore
	entered chan struct{}
	gate    chan struct{}
}

func (s lateActive) RecordDirectoryActive(ctx context.Context, email, userID string, readAt time.Time) (time.Time, error) {
	close(s.entered)
	<-s.gate
	return s.PgPeopleStore.RecordDirectoryActive(context.WithoutCancel(ctx), email, userID, readAt)
}

// TestIAPActiveAnswerFromAnEarlierReadReleasesNoLaterHold: two Dispatch processes check ada's
// account at once. Process A reads her row and the Directory answers Active; then Google suspends
// her, and process B reads her row, the Directory answers suspended, and B records Held. A's
// Active outcome is written only after B's Held: it rests on a Directory read older than the
// hold, so it writes nothing, ada stays held, and A counts RefreshHeld, what she is. The order
// control writes A's Active outcome before Google suspends her: A counts RefreshConfirmed, and B's
// Held then holds her, so the test fails only on the order of the writes.
//
// The same holds for a row that was already held when A read it: ada was suspended an hour ago,
// her account was restored before A's Directory read, and she is suspended again before B's. B's
// Held stamps the hold with its own time, after A's read, so A's late Active outcome writes nothing
// there either.
//
// Controls: clear held_since in the Active write whatever the time of the read it rests on; the
// first case fails, naming the row A's write released. Keep a held row's first held_since on a later
// Held outcome; the re-held case fails the same way.
func TestIAPActiveAnswerFromAnEarlierReadReleasesNoLaterHold(t *testing.T) {
	for _, c := range []struct {
		name        string
		heldBefore  bool
		activeFirst bool
		want        RefreshOutcome
	}{
		{"an Active outcome written after a later read's Held", false, false, RefreshHeld},
		{"order control: the Active outcome written before Google suspends her", false, true, RefreshConfirmed},
		{"a row held before A's read: an Active outcome written after a later read's Held", true, false, RefreshHeld},
		{"a row held before A's read, order control: the Active outcome written before Google suspends her again", true, true, RefreshConfirmed},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			r := newDirectoryStandingRig(t)
			adaActive(r.google)
			r.record(directoryAda)
			r.active(directoryAda, "u-ada", 2*time.Hour)
			if c.heldBefore {
				// Suspended an hour ago, and restored since: the Directory answers her Active again.
				r.held(directoryAda)
				r.age(directoryAda, time.Hour)
				if answer := r.standing(directoryAda); !answer.Held {
					t.Fatalf("ada before A's check = %+v; want held", answer)
				}
			}
			reader, err := NewDirectoryReader(t.Context(), directorySigner, directoryAdmin, r.google.ClientOptions()...)
			if err != nil {
				t.Fatalf("NewDirectoryReader: %v", err)
			}
			late := lateActive{PgPeopleStore: r.people, entered: make(chan struct{}), gate: make(chan struct{})}
			release := sync.OnceFunc(func() { close(late.gate) })
			a := &Membership{People: late, Rule: workspace, Directory: reader, Outliving: r.outliving}
			type result struct {
				outcome RefreshOutcome
				err     error
			}
			done := make(chan result, 1)
			go func() {
				outcome, err := a.ConfirmDirectory(ctx, directoryAda)
				done <- result{outcome, err}
			}()
			// A failure below leaves A's write held: release it and wait for it before the
			// rig's database is dropped.
			var got result
			waited := false
			t.Cleanup(func() {
				release()
				if !waited {
					<-done
				}
			})
			<-late.entered
			if c.activeFirst {
				release()
				got, waited = <-done, true
			}
			r.google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda, Suspended: true})
			r.newProcess()
			r.age(directoryAda, 2*time.Hour)
			if outcome, err := r.membership.ConfirmDirectory(ctx, directoryAda); err != nil || outcome != RefreshHeld {
				t.Fatalf("B's check of the suspended account = %v, %v; want RefreshHeld", outcome, err)
			}
			if answer := r.standing(directoryAda); !answer.Held {
				t.Fatalf("after B's Held outcome Standing = %+v; want held", answer)
			}
			if !c.activeFirst {
				release()
				got, waited = <-done, true
			}
			if got.err != nil || got.outcome != c.want {
				t.Errorf("A's check counted %v, %v; want %v", got.outcome, got.err, c.want)
			}
			row := r.row(directoryAda)
			r.newProcess()
			if answer := r.standing(directoryAda); !answer.Held {
				t.Fatalf("a Held outcome from the later Directory read was released by an Active outcome from an earlier one: "+
					"Standing = %+v; held_since %s, directory_confirmed_at %s, not_held_until %s",
					answer, stamp(row.heldSince), stamp(row.confirmedAt), stamp(row.notHeldUntil))
			}
		})
	}
}

// TestIAPMembershipPassCountsAnUnwrittenActiveHeld: the membership pass claims ada's due row and
// checks it, and the Directory answers Active; before its write lands, Google suspends her and
// another process's check records Held. The pass's Active write writes nothing, ada stays held,
// and the pass's line counts her held, not confirmed.
//
// Control: count an Active outcome that wrote nothing as RefreshNone, which the pass counts
// confirmed; the line reads confirmed=1 held=0, and the test fails naming it.
func TestIAPMembershipPassCountsAnUnwrittenActiveHeld(t *testing.T) {
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	ctx := context.Background()
	r := newDirectoryStandingRig(t)
	adaActive(r.google)
	r.record(directoryAda)
	r.active(directoryAda, "u-ada", 2*time.Hour)
	reader, err := NewDirectoryReader(t.Context(), directorySigner, directoryAdmin, r.google.ClientOptions()...)
	if err != nil {
		t.Fatalf("NewDirectoryReader: %v", err)
	}
	late := lateActive{PgPeopleStore: r.people, entered: make(chan struct{}), gate: make(chan struct{})}
	release := sync.OnceFunc(func() { close(late.gate) })
	pass := MembershipPass{Membership: &Membership{People: late, Rule: workspace, Directory: reader, Outliving: r.outliving}, People: r.people}
	ran := make(chan error, 1)
	go func() { ran <- pass.Run(ctx) }()
	// A failure below leaves the pass's write held: release it and wait for the pass before the
	// rig's database is dropped.
	finished := false
	t.Cleanup(func() {
		release()
		if !finished {
			<-ran
		}
	})
	<-late.entered
	r.google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda, Suspended: true})
	r.newProcess()
	if outcome, err := r.membership.ConfirmDirectory(ctx, directoryAda); err != nil || outcome != RefreshHeld {
		t.Fatalf("the other process's check of the suspended account = %v, %v; want RefreshHeld", outcome, err)
	}
	release()
	err = <-ran
	finished = true
	if err != nil {
		t.Fatalf("the pass: %v", err)
	}
	if answer := r.standing(directoryAda); !answer.Held {
		t.Fatalf("ada after the pass's late Active outcome = %+v; want held", answer)
	}
	var line string
	for candidate := range strings.Lines(logged.String()) {
		if strings.Contains(candidate, `msg="dispatch: membership pass"`) {
			line = strings.TrimSpace(candidate)
		}
	}
	if !strings.Contains(line, "confirmed=0 held=1 unasked=0 forgotten=0") {
		t.Fatalf("the pass's line is %q; want ada counted held=1 and confirmed=0: her hold stood", line)
	}
}
