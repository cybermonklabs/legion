package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/stateevent"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim's state is one of the fields Dispatch observes of an issue (stateevent.FieldClaim, the
// state route's claim of each role of the issue), and every write of it converges in putClaim, as
// the role-holder publication's does. Once the daemon has turned the state events on
// (PublishState), the claim write that moves the state the route serves queues one state_change
// row in that write's own transaction, so the change and its event commit together or not at all,
// and the row takes its sequence and its time when the transaction commits (the commit trigger of
// migration 20261126163320_state_events). This package computes the change and hands it to the
// queue PublishState was given (StateQueue); package stateevent's comment says whose queue that is
// and why it is handed in.

// StateQueue queues one state change in tx, due at due: record.Postgres.EnqueueStateChange, the
// queue package stateevent's comment names.
type StateQueue func(ctx context.Context, tx pgx.Tx, change stateevent.Change, due time.Time) error

// statePublication is what PublishState turns the state events on with: the Dispatch project key
// every row is queued under, the run that writes them, the queue that checks and inserts each row,
// and the wake told once a row commits.
type statePublication struct {
	project string
	run     string
	queue   StateQueue
	woken   func()
}

// PublishState turns the state events of the claim writes on: from this call on, each claim write
// that moves what the state route serves of its claim queues one state_change row (queueClaimState)
// in the write's own transaction, through queue, and woken is told once that transaction commits,
// where the daemon wakes the project's state lane so the row waits for no sweep. A transaction that
// rolls back tells nothing, since it queued nothing. project is the Dispatch project key, which the
// row's issue column holds so that every state row of the project runs on its one state lane
// (record.LaneState), and which the payload names; run is the id of the daemon run that writes the
// rows, the run the record store's events carry (record.Postgres.PublishState); queue is the record
// store's (StateQueue), and must not be nil.
//
// Until it is called the store queues no state row; only a daemon with a workflow calls it
// (internal/daemon's run says why).
func (s *Store) PublishState(project, run string, queue StateQueue, woken func()) {
	if woken == nil {
		woken = func() {}
	}
	s.stateQueued.Store(&statePublication{project: project, run: run, queue: queue, woken: woken})
}

// queuesState is the state events' switch as a write of claim c reads it: nil when the write can
// queue no state row, because the daemon has not turned the events on (PublishState) or because
// c is no claim a state event can name (statesClaim), and the switch otherwise. Neither depends on
// the stored row, so putClaim reads it before it locks that row (putClaim).
func (s *Store) queuesState(c supervise.Claim) *statePublication {
	if !statesClaim(c) {
		return nil
	}
	return s.stateQueued.Load()
}

// statesClaim is whether the state route shows claim c as the claim of a role of an issue, in the
// shape a state event names one: an issue key, and one of a claim's six roles (stateevent's check
// of a change). That holds of every claim the daemon makes (claim.NewToken), and of no claim that
// holds no role of an issue (holdsIssueRole): the controller's, which the route shows on no issue,
// writes none.
func statesClaim(c supervise.Claim) bool {
	return holdsIssueRole(c) && claim.IsIssueKey(c.Issue) && claim.IsRole(c.Role)
}

// queueClaimState queues, in tx, the state event of the write of claim c over stored, the row the
// write replaced (lockedClaim), and answers whether it queued one. The event names the claim's
// state under each role whose step c works (workedRoles), from the stored state, null where the
// store held no row yet, to c's, when the write moved its state or its role; and it names the
// phase, from null to unrecorded, when the write is the first to list an issue the daemon holds no
// record of (storedClaim.unlisted), which the route serves at unrecorded from then on. A write that
// moved none of them queues nothing; so a later claim write of that issue names the claim alone,
// since the route served its phase at unrecorded already. The row is due at once, as the
// role-holder publication beside it is (putClaim).
//
// The state route shows, as the claim of each role of an issue, the claim of the worker on that
// role's step (projection's listIssues, record.WorkerClaim), and Dispatch observes each role's
// claim from it (`claim.<role>`, from `workers` and `architect`): two claims of one issue can hold
// one role - the agent of a key on a step of another role, beside that role's own claim of an
// earlier version of the plan, which the route shows among `otherClaims` - so the event of a claim
// that works no step of its role would overwrite what Dispatch observes of the claim that does. A
// claim that works no role's step names no role. The agent of a key takes the role of its next
// step with its state unchanged (supervise's Rerole), after the phases row of that step names it,
// so the write that moves its role names its state under each role it works then, the new one
// among them, though its state did not move.
func queueClaimState(ctx context.Context, tx pgx.Tx, state *statePublication, c supervise.Claim, stored storedClaim) (bool, error) {
	var changes []stateevent.FieldChange
	if stored.unlisted {
		changes = append(changes, stateevent.FieldChange{Field: stateevent.FieldPhase, Old: stateevent.Null, New: encodedString(string(stateevent.Unrecorded))})
	}
	if c.State != stored.state || c.Role != stored.role {
		roles, err := workedRoles(ctx, tx, c)
		if err != nil {
			return false, err
		}
		for _, role := range roles {
			changes = append(changes, stateevent.FieldChange{Field: stateevent.FieldClaim, Role: role, Old: claimStateValue(stored.state), New: claimStateValue(c.State)})
		}
	}
	if len(changes) == 0 {
		return false, nil
	}
	change := stateevent.Change{Project: state.project, Run: state.run, Issue: c.Issue, Changes: changes}
	if err := state.queue(ctx, tx, change, time.Now().UTC()); err != nil {
		return false, fmt.Errorf("queue the state change of %s: %w", c.Issue, err)
	}
	return true, nil
}

// workedRoles is each role of c's issue whose step c works, by record.WorkerClaim's rule over the
// issue's phases rows as tx reads them: a role whose row names c while it runs a step of the
// issue's plan (step_task set), and the role c is named for (claim.Token.Slot: the role of its
// agent's first step, whose own token is c's), unless that role's row runs a step, whose agent it
// then names. This package sits below record (package stateevent's comment says why), so it
// restates the rule over the columns it reads, as the idle view carries them for it
// (pods.IdleView.Phases). A token that names no role is read as the role c holds. The roles are in
// claim.Roles' order, a role of the deployment after them by name.
func workedRoles(ctx context.Context, tx pgx.Tx, c supervise.Claim) ([]claim.Role, error) {
	rows, err := tx.Query(ctx, `select role, claim, step_task from phases where issue = $1`, c.Issue)
	if err != nil {
		return nil, fmt.Errorf("read the phases rows of %s for the state of %s: %w", c.Issue, c.Token, err)
	}
	named := c.Role
	if slot, ok := c.Token.Slot(); ok && claim.IsRole(claim.Role(slot)) {
		named = claim.Role(slot)
	}
	var roles []claim.Role
	namedRunsAStep := false
	for rows.Next() {
		var role, token, stepTask string
		if err := rows.Scan(&role, &token, &stepTask); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the phases rows of %s for the state of %s: %w", c.Issue, c.Token, err)
		}
		if stepTask == "" || token == "" {
			continue
		}
		if claim.Role(role) == named {
			namedRunsAStep = true
		}
		if claim.Token(token) == c.Token {
			roles = append(roles, claim.Role(role))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the phases rows of %s for the state of %s: %w", c.Issue, c.Token, err)
	}
	if !namedRunsAStep {
		roles = append(roles, named)
	}
	slices.SortFunc(roles, func(a, b claim.Role) int {
		return cmp.Or(cmp.Compare(claim.RoleRank(a), claim.RoleRank(b)), cmp.Compare(a, b))
	})
	return roles, nil
}

// claimStateValue is a claim's state as a state event carries it: the state string the route
// serves on the claim's view (api.ClaimViewOf), or null for the empty state, which is the old
// state of a claim the store held no row of (storedClaim), and which Dispatch reads as no value.
func claimStateValue(state supervise.ClaimState) json.RawMessage {
	if state == "" {
		return stateevent.Null
	}
	return encodedString(string(state))
}

// encodedString is value as a JSON string, the encoding a state event carries a phase or a claim's
// state in.
func encodedString(value string) json.RawMessage {
	// A string always encodes.
	encoded, _ := json.Marshal(value)
	return encoded
}
