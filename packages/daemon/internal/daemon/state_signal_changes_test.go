package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Each admission state event carries `admission.signalChanges` (S6 of dispatch://INFRA-5899): every
// entry of the record of changes written since the newest entry the admission event published
// before carried, and never fewer than the newest 50, newest first (record.AdmissionBlock's
// SignalChanges, store.PublishedSignalChangesTx). The steps write entries and end as the admission
// pass ends a fact that judges the block (admit's queueAdmissionBlock: admit.WithSignalChanges and
// record.Store.QueueAdmissionBlock), and the lane publishes each:
// a first publish of 60 entries carries all 60; one after 10 new entries the newest 50, so the 10
// and the 40 before them; one after 60 new entries all 60 of them; the first pass that writes no
// entry after it the newest 50, which the rule chooses where nothing is new; and a second such pass
// leaves the block as it was and publishes no event. Each event's entries are the record's own,
// member for member. It fails where the event carries no `signalChanges` key, or a list another
// rule chose.
func TestStateAdmissionEventsCarryEverySignalChangeSinceTheLast(t *testing.T) {
	t.Parallel()
	r := newStateRig(t)
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	published := 0
	// pass writes n entries of the idle share and the block in one transaction, drains the lane,
	// and answers the entries of each admission event it published.
	pass := func(t *testing.T, n int) [][]store.SignalChange {
		t.Helper()
		r.commit(t, r.records, func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			entries := make([]store.SignalChange, n)
			for i := range entries {
				value := float64(i) / 100
				entries[i] = store.SeriesValueChange(signals.Idle, at, &value, "")
			}
			if err := store.AppendSignalChangesTx(ctx, tx, "LEGION", entries); err != nil {
				return err
			}
			block, err := admit.WithSignalChanges(ctx, tx, "LEGION", record.AdmissionBlock{Cap: 1})
			if err != nil {
				return err
			}
			_, err = records.QueueAdmissionBlock(ctx, tx, "LEGION", block)
			return err
		})
		r.drain(t)
		states := statesPublished(t, r.publisher, published)
		published += len(states)
		var carried [][]store.SignalChange
		for _, state := range states {
			for _, event := range state.events {
				if event.Kind != "admission" {
					continue
				}
				var block struct {
					SignalChanges *[]store.SignalChange `json:"signalChanges"`
				}
				if err := json.Unmarshal(event.Admission, &block); err != nil {
					t.Fatalf("decode the admission event %s: %v", event.Admission, err)
				}
				if block.SignalChanges == nil {
					t.Fatalf("the admission event %s carries no admission.signalChanges", event.Admission)
				}
				carried = append(carried, *block.SignalChanges)
			}
		}
		return carried
	}
	// wholeRecord is the project's whole record of changes, newest first.
	wholeRecord := func(t *testing.T) []store.SignalChange {
		t.Helper()
		var entries []store.SignalChange
		if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			entries, err = store.SignalChangesTx(ctx, tx, "LEGION", 1000)
			return err
		}); err != nil {
			t.Fatalf("read the record of changes: %v", err)
		}
		return entries
	}
	// ids is from down to through, newest first.
	ids := func(from, through int64) []int64 {
		var listed []int64
		for id := from; id >= through; id-- {
			listed = append(listed, id)
		}
		return listed
	}
	for _, step := range []struct {
		name    string
		written int
		want    []int64
	}{
		{"the first publish of 60 entries", 60, ids(60, 1)},
		{"a publish after 10 new entries", 10, ids(70, 21)},
		{"a publish after 60 new entries", 60, ids(130, 71)},
		{"a publish after no new entry", 0, ids(130, 81)},
	} {
		carried := pass(t, step.written)
		if len(carried) != 1 {
			t.Fatalf("%s published %d admission events, want one", step.name, len(carried))
		}
		var got []int64
		for _, entry := range carried[0] {
			got = append(got, entry.ID)
		}
		if !slices.Equal(got, step.want) {
			t.Fatalf("%s carried the entries %v, want %v", step.name, got, step.want)
		}
		whole := wholeRecord(t)
		for _, entry := range carried[0] {
			kept := whole[len(whole)-int(entry.ID)]
			if want, got := mustJSON(t, kept), mustJSON(t, entry); got != want {
				t.Fatalf("%s carried the entry %s, and the record holds %s", step.name, got, want)
			}
		}
	}
	if carried := pass(t, 0); len(carried) != 0 {
		t.Fatalf("a pass that wrote no entry published %d admission events, want none: the block it judged is the one stored", len(carried))
	}
}
