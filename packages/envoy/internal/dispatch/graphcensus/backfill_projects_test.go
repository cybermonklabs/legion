package graphcensus

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheBackfillGivesEveryRowItsNodeAndProject is the backfill of the project of every node,
// which walks only the rows its marker does not mark. A database holds the rows a build
// before the node writers wrote: an issue, its document, a project's document, a conversation's
// file, an ask, a message on the issue, a direct message and its session's reply, with no node;
// and a comment whose node the comment writer stored with no project. Before the run the census
// names every one of them and ProjectComplete answers each; after it, every node is written in
// its row's project or between its thread's parties, dated at its row's own created_at, the census
// reads zero in every class and kind, and a second run writes nothing. Deleting one node is the
// control: the census and ProjectComplete name exactly that row again.
func TestTheBackfillGivesEveryRowItsNodeAndProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedRowsWithoutNodes(t, ctx, database)

	before, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows before the run: %v", err)
	}
	for _, want := range []string{
		"issue BACK-1: missing" + backfillCure, "artifact BACK-1/notes-md: missing" + backfillCure,
		"artifact BACK/runbook-md: missing" + backfillCure,
		"artifact agent/" + seeded.session + "/photo-png: missing" + backfillCure, "ask " + seeded.ask + ": missing" + backfillCure,
		"message " + seeded.issueMessage + ": missing" + backfillCure, "message " + seeded.direct + ": missing" + backfillCure,
		"message " + seeded.reply + ": missing" + backfillCure, "comment " + seeded.comment + ": unscoped" + backfillCure,
	} {
		if !slices.Contains(before, want) {
			t.Errorf("before the run ProjectComplete answers %v, which lacks %q", before, want)
		}
	}

	var out bytes.Buffer
	if _, err := Backfill(ctx, database, &out); err != nil {
		t.Fatalf("run the backfill: %v\n%s", err, out.String())
	}
	requireComplete(t, ctx, database, "after the run")

	direct := []string{"person:alice", "session:" + seeded.session}
	for _, want := range []struct {
		kind, id, project string
		parties           []string
		madeAt            time.Time
	}{
		{graphstore.KindIssue, "BACK-1", "BACK", nil, seeded.at},
		{graphstore.KindArtifact, "BACK-1/notes-md", "BACK", nil, seeded.at},
		{graphstore.KindArtifact, "BACK/runbook-md", "BACK", nil, seeded.at},
		{graphstore.KindArtifact, "agent/" + seeded.session + "/photo-png", "", []string{"session:" + seeded.session}, seeded.at},
		{graphstore.KindAsk, seeded.ask, "BACK", nil, seeded.at},
		{graphstore.KindMessage, seeded.issueMessage, "BACK", nil, seeded.at},
		{graphstore.KindMessage, seeded.direct, "", direct, seeded.at},
		{graphstore.KindMessage, seeded.reply, "", direct, seeded.at.Add(time.Minute)},
		{graphstore.KindComment, seeded.comment, "BACK", nil, seeded.at},
	} {
		node, held, err := graphstore.ReadNode(ctx, database.Pool, want.kind, want.id)
		if err != nil || !held {
			t.Fatalf("the store holds no node %s/%s after the run (held %t, %v)", want.kind, want.id, held, err)
		}
		if node.Project != want.project || !slices.Equal(node.Parties, want.parties) || !node.MadeAt.Equal(want.madeAt) {
			t.Errorf("node %s/%s carries project %q, parties %v, made at %s; want %q, %v, %s",
				want.kind, want.id, node.Project, node.Parties, node.MadeAt, want.project, want.parties, want.madeAt)
		}
	}

	// A second run reads no row the first marked: it writes no node and states nothing.
	var tuplesBefore int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&tuplesBefore); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	second, err := Backfill(ctx, database, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	for _, kind := range second.Kinds {
		if kind.Rows != 0 || kind.Nodes != 0 {
			t.Errorf("the second run reports %+v, want nothing written", kind)
		}
	}
	var tuplesAfter int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&tuplesAfter); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	if tuplesAfter != tuplesBefore {
		t.Errorf("the second run appended %d tuples, want none", tuplesAfter-tuplesBefore)
	}

	// The control: a row whose node is taken away is named again, by its kind and its id. The
	// erase deletes the tuples it is handed by seq and no others, so it is handed every tuple of
	// either side of the ask's node, read in its own transaction. The asks pass marked the ask's
	// row and its followers' rows with the node, and a mark is a foreign key
	// into graph_nodes, so the control first takes the marks off, leaving the rows as a build before
	// the store wrote them.
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		for _, table := range []string{"asks", "ask_followers"} {
			if _, err := tx.Exec(ctx, `update `+table+` set node_kind = null, node_address = null
				where node_kind = $1 and node_address = $2`, graphstore.KindAsk, seeded.ask); err != nil {
				return err
			}
		}
		var seqs []int64
		if err := tx.QueryRow(ctx, `
			select coalesce(array_agg(seq), '{}') from graph_tuples
			where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2)
		`, graphstore.KindAsk, seeded.ask).Scan(&seqs); err != nil {
			return err
		}
		return graphstore.EraseNode(ctx, tx, graphstore.KindAsk, seeded.ask, seqs)
	}); err != nil {
		t.Fatalf("take the ask's node away: %v", err)
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows after the control: %v", err)
	}
	if want := []string{"ask " + seeded.ask + ": missing" + backfillCure}; !slices.Equal(incomplete, want) {
		t.Errorf("with the ask's node taken away ProjectComplete answers %v, want %v", incomplete, want)
	}
	census, err := ProjectCensus(ctx, database.Pool)
	if err != nil {
		t.Fatalf("take the project census after the control: %v", err)
	}
	for _, kind := range census {
		if want := int64(0); kind.Kind == graphstore.KindAsk {
			want = 1
			if kind.Missing != want {
				t.Errorf("the census reads %+v for asks with one node taken away, want missing=1", kind)
			}
		} else if kind.Findings() != want {
			t.Errorf("the census reads %+v for %s, want nothing", kind, kind.Kind)
		}
	}
}

// requireComplete fails unless the project census reads zero in every class of every kind, and
// ProjectComplete answers nothing.
func requireComplete(t *testing.T, ctx context.Context, database *store.Store, when string) {
	t.Helper()
	census, err := ProjectCensus(ctx, database.Pool)
	if err != nil {
		t.Fatalf("take the project census %s: %v", when, err)
	}
	var printed strings.Builder
	writeProjectCensus(&printed, census)
	for _, kind := range census {
		if kind.Findings() != 0 {
			t.Errorf("%s the project census reads:\n%s", when, printed.String())
			break
		}
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows %s: %v", when, err)
	}
	if len(incomplete) != 0 {
		t.Errorf("%s ProjectComplete answers %v, want nothing", when, incomplete)
	}
}

// seededRows is what seedRowsWithoutNodes wrote.
type seededRows struct {
	at                               time.Time
	session                          string
	ask, issueMessage, direct, reply string
	comment                          string
}

// seedRowsWithoutNodes writes, by SQL alone, the rows a build before the node writers wrote, and a
// comment whose node a build before a node named its project stored with none.
func seedRowsWithoutNodes(t *testing.T, ctx context.Context, database *store.Store) seededRows {
	t.Helper()
	seeded := seededRows{
		at:           time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		session:      "01a1e0fe-90a3-76fa-b47a-7d62f39b4599",
		ask:          "11111111-1111-4111-8111-000000000001",
		issueMessage: "11111111-1111-4111-8111-000000000002",
		direct:       "11111111-1111-4111-8111-000000000003",
		reply:        "11111111-1111-4111-8111-000000000004",
		comment:      "11111111-1111-4111-8111-000000000005",
	}
	alice := `{"kind":"user","id":"alice"}`
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('BACK', 'Backfill', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank, created_at)
		values ('BACK-1', 'BACK', 1, 'One', '`+alice+`', 'U', '2026-11-01 12:00:00+00');
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
		values ('BACK-1', 'BACK', 'notes-md', 'notes.md', 'doc', '`+alice+`', '2026-11-01 12:00:00+00'),
		       (null, 'BACK', 'runbook-md', 'Runbook.md', 'doc', '`+alice+`', '2026-11-01 12:00:00+00');
		insert into artifacts (session_id, slug, name, kind, created_by, created_at)
		values ('`+seeded.session+`', 'photo-png', 'photo.png', 'image', '`+alice+`', '2026-11-01 12:00:00+00');
	`); err != nil {
		t.Fatalf("seed the project, the issue and the documents: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into asks (id, issue_key, author, question, options, multiple, urgency, kind, created_at)
		values ($1, 'BACK-1', $2, 'Does it ship?', '[]', false, 'med', 'question', $3)
	`, seeded.ask, alice, seeded.at); err != nil {
		t.Fatalf("seed the ask: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (id, issue_key, author, body, created_at)
		values ($1, 'BACK-1', $2, 'On the issue.', $3)
	`, seeded.issueMessage, alice, seeded.at); err != nil {
		t.Fatalf("seed the issue message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (id, issue_key, author, body, target, created_at)
		values ($1, null, $2, 'Directly.', 'session:' || $3, $4)
	`, seeded.direct, alice, seeded.session, seeded.at); err != nil {
		t.Fatalf("seed the direct message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (id, issue_key, author, body, target, in_reply_to, created_at)
		values ($1, null, jsonb_build_object('kind', 'session', 'id', $2::text), 'Answered.', 'session:' || $2, $3, $4)
	`, seeded.reply, seeded.session, seeded.direct, seeded.at.Add(time.Minute)); err != nil {
		t.Fatalf("seed the session's reply: %v", err)
	}
	// The comment writer of the build before stored the node with no project, then the row that
	// names it.
	node, tuples := graphstore.CommentFacts(nil, seeded.comment, model.Actor{Kind: "user", ID: "alice"}, seeded.at,
		graphstore.NoEvent, "", "", "", nil)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, created_at, node_kind, node_address)
			values ($1::uuid, 'BACK-1', $2, 'A remark.', $3, 'comment', $1)
		`, seeded.comment, alice, seeded.at)
		return err
	}); err != nil {
		t.Fatalf("seed the comment with a node naming no project: %v", err)
	}
	return seeded
}
