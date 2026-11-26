package graphcensus_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// unmarkedHeads is each issue of keys no mark of issue_head_marks names, in keys' order.
func unmarkedHeads(t *testing.T, ctx context.Context, database *store.Store, keys []string) []string {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		select key from unnest($1::text[]) with ordinality as listed (key, position)
		where not exists (select 1 from issue_head_marks mark where mark.issue_key = listed.key)
		order by position`, keys)
	if err != nil {
		t.Fatalf("read the issues no mark names: %v", err)
	}
	unmarked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the issues no mark names: %v", err)
	}
	return unmarked
}

// TestTheTickRefillsAnIssueWhoseMarkWasTakenAway fills more issues than one tick of the block index
// fills (graphcensus.HeadFillBatch), as a store of the release before holds them, and marks each;
// then a plain-SQL write of the release before on each takes every mark away. One tick of the block
// index (docs.Service.ReconcileBlockIndex) fills and marks a full batch of them and reads as full, so
// the next tick comes at once rather than after the interval; the next tick fills and marks the
// rest. Else the test prints the issues still unmarked after the ticks. The census kind
// issue_head_node then reads zero.
func TestTheTickRefillsAnIssueWhoseMarkWasTakenAway(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	graphcensus.SeedHeadProject(t, ctx, database)
	keys := make([]string, graphcensus.HeadFillBatch+5)
	for index := range keys {
		keys[index] = fmt.Sprintf("HBF-%d", index+1)
		graphcensus.SeedReleaseBeforeIssue(t, ctx, database, index+1, keys[index])
	}
	if _, _, err := graphcensus.FillIssueHeads(ctx, database, len(keys)); err != nil {
		t.Fatalf("fill the issues: %v", err)
	}
	if unmarked := unmarkedHeads(t, ctx, database, keys); len(unmarked) > 0 {
		t.Fatalf("the fill left %s unmarked", strings.Join(unmarked, ", "))
	}
	for _, key := range keys {
		if _, err := database.Pool.Exec(ctx, graphcensus.ReleaseBeforeHeadingText, key+"/spec-md#h", "# Heading, as the release before wrote it"); err != nil {
			t.Fatalf("append a tuple of %s as the release before: %v", key, err)
		}
	}
	if unmarked := unmarkedHeads(t, ctx, database, keys); len(unmarked) != len(keys) {
		t.Fatalf("the writes of the release before took %d of the %d marks away, want every one", len(unmarked), len(keys))
	}

	people := store.NewPgPeopleStore(database.Pool, auth.SingleSigningKey("signing-key"), nil)
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}, ServerURL: "https://dispatch.head.invalid", Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	first, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
	if err != nil {
		t.Fatalf("the first tick of the block index: %v", err)
	}
	afterFirst := unmarkedHeads(t, ctx, database, keys)
	t.Logf("the first tick of the block index filled %d issues and reads full %t; %d of %d issues hold no mark after it",
		first.HeadsFilled, first.Full, len(afterFirst), len(keys))
	if first.HeadsFilled != graphcensus.HeadFillBatch || len(afterFirst) != len(keys)-graphcensus.HeadFillBatch {
		t.Errorf("the first tick of the block index filled %d issues, and %d of %d hold no mark after it, want a full batch of %d filled and marked",
			first.HeadsFilled, len(afterFirst), len(keys), graphcensus.HeadFillBatch)
	}
	if !first.Full {
		t.Errorf("the first tick of the block index filled a full batch of issues and reads as not full, so the next tick waits the whole interval")
	}
	second, err := documents.ReconcileBlockIndex(ctx, first.Cursor, nil)
	if err != nil {
		t.Fatalf("the second tick of the block index: %v", err)
	}
	afterSecond := unmarkedHeads(t, ctx, database, keys)
	t.Logf("the second tick of the block index filled %d issues and reads full %t; %d of %d issues hold no mark after it",
		second.HeadsFilled, second.Full, len(afterSecond), len(keys))
	if len(afterSecond) > 0 {
		t.Errorf("after two ticks of the block index %d issues hold no mark: %s", len(afterSecond), strings.Join(afterSecond, ", "))
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		kind, err := graphcensus.CensusOfHeads(ctx, tx)
		if err != nil {
			return err
		}
		t.Logf("the census kind %s reads %d rows against %d the store gives, %d and %d differences",
			kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		for _, found := range append(kind.ReportedRows, kind.ReportedNodes...) {
			t.Errorf("after the ticks the census kind issue_head_node reads %d difference(s) at %s (%s)",
				kind.RowDifferences+kind.NodeDifferences, found.At, found.Cause)
		}
		return nil
	}); err != nil {
		t.Fatalf("take the census kind issue_head_node: %v", err)
	}
}
