package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The claims row keeps one session per claim, so a claim write that moves a claim off a session it
// recorded leaves that session behind: after the write no claims.session names it. The rows of a
// move out of the workflow and of the boot pass (the outbox's releaseClaim) act only on an issue
// whose record says it stopped (record.Stopped). While the issue runs, neither row releases the
// Dispatch claim that the left session still holds on it, and the claim's new session cannot claim
// the issue while Dispatch holds it for the left session (R5 of dispatch://INFRA-3144). So
// once the daemon has turned the release on (ReleaseLeftSessionClaims), that write queues, in its
// own transaction (putClaim), where every write of a claim's session converges, one claim_release
// row keyed by the claim's issue whose payload names the left session: the move and the release it
// owes commit together or not at all. The write that moves the session is the one place that still
// knows the session it moves off, whichever path makes it — a fresh session that clears it, or a
// new session registered over it — so the row does not depend on the claim's record of the session
// it abandoned.
//
// The row is keyed by the issue, so it is queued only for a claim on an issue (the controller's
// claim is on none) of the claim's own project (claim.KeyOfProject): only that project's daemon
// runs a row keyed by its issue (queuesLeftSessionRelease).

// claimReleaseKind is record.OutboxKindClaimRelease. This package cannot import record, whose
// tests import this package, so the name is written here once, and left_sessions_test.go reads
// every row this package queues back through record's decoder, which holds the two to one.
const claimReleaseKind = "claim_release"

// claimReleasePayload is the payload of that row, the shape of record.ClaimRelease when it names
// the session the daemon left behind.
type claimReleasePayload struct {
	Session string `json:"session"`
}

// ReleaseLeftSessionClaims turns the release of a left session's claim on: from this call on, each
// claim write that moves a claim of an issue of the claim's own project off a session the stored
// row names queues one claim_release row of that issue, whose payload names that session, in its
// own transaction (queuesLeftSessionRelease, putClaim), and woken is told the issue once the
// transaction commits, where the daemon wakes the issue's claim lane so the row waits for no
// sweep. A transaction that rolls back tells nothing, since it queued nothing.
//
// Until it is called the store queues nothing. Only a daemon with a workflow runs an outbox to
// send the row, and only that daemon calls this, as it calls PublishRoleHolders, which says why a
// row queued anywhere else would be neither sent nor deleted.
func (s *Store) ReleaseLeftSessionClaims(woken func(issue string)) {
	if woken == nil {
		woken = func(string) {}
	}
	s.leftSessionsQueued.Store(&woken)
}

// queuesLeftSessionRelease is whether a write of claim c can queue the release of a session it
// leaves behind at all: the daemon has turned the release on (ReleaseLeftSessionClaims), c is on
// an issue, which the controller's claim is not, and that issue is of c's own project
// (claim.KeyOfProject), whose row of the issue this daemon's outbox runs. None depends on the
// stored row, so putClaim reads it before it locks that row, and a write for which it is false
// neither reads the stored session for it nor writes the release's row.
func (s *Store) queuesLeftSessionRelease(c supervise.Claim) bool {
	return s.leftSessionsQueued.Load() != nil && c.Issue != "" && claim.KeyOfProject(c.Project, c.Issue)
}

// enqueueLeftSessionRelease writes, in tx, one claim_release row of issue, due at nextAt, whose
// payload names session, the session a claim write left behind. It is a plain insert, not an
// upsert: each session left behind may hold a Dispatch claim of its own to release, so two moves
// before the lane runs queue two rows.
func enqueueLeftSessionRelease(ctx context.Context, tx pgx.Tx, issue, session string, nextAt time.Time) error {
	payload, err := json.Marshal(claimReleasePayload{Session: session})
	if err != nil {
		return fmt.Errorf("enqueue the claim release of %s's left session %s: %w", issue, session, err)
	}
	if _, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3::jsonb, 0, $4, '')`,
		claimReleaseKind, issue, string(payload), nextAt); err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", claimReleaseKind, issue, err)
	}
	return nil
}

// wakeLeftSessionRelease tells the daemon's wake (ReleaseLeftSessionClaims) issue, whose release
// of a left session a claim write queued, once that write's transaction has committed: the hook
// putClaim registers, the one wake of that row.
func (s *Store) wakeLeftSessionRelease(issue string) {
	if woken := s.leftSessionsQueued.Load(); woken != nil {
		(*woken)(issue)
	}
}
