package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A session's clock can step backwards within one launch: a host corrected after it ran ahead,
// a VM resumed on a stale clock. Every reading the session takes after the step is dated before
// the one recorded, and the ordering guard, were it to compare the session's times alone, would
// drop each of them until the session's clock passed the recorded time again, while the reading
// it left behind stood as current. A reading dated 10 minutes before the recorded one that the
// daemon received later is no earlier reading overtaken in flight, so it is written, and the
// guard goes on ordering the session's readings by its clock as it now runs.
func TestAnActivityReportAfterTheSessionClockSteppedBackIsWritten(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-213-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	before := claim.SessionActivity{At: at(30), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, before, at(30)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report before the step = %t, %v", known, err)
	}

	// The session's clock steps back 10 minutes; its next reading, that the job has ended, reaches
	// the daemon 30 seconds after the one before.
	received := at(30).Add(30 * time.Second)
	stepped := claim.SessionActivity{At: at(20)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, stepped, received); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report after the step = %t, %v", known, err)
	}
	got, background, kinds, _ := storedActivity(t, store, c.Token)
	if sent := storedSentAt(t, store, c.Token); !got.Equal(received) || !sent.Equal(stepped.At) || background || kinds != "" {
		t.Fatalf("after a report dated 10 minutes before the recorded one and received 30 seconds after it, the row reads "+
			"received %s, sent %s, background %t, kinds %q; want the report received at %s and sent at %s written, nothing running",
			got, sent, background, kinds, received, stepped.At)
	}

	// The session's next reading, a minute on by its clock as it now runs, is written.
	next := claim.SessionActivity{At: at(21), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, next, at(31).Add(30*time.Second)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the next report = %t, %v", known, err)
	}
	// A reading taken 30 seconds before that one, which the daemon receives a second after it, is
	// an earlier reading overtaken in flight on the same clock, and is dropped.
	overtaken := claim.SessionActivity{At: at(20).Add(30 * time.Second)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, overtaken, at(31).Add(31*time.Second)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the overtaken report = %t, %v", known, err)
	}
	_, background, kinds, _ = storedActivity(t, store, c.Token)
	if sent := storedSentAt(t, store, c.Token); !sent.Equal(next.At) || !background || kinds != "async-job" {
		t.Fatalf("after an earlier reading of the stepped clock arrived late the row reads sent %s, background %t, kinds %q; "+
			"want the reading sent at %s kept", sent, background, kinds, next.At)
	}
}
