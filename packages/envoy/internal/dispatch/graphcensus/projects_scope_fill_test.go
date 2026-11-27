package graphcensus

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// scopeFillNode is one node a build before a node named its project stored with neither a project
// nor parties: its address, the base attribute the scope fill must state of it (project or
// parties), and the value its row gives that attribute.
type scopeFillNode struct {
	kind, id, attribute, value string
}

// scopeFillHistory is the seqs of a node's visibility and state statements, which the scope fill
// must leave as they are: the log is append-only, so the same seqs are the same statements.
type scopeFillHistory struct {
	visibility, state []int64
}

// The scope fill (graphstore.FillScopes) states a node's project or parties as the tuple the
// node's own write would have stated, not only as the column that projects it. Two comment nodes
// are seeded as a build before a node named its project stored them: one of a comment on an
// issue, whose project is the issue's, caused by its comment.created event; one of a comment on a
// file of an agent's conversation, whose parties are that conversation's session, at the
// sentinel, made by another writer a minute later. Neither names a project or parties in its
// columns or its log. The backfill then gives each its scope: the column, and exactly one tuple of
// the attribute, which graphstore.CurrentValue reads, at the node's own made_at, by its made_by
// and under its source_event, and nothing of its visibility or its state moves. A second run
// appends nothing.
//
// Its control is the tuple append taken out of FillScopes: the column is still filled, so the
// project census reads every comment complete, but CurrentValue reads no project and no parties.
func TestTheScopeFillStatesTheProjectAsATupleNotOnlyTheColumn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 2, 7, 30, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	const session = "01a1e0fe-90a3-76fa-b47a-7d62f39b4601"
	const owned = "22222222-2222-4222-8222-000000000001"
	const conversed = "22222222-2222-4222-8222-000000000002"

	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('SCOP', 'Scopes', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank, created_at)
		values ('SCOP-1', 'SCOP', 1, 'One', '{"kind":"user","id":"alice"}', 'U', '2026-11-02 07:00:00+00');
	`); err != nil {
		t.Fatalf("seed the project and the issue: %v", err)
	}
	var file, fileRef string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by, created_at)
		values ($1, 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', $2)
		returning id::text, ref_key
	`, session, at).Scan(&file, &fileRef); err != nil {
		t.Fatalf("seed the conversation's file: %v", err)
	}
	var event int64
	if err := database.Pool.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ('SCOP-1', 1, 'comment.created', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $1::text), false)
		returning id
	`, owned).Scan(&event); err != nil {
		t.Fatalf("append the issue comment's comment.created event: %v", err)
	}
	// The comment writer of the build before stored each node with no project and no parties,
	// then the row that names it.
	ownedNode, tuples := graphstore.CommentFacts(nil, owned, alice, at, event, "", "", "", nil)
	conversedNode, tuples := graphstore.CommentFacts(tuples, conversed, bob, at.Add(time.Minute), graphstore.NoEvent, "", fileRef, "", nil)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{ownedNode, conversedNode}, tuples); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, created_at, node_kind, node_address)
			values ($1::uuid, 'SCOP-1', '{"kind":"user","id":"alice"}', 'On the issue.', $2, 'comment', $1)
		`, owned, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into comments (id, artifact_id, author, body, created_at, node_kind, node_address)
			values ($1::uuid, $2::uuid, '{"kind":"user","id":"bob"}', 'In the conversation.', $3, 'comment', $1)
		`, conversed, file, at.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("seed the two comments with nodes naming no scope: %v", err)
	}

	nodes := []scopeFillNode{
		{graphstore.KindComment, owned, graphstore.AttributeProject, "SCOP"},
		{graphstore.KindComment, conversed, graphstore.AttributeParties, graphstore.PartySession(session)},
	}
	held := make([]graphstore.Node, len(nodes))
	before := make([]scopeFillHistory, len(nodes))
	for index, node := range nodes {
		held[index], before[index] = scopeFillBefore(t, database, node)
	}
	if held[0].SourceEvent != event || held[1].SourceEvent != graphstore.NoEvent {
		t.Fatalf("the seeded nodes are caused by %d and %d, want %d and the sentinel", held[0].SourceEvent, held[1].SourceEvent, event)
	}
	if comments := scopeFillCensus(t, database, graphstore.KindComment); comments.Rows != 2 || comments.Unscoped != 2 {
		t.Fatalf("before the run the project census reads %+v for comments, want 2 rows, both unscoped", comments)
	}

	report, err := Backfill(ctx, database, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	for index, node := range nodes {
		requireScopeFilled(t, database, node, held[index], before[index])
	}
	if comments := scopeFillCensus(t, database, graphstore.KindComment); comments.Rows != 2 || comments.Findings() != 0 {
		t.Errorf("after the run the project census reads %+v for comments, want 2 rows and no finding", comments)
	}
	if filled := scopeFillReported(t, report, graphstore.KindComment+"_project"); filled.Nodes != 2 {
		t.Errorf("the backfill reports %+v for the comment scopes, want 2 nodes filled", filled)
	}

	// A second run reads no node the first filled: it fills nothing and appends nothing.
	tuplesBefore := scopeFillTuples(t, database)
	second, err := Backfill(ctx, database, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	if filled := scopeFillReported(t, second, graphstore.KindComment+"_project"); filled.Rows != 0 || filled.Nodes != 0 {
		t.Errorf("the second run reports %+v for the comment scopes, want nothing filled", filled)
	}
	if appended := scopeFillTuples(t, database) - tuplesBefore; appended != 0 {
		t.Errorf("the second run appended %d tuples, want none", appended)
	}
	for index, node := range nodes {
		requireScopeFilled(t, database, node, held[index], before[index])
	}
}

// An existing block node a build before a node named its project stored with neither a project
// nor parties, and no scope tuple, is given its document's project by the backfill: its column and
// its one project tuple, at the node's own made_at, by its made_by and under its source_event.
// Its state history stays as it was: one block's is the single live statement it was made with,
// the other's that statement, a gone one an hour later and a live one an hour after that, so a
// fill dated at the node's newest statement rather than its made_at would show too. The project
// census then reads both block rows and no finding.
//
// Its control is the blocks left out of the scope fill's kinds (scopeKinds): the block nodes keep
// naming no project, and the census reads them unscoped.
func TestTheScopeFillGivesAnExistingBlockNodeItsDocumentsProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedIssueDocument(t, database, "BLKS", "33333333-3333-4333-8333-000000000001", "First.\n\nSecond.\n")
	made := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	blockIDs := []string{"first", "second"}
	blockNode := func(address, state string, at time.Time) graphstore.Node {
		return graphstore.Node{
			Kind: refs.BlockNodeKind, ID: address, Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: state,
			MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	nodes := make([]scopeFillNode, len(blockIDs))
	written := make([]graphstore.Node, len(blockIDs))
	var memberships []graphstore.Tuple
	for index, blockID := range blockIDs {
		address := document.refKey + "#" + blockID
		nodes[index] = scopeFillNode{refs.BlockNodeKind, address, graphstore.AttributeProject, "BLKS"}
		written[index] = blockNode(address, graphstore.StateLive, made)
		memberships = append(memberships, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.TupleInDocument,
			ToKind: "artifact", ToID: document.refKey,
			At: made, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	second := nodes[1].id
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, written, memberships); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{blockNode(second, graphstore.StateGone, made.Add(time.Hour))}, nil); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{blockNode(second, graphstore.StateLive, made.Add(2*time.Hour))}, nil); err != nil {
			return err
		}
		for index, blockID := range blockIDs {
			if _, err := tx.Exec(ctx, `
				insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at, node_kind, node_address)
				values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, $5, $6, $3)
			`, document.artifactID, blockID, nodes[index].id, "Block "+blockID+".", made, refs.BlockNodeKind); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the block nodes naming no project and the rows that name them: %v", err)
	}

	held := make([]graphstore.Node, len(nodes))
	before := make([]scopeFillHistory, len(nodes))
	for index, node := range nodes {
		held[index], before[index] = scopeFillBefore(t, database, node)
	}
	if len(before[1].state) != 3 {
		t.Fatalf("the second block holds %d state statements, want the 3 the seed wrote", len(before[1].state))
	}
	if blocks := scopeFillCensus(t, database, refs.BlockNodeKind); blocks.Rows != 2 || blocks.Unscoped != 2 {
		t.Fatalf("before the run the project census reads %+v for blocks, want 2 rows, both unscoped", blocks)
	}

	report, err := Backfill(ctx, database, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	for index, node := range nodes {
		requireScopeFilled(t, database, node, held[index], before[index])
	}
	if blocks := scopeFillCensus(t, database, refs.BlockNodeKind); blocks.Rows != 2 || blocks.Findings() != 0 {
		t.Errorf("after the run the project census reads %+v for blocks, want 2 rows and no finding", blocks)
	}
	if filled := scopeFillReported(t, report, refs.BlockNodeKind+"_project"); filled.Nodes != 2 {
		t.Errorf("the backfill reports %+v for the block scopes, want 2 nodes filled", filled)
	}
}

// scopeFillBefore reads node as the build before a node named its project left it, and fails
// unless it names neither a project nor parties, in its columns or in its log, and holds a
// visibility and a state statement to compare. It answers the node and those statements' seqs.
func scopeFillBefore(t *testing.T, database *store.Store, node scopeFillNode) (graphstore.Node, scopeFillHistory) {
	t.Helper()
	held, found, err := graphstore.ReadNode(context.Background(), database.Pool, node.kind, node.id)
	if err != nil || !found {
		t.Fatalf("read the seeded node %s/%s: found %t, %v", node.kind, node.id, found, err)
	}
	if held.Project != "" || len(held.Parties) != 0 {
		t.Fatalf("the seeded node %s/%s carries project %q and parties %v, want neither", node.kind, node.id, held.Project, held.Parties)
	}
	for _, attribute := range []string{graphstore.AttributeProject, graphstore.AttributeParties} {
		if stated := scopeFillSeqs(t, database, node, attribute); len(stated) != 0 {
			t.Fatalf("the seeded node %s/%s holds %s tuples %v, want none", node.kind, node.id, attribute, stated)
		}
	}
	history := scopeFillHistory{
		visibility: scopeFillSeqs(t, database, node, graphstore.AttributeVisibility),
		state:      scopeFillSeqs(t, database, node, graphstore.AttributeState),
	}
	if len(history.visibility) == 0 || len(history.state) == 0 {
		t.Fatalf("the seeded node %s/%s holds visibility %v and state %v, so the test would compare nothing",
			node.kind, node.id, history.visibility, history.state)
	}
	return held, history
}

// requireScopeFilled fails unless node carries its row's scope in its column and as exactly one
// tuple of its attribute, which graphstore.CurrentValue reads, stated at the node's own made_at, by
// its made_by and under its source_event as held read them before the run, with no tuple of the
// other scope attribute, and its visibility and state statements as before read them.
func requireScopeFilled(t *testing.T, database *store.Store, node scopeFillNode, held graphstore.Node, before scopeFillHistory) {
	t.Helper()
	ctx := context.Background()
	after, found, err := graphstore.ReadNode(ctx, database.Pool, node.kind, node.id)
	if err != nil || !found {
		t.Fatalf("read node %s/%s after the run: found %t, %v", node.kind, node.id, found, err)
	}
	column, other := after.Project, graphstore.AttributeParties
	if node.attribute == graphstore.AttributeParties {
		column, other = strings.Join(after.Parties, " "), graphstore.AttributeProject
	}
	if column != node.value {
		t.Errorf("node %s/%s carries %s %q in its column, want %q", node.kind, node.id, node.attribute, column, node.value)
	}
	value, stands, err := graphstore.CurrentValue(ctx, database.Pool, node.kind, node.id, node.attribute)
	if err != nil {
		t.Fatalf("read the %s of %s/%s: %v", node.attribute, node.kind, node.id, err)
	}
	if !stands || value != node.value {
		t.Errorf("CurrentValue reads %s %q (stands %t) of %s/%s, want %q: the log must state what the column projects",
			node.attribute, value, stands, node.kind, node.id, node.value)
	}
	stated, err := graphstore.History(ctx, database.Pool, node.kind, node.id, node.attribute)
	if err != nil {
		t.Fatalf("read the %s history of %s/%s: %v", node.attribute, node.kind, node.id, err)
	}
	if len(stated) != 1 {
		t.Errorf("node %s/%s holds %d %s tuples %+v, want exactly one", node.kind, node.id, len(stated), node.attribute, stated)
	} else if tuple := stated[0]; tuple.ToKind != graphstore.KindValue || tuple.ToID != node.value || !tuple.Asserted ||
		!tuple.At.Equal(held.MadeAt) || !reflect.DeepEqual(tuple.Who, held.MadeBy) || tuple.SourceEvent != held.SourceEvent {
		t.Errorf("node %s/%s's %s tuple is %+v; want the asserted value %q at its made_at %s, by its made_by %+v, under its source_event %d",
			node.kind, node.id, node.attribute, tuple, node.value, held.MadeAt, held.MadeBy, held.SourceEvent)
	}
	if extra := scopeFillSeqs(t, database, node, other); len(extra) != 0 {
		t.Errorf("node %s/%s holds %s tuples %v, want none", node.kind, node.id, other, extra)
	}
	for _, kept := range []struct {
		attribute string
		before    []int64
	}{{graphstore.AttributeVisibility, before.visibility}, {graphstore.AttributeState, before.state}} {
		if now := scopeFillSeqs(t, database, node, kept.attribute); !slices.Equal(now, kept.before) {
			t.Errorf("node %s/%s's %s statements are %v, want them unchanged at %v", node.kind, node.id, kept.attribute, now, kept.before)
		}
	}
}

// scopeFillSeqs is the seqs of node's tuples of kind, in the order the store appended them.
func scopeFillSeqs(t *testing.T, database *store.Store, node scopeFillNode, kind string) []int64 {
	t.Helper()
	stated, err := graphstore.History(context.Background(), database.Pool, node.kind, node.id, kind)
	if err != nil {
		t.Fatalf("read the %s history of %s/%s: %v", kind, node.kind, node.id, err)
	}
	seqs := make([]int64, len(stated))
	for index, tuple := range stated {
		seqs[index] = tuple.Seq
	}
	return seqs
}

// scopeFillCensus is the project census's report of kind.
func scopeFillCensus(t *testing.T, database *store.Store, kind string) ProjectKindReport {
	t.Helper()
	census, err := ProjectCensus(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("take the project census: %v", err)
	}
	for _, report := range census {
		if report.Kind == kind {
			return report
		}
	}
	t.Fatalf("the project census reads no %s kind: %+v", kind, census)
	return ProjectKindReport{}
}

// scopeFillReported is the backfill report's line of kind, or the zero line where the report holds
// none, which fails the test without stopping it: the nodes are read either way.
func scopeFillReported(t *testing.T, report BackfillReport, kind string) KindBackfill {
	t.Helper()
	for _, done := range report.Kinds {
		if done.Kind == kind {
			return done
		}
	}
	t.Errorf("the backfill reports no %s kind", kind)
	return KindBackfill{}
}

// scopeFillTuples is how many tuples the store holds.
func scopeFillTuples(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var tuples int64
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from graph_tuples`).Scan(&tuples); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	return tuples
}
