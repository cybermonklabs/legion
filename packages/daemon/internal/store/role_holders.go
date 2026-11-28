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

// The publication of an issue's role sessions to Dispatch is an outbox row the daemon's board lane
// runs (record.RoleHoldersPublish, PUT /api/v1/issues/{key}/role-holders), and it reads from the
// claims when it runs the set of sessions whose claims hold their roles (supervise.HoldsRole): a
// claim the idle release suspended is in it. Once the daemon has turned the publication on
// (PublishRoleHolders), the claim write that changes the session holding a role queues it in that
// write's own transaction (putClaim), where every write of a claim's state and session converges,
// so the change and the obligation to publish it commit together or not at all: no change of a
// holder stands that nothing owes a publication, and a write the store refused leaves neither. An
// end whose own write failed with an error the machine owes is published when the repair that
// makes it good lands, whichever decision makes it. An end whose failed write is not owed is
// written again only when a later decision retires a task the claim still holds, which writes the
// claim with it; until then the store, and so Dispatch's set, keeps the claim holding its role
// (dispatch://INFRA-4099).
//
// The row is keyed by the issue, so it is queued only for an issue of the claim's own project
// (claim.KeyOfProject): a claim of another project's issue, which the operator's spawn makes under
// this project's token, queues none, since only that project's daemon runs a row keyed by its
// issue, against its own claims (queuesRoleHolders, EnqueueRoleHolders).

// roleHoldersPublishKind is record.OutboxKindRoleHoldersPublish. This package cannot import record,
// whose tests import this package, so the name is written here once, and role_holders_test.go reads
// every row this package queues back through record's decoder, which holds the two to one.
const roleHoldersPublishKind = "role_holders_publish"

// roleHoldersPublishPayload is the payload of that row, record.RoleHoldersPublish's shape: the
// issue alone, since the executor reads the set when the row runs.
type roleHoldersPublishPayload struct {
	Issue string `json:"issue"`
}

// holdsIssueRole is whether c's session can hold a role of an issue at all: a claim on no issue
// holds none, and neither does the project's controller.
func holdsIssueRole(c supervise.Claim) bool {
	return c.Issue != "" && c.Role != claim.RoleController
}

// roleHolder is the session a claims row names as the holder of its role: its session while its
// state and suspension source hold the role (supervise.HoldsRole), "" otherwise. It is the row's
// part of what a publication sends (record.Postgres.RoleHolders, which reads the same rule), so a
// write that moves the claim between two states that both hold the role, such as the idle
// release's suspension and the launch that resumes its session, changes no holder.
func roleHolder(state supervise.ClaimState, by supervise.SuspendSource, session string) string {
	if !supervise.HoldsRole(state, by) {
		return ""
	}
	return session
}

// EnqueueRoleHolders writes the one role_holders_publish row of issue, due at nextAt, and answers
// whether it wrote it. The payload names the issue alone, since the executor reads the set
// when the row runs, so a row the outbox already holds is made due again rather than queued
// beside: its next_at moves to the earlier of the two, and its attempts and its last error stay.
// The unique partial index of 20261116165926_role_holders_publish makes one row per issue the
// table's rule.
//
// A lane that holds the row now may be inside the PUT with a set it read before the change that
// queues this call. So the lease token is marked (requeuedLease), not cleared: the row stays
// leased, so no other lane sends it beside that PUT, and that worker's finish and its retry,
// which match its own token, match nothing. Once the worker has recorded what its run came to,
// the outbox makes the row due at once (RedueRoleHolders), so the row runs again, with the newer
// set, as soon as the send in flight ends rather than once the lease lapses. A lease already
// marked keeps its mark, and a row no lane holds stays unleased.
//
// project is the caller's own project, its Dispatch key or its claim token's spelling. An issue
// of another project is refused (claim.KeyOfProject): it writes nothing, and answers false and no
// error, since a row keyed by it is run only by that project's daemon, whose own row of the issue
// this upsert would make due and take the lease of.
//
// A claim write that changes a holder calls it in its own transaction (putClaim); the daemon's
// boot pass and the ready route call it in transactions of their own.
func EnqueueRoleHolders(ctx context.Context, tx pgx.Tx, project, issue string, nextAt time.Time) (bool, error) {
	if !claim.KeyOfProject(project, issue) {
		return false, nil
	}
	payload, err := json.Marshal(roleHoldersPublishPayload{Issue: issue})
	if err != nil {
		return false, fmt.Errorf("enqueue the role-holders publish of %s: %w", issue, err)
	}
	if _, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3::jsonb, 0, $4, '')
		on conflict (issue) where kind = 'role_holders_publish'
		do update set next_at = least(outbox.next_at, excluded.next_at),
			lease_token = case when outbox.lease_token = '' or starts_with(outbox.lease_token, $5::text) then outbox.lease_token
				else $5::text || outbox.lease_token end`,
		roleHoldersPublishKind, issue, string(payload), nextAt, requeuedLease); err != nil {
		return false, fmt.Errorf("enqueue %s for %s: %w", roleHoldersPublishKind, issue, err)
	}
	return true, nil
}

// requeuedLease is the mark EnqueueRoleHolders puts before the lease token of a row a lane holds
// when it queues the row again. The lane that holds the token can then tell, at the end of its
// run, that the row was queued again under its own lease (RedueRoleHolders), and not leased by
// another lane after its lease lapsed. A token the outbox mints is hex (record's leaseToken), so no
// minted token starts with it.
const requeuedLease = "requeued:"

// RedueRoleHolders makes the role_holders_publish row id due at now, with no lease, where
// EnqueueRoleHolders marked the lease leaseToken while a lane held it (requeuedLease), and answers
// whether it did. The daemon's outbox calls it once the run that held leaseToken has recorded
// what it came to (outbox.redueRequeued): that run sent a set read before the change that queued
// the row again, so its finish and its retry matched nothing, and the row now runs again at once
// with the set the claims hold. Its attempts and its last error stay, as a queue leaves them. A
// row the mark does not name is left as it stands: one the run finished or returned to the
// outbox, one that is gone, and one another lane leased after this lease lapsed, whose token is
// that lane's own.
//
// The rule is this kind's alone, because only this row's wait leaves wrong data behind: Dispatch
// takes the role of each check result from the set it holds, so a session that a change gave a
// role records its results with no role until the newer set lands, for the rest of the lease and
// up to one sweep after it. The design-gates publication also queues its row again under a live
// lease (record.Postgres.EnqueueDesignGates), and its wait only delays news. A rule for every kind
// would give every row the outbox runs one statement more at its end, where only these two queues
// ever change a lease under a run.
func RedueRoleHolders(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `update outbox set lease_token = '', lease_until = null, next_at = least(next_at, $4)
		where id = $1 and kind = $2 and lease_token = $3`, id, roleHoldersPublishKind, requeuedLease+leaseToken, now)
	if err != nil {
		return false, fmt.Errorf("make the %s row %d due again: %w", roleHoldersPublishKind, id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// PublishRoleHolders turns the role-holder publication on: from this call on, each claim write
// that changes a role holder of an issue of the claim's own project queues its issue's publication
// in its own transaction (queuesRoleHolders), and woken is told that issue once the transaction
// commits, where the daemon wakes the issue's board lane so the row waits for no sweep. A
// transaction that rolls back tells nothing, since it queued nothing.
//
// Until it is called the store queues nothing. Only a daemon with a workflow runs an outbox to
// send the row, and only that daemon calls this: a daemon with no dispatch_url opens no workflow
// (internal/daemon's openWorkflow) and so builds no outbox runner (workflowRuntime.attach's
// newOutbox), and it queues no row of any other Dispatch kind either, so a row queued there would
// be neither sent nor deleted.
func (s *Store) PublishRoleHolders(woken func(issue string)) {
	if woken == nil {
		woken = func(string) {}
	}
	s.roleHoldersQueued.Store(&woken)
}

// queuesRoleHolders is whether a write of claim c can queue a role-holder publication at all: the
// daemon has turned the publication on (PublishRoleHolders), c's session can hold a role of an
// issue (holdsIssueRole), and that issue is of c's own project (claim.KeyOfProject), whose row of
// the issue this daemon's outbox runs. None depends on the stored row, so putClaim reads it before
// it locks that row (putClaim), and a write for which it is false neither reads the stored holder
// for it nor writes the publication's row.
func (s *Store) queuesRoleHolders(c supervise.Claim) bool {
	return s.roleHoldersQueued.Load() != nil && holdsIssueRole(c) && claim.KeyOfProject(c.Project, c.Issue)
}

// wakeRoleHolders tells the daemon's wake (PublishRoleHolders) issue, whose publication a claim
// write queued, once that write's transaction has committed: the hook putClaim registers, the one
// wake of that publication.
func (s *Store) wakeRoleHolders(issue string) {
	if woken := s.roleHoldersQueued.Load(); woken != nil {
		(*woken)(issue)
	}
}
