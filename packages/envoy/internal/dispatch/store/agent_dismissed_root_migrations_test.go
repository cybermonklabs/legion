package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The Agent half's dismissal on its root: 20261125052902 adds messages.agent_dismissed_at,
// 20261125053125 attaches the trigger that keeps it from user_agent_thread_dismissed and catches up
// the marks already stored, and 20261125053341 builds the index of the live roots the Inbox's two
// states read (api/replies_agent.go, myAgentLiveRoot).
const (
	agentDismissedColumn    = 20261125052902
	agentDismissedRoot      = 20261125053125
	agentDismissedLiveIndex = 20261125053341
)

// agentDismissedAt is a message's agent_dismissed_at, nil while it holds none.
func agentDismissedAt(t *testing.T, store *Store, message string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := store.Pool.QueryRow(context.Background(), `select agent_dismissed_at from messages where id = $1`, message).Scan(&at); err != nil {
		t.Fatalf("read the agent_dismissed_at of message %s: %v", message, err)
	}
	return at
}

// requireAgentDismissedAt fails unless message holds want (nil for none), naming what the moment
// is the column of.
func requireAgentDismissedAt(t *testing.T, store *Store, message string, want *time.Time, what string) {
	t.Helper()
	got := agentDismissedAt(t, store, message)
	if (got == nil) != (want == nil) || (got != nil && !got.Equal(*want)) {
		t.Fatalf("%s: message %s holds agent_dismissed_at %v, want %v", what, message, got, want)
	}
}

// The column is its author's mark and nothing else, from every write of that mark. Two one-off
// roots of alice's, each answered by a session, and one of bob's, written as the release before
// writes them, with alice's marks on both of hers and bob's mark on one of alice's - a mark no route
// writes, of somebody who is not the root's author - stored before the trigger exists. The
// migration's catch-up gives each of alice's roots her mark's moment and bob's own root none; then
// every later write of a mark moves the column in its own statement: a restore's delete clears it,
// a dismissal's insert sets it and its upsert's update moves it, a delete of bob's mark leaves
// alice's, and a session's reply written two deep - an older build's, through the clear trigger,
// which deletes every mark of its root - clears it.
func TestTheAgentRootCarriesItsAuthorsDismissalFromEveryWriteOfTheMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, agentDismissedRoot-1)
	root := func(author string) string {
		return insertReturningID(t, store, `
			insert into messages (author, body, target) values (jsonb_build_object('kind', 'user', 'id', $1::text), 'Can you look?', 'session:s1')
			returning id::text`, author)
	}
	reply := func(parent string) string {
		return insertReturningID(t, store, `
			insert into messages (author, body, in_reply_to, target) values ('{"kind":"session","id":"s1"}', 'Looking.', $1, 'session:s1')
			returning id::text`, parent)
	}
	// One of alice's roots carries her login in capitals, which the Inbox's root filter lowers
	// (myAgentRootFilter) and the trigger lowers with it.
	first, second, bobs := root("Alice"), root("alice"), root("bob")
	firstReply := reply(first)
	reply(second)
	reply(bobs)
	base := time.Date(2026, 11, 25, 5, 0, 0, 0, time.UTC)
	at := func(minute int) *time.Time {
		moment := base.Add(time.Duration(minute) * time.Minute)
		return &moment
	}
	mark := func(login, message string, minute int) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into user_agent_thread_dismissed (login, message_id, dismissed_at) values ($1, $2, $3)
			on conflict (login, message_id) do update set dismissed_at = excluded.dismissed_at
		`, login, message, *at(minute)); err != nil {
			t.Fatalf("write %s's mark on %s: %v", login, message, err)
		}
	}
	unmark := func(login, message string) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `delete from user_agent_thread_dismissed where login = $1 and message_id = $2`, login, message); err != nil {
			t.Fatalf("delete %s's mark on %s: %v", login, message, err)
		}
	}
	mark("alice", first, 1)
	mark("bob", first, 2)
	mark("alice", second, 3)

	migrateAbove(t, store, agentDismissedRoot-1, agentDismissedRoot)
	requireAgentDismissedAt(t, store, first, at(1), "the catch-up of alice's mark, beside bob's later one")
	requireAgentDismissedAt(t, store, second, at(3), "the catch-up of alice's mark")
	requireAgentDismissedAt(t, store, bobs, nil, "bob's root, which no mark names")
	requireAgentDismissedAt(t, store, firstReply, nil, "a reply, which no mark names")

	unmark("alice", second)
	requireAgentDismissedAt(t, store, second, nil, "the restore's delete")
	mark("alice", second, 4)
	requireAgentDismissedAt(t, store, second, at(4), "the dismissal's insert")
	mark("alice", second, 5)
	requireAgentDismissedAt(t, store, second, at(5), "the dismissal's update of a mark that stood")
	mark("bob", bobs, 6)
	requireAgentDismissedAt(t, store, bobs, at(6), "bob's mark on his own root")
	unmark("bob", first)
	requireAgentDismissedAt(t, store, first, at(1), "a delete of bob's mark on alice's root")

	reply(firstReply)
	var marks int
	if err := store.Pool.QueryRow(ctx, `select count(*) from user_agent_thread_dismissed where message_id = $1`, first).Scan(&marks); err != nil {
		t.Fatalf("count the marks of %s: %v", first, err)
	}
	if marks != 0 {
		t.Fatalf("a session's reply two deep left %d marks on its root, want the clear trigger to delete them all", marks)
	}
	requireAgentDismissedAt(t, store, first, nil, "a session's reply that clears the root's marks")
	requireAgentDismissedAt(t, store, second, at(5), "a root no reply reached")
}

// The live index holds the one-off roots no dismissal marks and no other: its predicate, as the
// catalogue holds it, requires agent_dismissed_at is null, and a root's mark takes the root out
// of that predicate and the mark's delete puts it back.
func TestTheLiveAgentRootsIndexHoldsTheRootsNoDismissalMarks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, agentDismissedLiveIndex)
	var valid bool
	var predicate string
	if err := store.Pool.QueryRow(ctx, `
		select i.indisvalid, coalesce(pg_get_expr(i.indpred, i.indrelid), '')
		from pg_index i join pg_class c on c.oid = i.indexrelid
		where c.relname = 'messages_direct_roots_live'
	`).Scan(&valid, &predicate); err != nil || !valid {
		t.Fatalf("messages_direct_roots_live is valid %t (%v), want a valid index", valid, err)
	}
	if !strings.Contains(predicate, "agent_dismissed_at IS NULL") {
		t.Fatalf("messages_direct_roots_live's predicate is %q, want it to hold no root a dismissal marks (agent_dismissed_at IS NULL)", predicate)
	}
	root := insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Can you look?', 'session:s1')
		returning id::text`)
	// live reads whether the root is in the index: whether its row passes the index's own
	// predicate.
	live := func() bool {
		t.Helper()
		var held bool
		if err := store.Pool.QueryRow(ctx, `select exists (select 1 from messages where id = $1 and `+predicate+`)`, root).Scan(&held); err != nil {
			t.Fatalf("read whether %s is live: %v", root, err)
		}
		return held
	}
	if !live() {
		t.Fatal("a root no mark names is not live")
	}
	if _, err := store.Pool.Exec(ctx, `insert into user_agent_thread_dismissed (login, message_id, dismissed_at) values ('alice', $1, now())`, root); err != nil {
		t.Fatalf("dismiss the root: %v", err)
	}
	if live() {
		t.Fatal("a root its author dismissed is still live")
	}
	if _, err := store.Pool.Exec(ctx, `delete from user_agent_thread_dismissed where message_id = $1`, root); err != nil {
		t.Fatalf("restore the root: %v", err)
	}
	if !live() {
		t.Fatal("a root whose mark was deleted is not live again")
	}
}

// The census of the three, as each is pending alone at the schema before it, over a database
// whose every table is above a limit of one byte: the column is a catalogue change the census
// admits at any size, and so is the index, a concurrent build; the trigger's migration is held to
// the limit, since its catch-up updates messages by the marks, which is no bounded write, and is
// admitted under the shipped limit. Each prints the tables it touches, which a deploy's census
// reads.
func TestCensusAdmitsTheAgentDismissedColumnAndIndexAtAnySizeAndHoldsTheCatchUpToTheLimit(t *testing.T) {
	t.Parallel()
	take := func(t *testing.T, migration int, limit int64) *pgmigrate.Report {
		t.Helper()
		store := openMigratedTestStore(t, migration-1)
		report, err := census(context.Background(), store.Pool.Config().ConnString(), migrationsThrough(t, migration), pgmigrate.CensusOptions{TableLimit: limit})
		if err != nil {
			t.Fatalf("census of %d: %v", migration, err)
		}
		if len(report.Pending) != 1 || report.Pending[0].Migration.Version != migration {
			t.Fatalf("pending = %+v, want %d alone; report:\n%s", report.Pending, migration, reportText(report))
		}
		t.Logf("census of %d at a limit of %d bytes:\n%s", migration, limit, reportText(report))
		return report
	}
	t.Run("the column at any size", func(t *testing.T) {
		report := take(t, agentDismissedColumn, 1)
		if report.Refused() {
			t.Errorf("refused: %v", refusals(report))
		}
		if exemption := report.Pending[0].TableSizeExemption; !strings.Contains(exemption, "alter table") {
			t.Errorf("TableSizeExemption = %q, want one naming alter table", exemption)
		}
	})
	t.Run("the index at any size", func(t *testing.T) {
		report := take(t, agentDismissedLiveIndex, 1)
		if entry := report.Pending[0]; !entry.ConcurrentIndex || report.Refused() {
			t.Errorf("not admitted as a concurrent index: %+v, refusals %v", entry, refusals(report))
		}
	})
	t.Run("the trigger and its catch-up under the limit", func(t *testing.T) {
		if report := take(t, agentDismissedRoot, 0); report.Refused() {
			t.Errorf("refused under the shipped limit: %v", refusals(report))
		}
		report := take(t, agentDismissedRoot, 1)
		if !report.Refused() || !strings.Contains(strings.Join(refusals(report), "\n"), "messages") {
			t.Errorf("admitted with messages above the limit, want the catch-up's update of messages refused: %v", refusals(report))
		}
	})
}
