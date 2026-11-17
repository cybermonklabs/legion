package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The publication of an issue's role sessions to Dispatch is an outbox row the daemon's board lane
// runs (record.RoleHoldersPublish, PUT /api/v1/issues/{key}/role-holders), and it reads the live
// set from the claims when it runs. Once the daemon has turned the publication on
// (PublishRoleHolders), the claim write that changes the session holding a role queues it in that
// write's own transaction (putClaim), where every write of a claim's state and session converges,
// so the change and the obligation to publish it commit together or not at all: no change of a
// holder stands that nothing owes a publication, and a write the store refused leaves neither. An
// end whose own write failed with an error the machine owes is published when the repair that
// makes it good lands, whichever decision makes it. An end whose failed write is not owed is
// written again only when a later decision retires a task the claim still holds, which writes the
// claim with it; until then the store, and so Dispatch's set, keeps the claim live
// (dispatch://INFRA-4099).

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
// state has a registered agent with a running process (supervise.HoldsCapability), "" in every
// other state. It is the row's part of what a publication sends (record.Postgres.LiveRoleHolders,
// whose state list a test holds to HoldsCapability).
func roleHolder(state supervise.ClaimState, session string) string {
	if !supervise.HoldsCapability(state) {
		return ""
	}
	return session
}

// storedRoleHolder is the holder the row of c names before c is written, read under the row's lock
// so that no other write of the claim lands between this read and the write that replaces it. A
// claim the store does not hold yet names none. A claim's first row is its admission (AdmitClaim),
// which is globally serialized, so two first writes of one claim never race past this read.
func storedRoleHolder(ctx context.Context, tx pgx.Tx, c supervise.Claim) (string, error) {
	var state, session string
	err := tx.QueryRow(ctx, `select state, session from claims where token = $1 for update`, string(c.Token)).Scan(&state, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the role holder the row of %s names: %w", c.Token, err)
	}
	return roleHolder(supervise.ClaimState(state), session), nil
}

// EnqueueRoleHolders writes the one role_holders_publish row of issue, due at nextAt. The payload
// names the issue alone, since the executor reads the live set when the row runs, so a row the
// outbox already holds is made due again rather than queued beside: its next_at moves to the
// earlier of the two, and its attempts and its last error stay. The unique partial index of
// 20261116165926_role_holders_publish makes one row per issue the table's rule.
//
// A lane that holds the row now may be inside the PUT with a set it read before the change that
// queues this call. So the lease token is rotated, as record.Postgres.EnqueueDesignGates rotates
// it: that worker's finish and its retry match nothing, and the row runs again, with the newer
// set, once the lease expires.
//
// A claim write that changes a holder calls it in its own transaction (putClaim); the daemon's
// boot pass and the ready route call it in transactions of their own.
func EnqueueRoleHolders(ctx context.Context, tx pgx.Tx, issue string, nextAt time.Time) error {
	payload, err := json.Marshal(roleHoldersPublishPayload{Issue: issue})
	if err != nil {
		return fmt.Errorf("enqueue the role-holders publish of %s: %w", issue, err)
	}
	if _, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3::jsonb, 0, $4, '')
		on conflict (issue) where kind = 'role_holders_publish'
		do update set next_at = least(outbox.next_at, excluded.next_at),
			lease_token = case when outbox.lease_token = '' then '' else gen_random_uuid()::text end`,
		roleHoldersPublishKind, issue, string(payload), nextAt); err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", roleHoldersPublishKind, issue, err)
	}
	return nil
}

// PublishRoleHolders turns the role-holder publication on: from this call on, each claim write
// that changes a role holder queues its issue's publication in its own transaction, and woken is
// told that issue once the transaction commits, where the daemon wakes the issue's board lane so
// the row waits for no sweep. A transaction that rolls back tells nothing, since it queued nothing.
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
// daemon has turned the publication on (PublishRoleHolders), and c's session can hold a role of an
// issue (holdsIssueRole). Neither depends on the stored row, so each is read before a transaction
// opens, and a write for which it is false needs none (PutClaim).
func (s *Store) queuesRoleHolders(c supervise.Claim) bool {
	return s.roleHoldersQueued.Load() != nil && holdsIssueRole(c)
}

// committedClaim is told each claim write whose transaction has committed, queued whether that
// write queued the publication of its issue.
func (s *Store) committedClaim(c supervise.Claim, queued bool) {
	if !queued {
		return
	}
	if woken := s.roleHoldersQueued.Load(); woken != nil {
		(*woken)(c.Issue)
	}
}
