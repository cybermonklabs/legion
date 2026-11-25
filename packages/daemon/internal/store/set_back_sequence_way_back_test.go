package store

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// setBackSequenceVersion is the version of 20261120012547_set_back_observed_seq, whose header
// carries the way back of the release that lets the owning architect's status stand.
// docs/kubernetes.md carries the same block under "Rolling back the release that lets the owning
// architect's status stand", and internal/textcheck's
// TestTheSetBackSequenceWayBackIsOneTextInTheHeaderAndTheGuide holds them to one text. This file
// reads the header alone, so the guide stays out of this package's CI filter.
const setBackSequenceVersion = 20261120012547

// setBackSequenceRelease names that release, as a failure to read the header's block names it.
const setBackSequenceRelease = "the release that lets the owning architect's status stand"

// statusWriteBeforeTheSequence is record.StatusWrite as the release before this one declares it:
// every field of this release's shape but ObservedSeq. It is a copy because that release's type is
// not in the tree. That release decodes a dispatch_status row strictly (record.DecodeOutboxPayload,
// DisallowUnknownFields), so a row this shape refuses is a row it fails at every attempt.
type statusWriteBeforeTheSequence struct {
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	ObservedStatus  string `json:"observedStatus"`
	Reason          string `json:"reason,omitempty"`
	HandOverTree    string `json:"handOverTree,omitempty"`
	HandOverLinger  uint64 `json:"handOverLinger,omitempty"`
	CloseGeneration uint64 `json:"closeGeneration,omitempty"`
}

// decodedBeforeTheSequence is the error the release before's strict decode of a dispatch_status
// payload gives, nil where it takes the payload.
func decodedBeforeTheSequence(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value statusWriteBeforeTheSequence
	return decoder.Decode(&value)
}

// The way back of the release that lets the owning architect's status stand takes the key
// observedSeq off each status row of the project that rolls back, and changes nothing else. The
// rows are pending writes of two projects as this release queues them (record.NewOutboxRow), as the
// daemon stops for the rollback: the set-back of a worker's backlog, which Dispatch refused twice
// and which waits on its backoff with its last error, a set-back an earlier release queued with no
// sequence, a phase status write, a close with its reason and a message of LEGION, and another
// project's set-back. The block runs for LEGION as the migration's header carries it, as one exec
// (the simple protocol a psql run uses). Then LEGION's set-back is the same write without its
// sequence, every other row reads as it was queued, and each row keeps its retry state. Each status
// row of LEGION takes the strict decode of the release before, and OTHER's set-back keeps its key:
// a daemon of OTHER that stays on this release still checks its own set-backs.
//
// The control comes first, on the same rows: the strict decode of the release before refuses each
// set-back that carries a sequence, and takes every other status row.
//
// What this cannot show is the earlier daemon running: its binary is not here, so its decode is the
// copy above (statusWriteBeforeTheSequence).
func TestTheSetBackSequenceWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stopped := time.Date(2026, 11, 20, 1, 30, 0, 0, time.UTC)
	queued := []struct {
		name    string
		issue   string
		payload record.OutboxPayload
		// after is what this release reads of the row once the way back has run, nil where it is
		// the payload as it was queued.
		after record.OutboxPayload
		// attempts and lastError are the row's retry state when the daemon stops.
		attempts  int
		lastError string
	}{
		{name: "the set-back of a worker's backlog", issue: "LEGION-2", payload: record.ReassertStatusWrite("in_progress", "backlog", 14),
			after: record.ReassertStatusWrite("in_progress", "backlog", 0), attempts: 2,
			lastError: "set Dispatch issue LEGION-2 to in_progress: it changed under each of the 3 reads this attempt made"},
		{name: "a set-back an earlier release queued", issue: "LEGION-3", payload: record.ReassertStatusWrite("retro", "done", 0)},
		{name: "a phase status write", issue: "LEGION-2", payload: record.OrdinaryStatusWrite("testing", "in_progress", "")},
		{name: "a close with its reason", issue: "LEGION-4", payload: record.CloseStatusWrite("done", "merging", "Merged as #42.", 3)},
		{name: "a message", issue: "LEGION-2", payload: record.MessagePost{Body: "Pull request checks are blocked."}},
		{name: "another project's set-back", issue: "OTHER-7", payload: record.ReassertStatusWrite("in_progress", "done", 9)},
	}
	// retryState is a row's attempts, the time it is next due and its last error.
	type retryState struct {
		attempts  int
		nextAt    time.Time
		lastError string
	}
	ids := make([]int64, len(queued))
	inserted := make([]retryState, len(queued))
	for i, q := range queued {
		row, err := record.NewOutboxRow(q.issue, q.payload, stopped)
		if err != nil {
			t.Fatalf("queue %s: %v", q.name, err)
		}
		state := retryState{attempts: q.attempts, nextAt: stopped, lastError: q.lastError}
		for attempt := range q.attempts {
			state.nextAt = state.nextAt.Add(record.RetryBackoff(attempt))
		}
		if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3, $4, $5, $6) returning id`, string(row.Kind), row.Issue, row.Payload, state.attempts, state.nextAt, state.lastError).Scan(&ids[i]); err != nil {
			t.Fatalf("insert %s: %v", q.name, err)
		}
		inserted[i] = state
		if row.Kind != record.OutboxKindDispatchStatus {
			continue
		}
		carries := q.payload.(record.StatusWrite).ObservedSeq != 0
		if err := decodedBeforeTheSequence(row.Payload); carries != (err != nil) {
			t.Fatalf("the release before's decode of %s = %v; the control wants it to refuse the set-backs that carry a sequence alone, or this test cannot see the rows the way back exists for",
				q.name, err)
		}
	}

	block := strings.ReplaceAll(wayBackOf(t, setBackSequenceVersion, setBackSequenceRelease), rollingBackProject, "LEGION")
	if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back the header of migration %d carries: %v", setBackSequenceVersion, err)
	}

	for i, q := range queued {
		var kind, issue string
		var payload []byte
		var got retryState
		if err := store.Pool().QueryRow(ctx, `select kind, issue, payload, attempts, next_at, last_error from outbox where id = $1`, ids[i]).
			Scan(&kind, &issue, &payload, &got.attempts, &got.nextAt, &got.lastError); err != nil {
			t.Errorf("read %s (row %d) after the way back: %v", q.name, ids[i], err)
			continue
		}
		want := q.after
		if want == nil {
			want = q.payload
		}
		row := record.OutboxRow{ID: ids[i], Kind: record.OutboxKind(kind), Issue: issue, Payload: payload}
		if decoded, err := record.DecodeOutboxPayload(row); err != nil {
			t.Errorf("this release's decode of %s after the way back: %v", q.name, err)
		} else if !reflect.DeepEqual(decoded, want) {
			t.Errorf("%s reads %#v after the way back, want %#v", q.name, decoded, want)
		}
		if w := inserted[i]; got.attempts != w.attempts || !got.nextAt.Equal(w.nextAt) || got.lastError != w.lastError {
			t.Errorf("%s stands after the way back with %d attempts, due at %s, after %q; want its %d, %s and %q as they were",
				q.name, got.attempts, got.nextAt.UTC(), got.lastError, w.attempts, w.nextAt, w.lastError)
		}
		if row.Kind != record.OutboxKindDispatchStatus || !strings.HasPrefix(issue, "LEGION-") {
			continue
		}
		if err := decodedBeforeTheSequence(payload); err != nil {
			t.Errorf("the release before refuses %s after the way back, and retries it at every attempt: %v", q.name, err)
		}
	}
	if rows := rowsAsText(t, store, `select id::text from outbox`); len(rows) != len(queued) {
		t.Errorf("the outbox holds %d rows after the way back, want the %d it held: %v", len(rows), len(queued), rows)
	}
}
