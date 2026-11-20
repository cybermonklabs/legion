package duty_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// sentRecorder answers every statement of a hold as one that affected the hold's row, and keeps
// the clock reading each take and renewal carried (duty.SentAt). A renewal waits until renew is
// closed, or until its own context ends unanswered (opened), so no renewal moves the take's
// deadline before the test has read it, however late the test runs after the take.
type sentRecorder struct {
	renew chan struct{}

	mu      sync.Mutex
	take    []time.Time
	renewed []time.Time
	// none counts the takes and renewals that carried no reading.
	none int
}

func (r *sentRecorder) Exec(ctx context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	take := strings.HasPrefix(sql, "insert into duty_holds")
	renewal := strings.HasPrefix(sql, "update duty_holds set expires_at")
	if renewal && !r.opened(ctx) {
		return pgconn.CommandTag{}, ctx.Err()
	}
	sent, ok := duty.SentAt(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !take && !renewal:
	case !ok:
		r.none++
	case take:
		r.take = append(r.take, sent)
	default:
		r.renewed = append(r.renewed, sent)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// opened waits until renew is closed, and reports false when ctx ends first. A renewal that comes
// once renew is closed is answered, whatever its context says.
func (r *sentRecorder) opened(ctx context.Context) bool {
	select {
	case <-r.renew:
		return true
	default:
	}
	select {
	case <-r.renew:
		return true
	case <-ctx.Done():
		return false
	}
}

func (*sentRecorder) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

// The take and each renewal carry the clock reading the hold took before it sent them, and the
// loss deadline is exactly that reading plus TTL - Margin: a tracer of the hold's pool ties each
// deadline to its statement with no second reading between them.
func TestEachStatementOfAHoldCarriesTheReadingItsDeadlineIsMadeOf(t *testing.T) {
	opts := duty.HoldOptions{TTL: 10 * time.Second, Renew: 100 * time.Millisecond, Margin: time.Second}
	db := &sentRecorder{renew: make(chan struct{})}
	hold, err := duty.TryHold(context.Background(), db, "sent-at", uuid.New(), opts)
	if err != nil || hold == nil {
		t.Fatalf("take a hold: %v", err)
	}
	defer func() { _ = hold.Release(context.Background()) }()
	db.mu.Lock()
	take := slices.Clone(db.take)
	db.mu.Unlock()
	if len(take) != 1 || !take[0].Add(opts.TTL-opts.Margin).Equal(hold.Deadline()) {
		t.Fatalf("the take carried the readings %v and the deadline is %s, want one reading, plus TTL - Margin the deadline", take, hold.Deadline())
	}
	close(db.renew)
	deadline := time.Now().Add(5 * time.Second)
	for {
		db.mu.Lock()
		renewed, none := slices.Clone(db.renewed), db.none
		db.mu.Unlock()
		if none != 0 {
			t.Fatalf("%d takes or renewals of the hold carried no reading", none)
		}
		if n := len(renewed); n >= 3 && renewed[n-1].Add(opts.TTL-opts.Margin).Equal(hold.Deadline()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %d renewals carried the readings %v the deadline is %s, want the last reading plus TTL - Margin", len(renewed), renewed, hold.Deadline())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
