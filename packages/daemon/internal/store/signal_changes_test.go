package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The entries a published state carries (S6, SignalChangesForPublishTx): every entry written since
// the newest one the state published before carried, and never fewer than the newest 50, newest
// first. The rows are the first publish of a record of fewer than 50, of one of more than 50, a
// publish after fewer than 50 new entries, one after more than 50, and a repeat with nothing new.
// The ids of a project rise by one for each entry in the transaction that writes it, so a
// transaction rolled back, and one retried after it, leave no gap, and each project counts its own.
func TestSignalChangesForPublish(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	const project = "LEGION"
	at := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	write := func(t *testing.T, project string, n int) {
		t.Helper()
		entries := make([]SignalChange, n)
		for i := range entries {
			value := float64(i)
			entries[i] = SeriesValueChange("kills", at.Add(time.Duration(i)*time.Second), &value, "")
		}
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return AppendSignalChangesTx(ctx, tx, project, entries)
		}); err != nil {
			t.Fatalf("write %d entries of %s: %v", n, project, err)
		}
	}
	chosen := func(t *testing.T, published int64) []int64 {
		t.Helper()
		var ids []int64
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			entries, err := SignalChangesForPublishTx(ctx, tx, project, published)
			for _, entry := range entries {
				ids = append(ids, entry.ID)
			}
			return err
		}); err != nil {
			t.Fatalf("choose the entries after %d: %v", published, err)
		}
		return ids
	}
	// ids is from down to through, newest first.
	ids := func(from, through int64) []int64 {
		var want []int64
		for id := from; id >= through; id-- {
			want = append(want, id)
		}
		return want
	}
	if got := chosen(t, 0); len(got) != 0 {
		t.Fatalf("the first publish of an empty record chose %v, want none", got)
	}
	write(t, project, 30)
	if got, want := chosen(t, 0), ids(30, 1); !slices.Equal(got, want) {
		t.Fatalf("the first publish of 30 entries chose %v, want %v", got, want)
	}
	write(t, project, 40)
	if got, want := chosen(t, 0), ids(70, 1); !slices.Equal(got, want) {
		t.Fatalf("the first publish of 70 entries chose %v, want every one, %v", got, want)
	}
	if got, want := chosen(t, 60), ids(70, 21); !slices.Equal(got, want) {
		t.Fatalf("a publish after 10 new entries chose %v, want the newest 50, %v", got, want)
	}
	write(t, project, 60)
	if got, want := chosen(t, 70), ids(130, 71); !slices.Equal(got, want) {
		t.Fatalf("a publish after 60 new entries chose %v, want every one of them, %v", got, want)
	}
	if got, want := chosen(t, 130), ids(130, 81); !slices.Equal(got, want) {
		t.Fatalf("a repeat publish with nothing new chose %v, want the newest 50, %v", got, want)
	}

	rolledBack := errors.New("the fact's transaction fails after its entries")
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		value := 1.0
		if err := AppendSignalChangesTx(ctx, tx, project, []SignalChange{SeriesValueChange("idle", at, &value, "")}); err != nil {
			return err
		}
		return rolledBack
	}); !errors.Is(err, rolledBack) {
		t.Fatalf("the rolled-back write ended with %v, want its own error", err)
	}
	write(t, project, 1)
	write(t, "ACME", 2)
	if got, want := chosen(t, 130), ids(131, 82); !slices.Equal(got, want) {
		t.Fatalf("after a rollback and its retry the publish chose %v, want %v: the retry takes the id the rollback gave back", got, want)
	}
	var acme []int64
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		entries, err := SignalChangesTx(ctx, tx, "ACME", SignalChangesShown)
		for _, entry := range entries {
			acme = append(acme, entry.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []int64{2, 1}; !slices.Equal(acme, want) {
		t.Fatalf("ACME's entries are %v, want %v: each project counts its own ids", acme, want)
	}
}
