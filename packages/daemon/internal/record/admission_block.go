package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdmissionBlock is a project's admission block: the cap, the number of P0 holders and the five
// lists GET /legion/v1/state serves under `admission` (api.Admission, which is built from this
// type) and a state_admission event carries (StateAdmission). Both readers build it with
// blockers.AdmissionOf, which says from which judgement each does, so the block the route serves
// and the block the event carries are the same value for the same records.
//
// It is declared here, below both readers, because the admission pass that writes the event cannot
// import api. Its JSON is the route's own: the keys api.Admission writes, in its order, and every
// list an array, never null (MarshalJSON). The route's `p0Bound` is no key of it: the bound in
// force is read beside the judgement (api.P0BoundStateTx) and is nothing Dispatch reads. P0Holders
// is the count of the holders admission treats as P0 (record.HoldersOf), which the Dispatch
// board's header copies (dispatch://INFRA-5636), 0 included. No field holds a time, so a block
// judged twice over the same records encodes the same. One word of the held list does read the
// clock: HeldTreeClosed asks whether a root's linger still runs at the pass's moment
// (LineFacts.Now), so a member of a parked tree moves from tree_closed to tree_parked when that
// linger runs out, with no write (admit's queueAdmissionBlock says when that change is written).
type AdmissionBlock struct {
	Cap       int            `json:"cap"`
	P0Holders int            `json:"p0Holders"`
	Active    []string       `json:"active"`
	Waiting   []string       `json:"waiting"`
	Trees     []string       `json:"trees"`
	Blocked   []BlockedIssue `json:"blocked"`
	Held      []HeldIssue    `json:"held"`
}

// MarshalJSON keeps every list an array on the wire, empty rather than `null`, as api.Admission
// writes them for the caller the route serves the whole block to.
func (b AdmissionBlock) MarshalJSON() ([]byte, error) {
	type wire AdmissionBlock
	out := wire(b)
	if out.Active == nil {
		out.Active = []string{}
	}
	if out.Waiting == nil {
		out.Waiting = []string{}
	}
	if out.Trees == nil {
		out.Trees = []string{}
	}
	if out.Blocked == nil {
		out.Blocked = []BlockedIssue{}
	}
	if out.Held == nil {
		out.Held = []HeldIssue{}
	}
	return json.Marshal(out)
}

// QueueAdmissionBlock writes block as project's admission block (state_admission_blocks) where it
// differs from the block stored there, and queues the state event of that change (StateAdmission,
// carrying the run the state events were turned on with, PublishState): a block equal to the stored
// one writes nothing and queues nothing. The two are compared as jsonb, so no key order or spacing
// can make two equal blocks differ. It answers whether it queued the event, and once it has, it
// calls the call PublishState was handed (statePublication.queued), as the issue and slot writes
// do once they queue a change (queueStateChange).
//
// The caller holds the switch: it reads StatePublished first, since the block it hands in costs a
// judgement of its own (admit's queueAdmissionBlock), so a call while the state events are off is
// refused here. The event's row is built and checked (NewOutboxRow) before the upsert runs, so a
// block whose event would be refused is not written either, and once the upsert returns a row it
// goes in through the insert every outbox row takes (Enqueue), due at the time the upsert returned,
// the transaction's own now(). A pass that leaves the block as it was runs the one statement. The
// row's issue column is project, the bare Dispatch project key, so it runs on the project's one
// state lane (LaneState) as a StateChange does (queueStateChange), and the commit trigger of
// migration 20261126163320_state_events gives it its sequence and its time when tx commits.
func (s *Postgres) QueueAdmissionBlock(ctx context.Context, tx pgx.Tx, project string, block AdmissionBlock) (bool, error) {
	state := s.state.Load()
	if state == nil {
		return false, fmt.Errorf("queue the admission block of %s: the state events are off (PublishState), and the caller reads StatePublished before it judges a block", project)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		return false, fmt.Errorf("queue the admission block of %s: encode it: %w", project, err)
	}
	row, err := NewOutboxRow(project, StateAdmission{Project: project, Run: state.run, Admission: encoded}, time.Time{})
	if err != nil {
		return false, fmt.Errorf("queue the admission block of %s: %w", project, err)
	}
	err = tx.QueryRow(ctx, `insert into state_admission_blocks as stored (project, block) values ($1, $2::jsonb)
		on conflict (project) do update set block = excluded.block
			where stored.block is distinct from excluded.block
		returning now()`, project, string(encoded)).Scan(&row.NextAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("queue the admission block of %s: %w", project, err)
	}
	if err := s.Enqueue(ctx, tx, row); err != nil {
		return false, fmt.Errorf("queue the admission block of %s: %w", project, err)
	}
	state.queued(ctx)
	return true, nil
}
