package onepassword

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// The requests each call of Client spends of the account's daily limit. They are provisional:
// how many requests one SDK List or GetAll costs is not measured, and the live run on the test
// vault measures them with `op service-account ratelimit` before and after each call.
const (
	ListRequests    = 1
	CreateRequests  = 1
	PutRequests     = 1
	ArchiveRequests = 1
)

// GetRequests is the requests one Get of n items spends.
func GetRequests(n int) int { return 1 }

// The calls a store spends its Budget for.
const (
	// callList is a call of the store's List.
	callList = "list"
	// callDescribe is a call of the store's Describe.
	callDescribe = "describe"
	// callRead is a call of a value read, which a Budget never holds.
	callRead = "read"
	// callFirst is a List's or a Describe's call that the budget holds, on a store that holds no
	// earlier result to answer instead.
	callFirst = "first"
)

// Budget is the deployment's daily budget of 1Password requests, which the store spends from
// before each call it makes.
type Budget interface {
	// Spend counts n requests of call and answers false, counting nothing, when call may hold and
	// the moving 24 hours plus n would pass limit - limit/5. A "read" call always spends. A "first"
	// call - a List or Describe on a store that holds no earlier result to answer, such as one that
	// starts during a hold - spends from the reserve up to limit itself, and past limit answers
	// ErrBudgetSpent, counting nothing.
	Spend(ctx context.Context, call string, n int) (spent bool, err error)
}

// Unlimited is the Budget that never holds: it spends every call and counts nothing.
var Unlimited Budget = unlimited{}

type unlimited struct{}

func (unlimited) Spend(context.Context, string, int) (bool, error) { return true, nil }

// ErrBudgetSpent is a List or a Describe the budget refused on a store that holds no earlier
// result: the whole day's budget is spent. The store answers it as a store error, so the policy
// keeps what it serves, and a boot's first load fails as it does on any store failure.
var ErrBudgetSpent = errors.New("the 1Password request budget is spent and this replica holds no earlier result; it calls 1Password again once the window has room")

// The lines a budget writes.
const (
	// heldMessage, at WARN, once when a replica's List or Describe first holds: call, spent (the
	// count of the moving 24 hours), budget and reserve.
	heldMessage = "onepassword requests held"
	// resumedMessage, at INFO, when a replica's List or Describe calls 1Password again after a
	// hold.
	resumedMessage = "onepassword requests resumed"
	// passedMessage, at WARN, at most once an hour, when a value read spends past the whole
	// budget: spent and budget.
	passedMessage = "onepassword budget passed by value reads"
)

// budgetLock is the advisory lock every replica's spend takes, so two replicas read and add to the
// count one at a time. store.Migrate's lock is 8330001, and enroll's serving lock 8330019.
const budgetLock int64 = 8330042

// budgetKeep is how long a row of the count is kept: two days, so the moving 24 hours always
// reads from rows a spend has not yet deleted.
const budgetKeep = 48 * time.Hour

// pgBudget is the count of onepassword_requests in the broker's Postgres, which every replica of
// the deployment shares.
type pgBudget struct {
	db    secretstore.Database
	limit int
	now   func() time.Time
	log   *slog.Logger

	mu sync.Mutex
	// holding is whether this replica's last List or Describe held.
	holding bool
	// passedAt is when this replica last logged passedMessage.
	passedAt time.Time
}

// NewBudget is the budget of limit requests a moving 24 hours, counted in onepassword_requests of
// db by the hour of now (time.Now where nil), logging to log (slog.Default where nil). Each spend
// is one transaction: it takes budgetLock, reads the sum of the last 24 hourly rows, the current
// one included, and, where it spends, adds n to the current hour's row and deletes the rows past
// budgetKeep.
func NewBudget(db secretstore.Database, limit int, now func() time.Time, log *slog.Logger) Budget {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &pgBudget{db: db, limit: limit, now: now, log: log}
}

// Spend counts n requests of call, as Budget says.
func (b *pgBudget) Spend(ctx context.Context, call string, n int) (bool, error) {
	at := b.now().UTC()
	hour := at.Truncate(time.Hour)
	reserve := b.limit / 5
	var spent int
	paid := false
	err := b.db.InTx(ctx, func(tx secretstore.Tx) error {
		if err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, budgetLock); err != nil {
			return fmt.Errorf("lock the 1Password request count: %w", err)
		}
		if err := tx.QueryRow(ctx, `select coalesce(sum(requests), 0)::bigint from onepassword_requests where hour > $1`, hour.Add(-24*time.Hour)).Scan(&spent); err != nil {
			return fmt.Errorf("read the 1Password request count: %w", err)
		}
		switch call {
		case callRead:
		case callFirst:
			if spent+n > b.limit {
				return ErrBudgetSpent
			}
		default:
			if spent+n > b.limit-reserve {
				return nil
			}
		}
		if err := tx.Exec(ctx, `insert into onepassword_requests (hour, requests) values ($1, $2)
			on conflict (hour) do update set requests = onepassword_requests.requests + excluded.requests`, hour, n); err != nil {
			return fmt.Errorf("add to the 1Password request count: %w", err)
		}
		if err := tx.Exec(ctx, `delete from onepassword_requests where hour < $1`, hour.Add(-budgetKeep)); err != nil {
			return fmt.Errorf("delete old 1Password request counts: %w", err)
		}
		paid = true
		return nil
	})
	switch {
	case errors.Is(err, ErrBudgetSpent):
		return false, err
	case err != nil:
		return false, fmt.Errorf("count 1Password requests: %w", err)
	case !paid:
		b.held(call, spent, reserve)
		return false, nil
	}
	b.spent(call, spent+n, at)
	return true, nil
}

// held logs the start of a hold.
func (b *pgBudget) held(call string, spent, reserve int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holding {
		return
	}
	b.holding = true
	b.log.Warn(heldMessage, "call", call, "spent", spent, "budget", b.limit, "reserve", reserve)
}

// spent logs the end of a hold, at a List or Describe that spent, and a value read's spend past the
// budget, once an hour.
func (b *pgBudget) spent(call string, total int, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case (call == callList || call == callDescribe) && b.holding:
		b.holding = false
		b.log.Info(resumedMessage, "call", call, "spent", total, "budget", b.limit)
	case call == callRead && total > b.limit && (b.passedAt.IsZero() || at.Sub(b.passedAt) >= time.Hour):
		b.passedAt = at
		b.log.Warn(passedMessage, "spent", total, "budget", b.limit)
	}
}
