package graphcensus

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// The three risks seedEndedPreventions writes, each prevented by one check: one of a document a
// process of the release before retired, one that process's version dropped, and one that stands.
const (
	retiredCheckRisk = "risk-of-a-check-of-a-retired-document"
	goneCheckRisk    = "risk-of-a-check-a-version-dropped"
	keptCheckRisk    = "risk-of-a-check-that-stands"
)

// endedPreventions is what seedEndedPreventions wrote: the issue the documents stand under and the
// address of each check.
type endedPreventions struct {
	issue                              string
	retiredCheck, goneCheck, keptCheck string
}

// seedEndedPreventions writes, under project PREV and its issue PREV-1, what a process of the
// release before leaves of the prevented_by tuples this build wrote, during a roll or after a
// rollback. This build wrote a risk prevented by a check of the document verification and two
// risks prevented by the checks dropped and kept of the document notes, before the roll. The
// process of the release before then retired verification and dropped the check dropped from
// notes, as it does both: it set the document's retired_at, stated the dropped block's node gone,
// and retracted neither prevented_by. Each of those rows is written by SQL as that process leaves
// it; the nodes are this build's, written through the store's writer.
func seedEndedPreventions(t *testing.T, database *store.Store) endedPreventions {
	t.Helper()
	const project = "PREV"
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	seed := endedPreventions{issue: project + "-1"}
	exec("create the project", `insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`, project)
	exec("create the issue", `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'Checks nobody runs', '{"kind":"user","id":"alice"}', 'n')
	`, seed.issue, project)
	document := func(slug string) (id, refKey string) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, $2, $3, $3 || '.md', 'doc', '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, seed.issue, project, slug).Scan(&id, &refKey); err != nil {
			t.Fatalf("create the document %s: %v", slug, err)
		}
		return id, refKey
	}
	retiredID, retiredKey := document("verification")
	_, notesKey := document("notes")
	seed.retiredCheck, seed.goneCheck, seed.keptCheck = retiredKey+"#chk", notesKey+"#dropped", notesKey+"#kept"

	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 18, 9, 46, 0, 0, time.UTC)
	node := func(kind, id string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			Project: project, MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			node(refs.BlockNodeKind, seed.retiredCheck), node(refs.BlockNodeKind, seed.goneCheck), node(refs.BlockNodeKind, seed.keptCheck),
			node(graphstore.KindRisk, retiredCheckRisk), node(graphstore.KindRisk, goneCheckRisk), node(graphstore.KindRisk, keptCheckRisk),
		}, nil)
	}); err != nil {
		t.Fatalf("write the checks and the risks: %v", err)
	}
	exec("assert each risk's prevented_by", `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select $1::text, p.risk, $2::text, $3::text, p.check_id, $4::timestamptz, '{"kind":"user","id":"alice"}'::jsonb, 0, true
		from unnest($5::text[], $6::text[]) as p (risk, check_id)
	`, graphstore.KindRisk, graphstore.TuplePreventedBy, refs.BlockNodeKind, at,
		[]string{retiredCheckRisk, goneCheckRisk, keptCheckRisk}, []string{seed.retiredCheck, seed.goneCheck, seed.keptCheck})
	exec("retire verification as the release before retires it", `
		update artifacts set retired_at = now(), role = null where id = $1::uuid
	`, retiredID)
	exec("drop the check from notes as the release before drops it", `
		update graph_nodes set state = $1 where kind = $2 and id = $3
	`, graphstore.StateGone, refs.BlockNodeKind, seed.goneCheck)
	return seed
}

// preventedBy is the far end of each prevented_by risk states now.
func preventedBy(t *testing.T, database *store.Store, risk string) []string {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindRisk, risk, []string{graphstore.TuplePreventedBy})
	if err != nil {
		t.Fatalf("read the prevented_by of %s: %v", risk, err)
	}
	ends := []string{}
	for _, tuple := range held {
		ends = append(ends, tuple.ToID)
	}
	return ends
}

// retractedPreventions counts the prevented_by retractions the store holds, of every risk.
func retractedPreventions(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var retracted int64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where kind = $1 and not asserted
	`, graphstore.TuplePreventedBy).Scan(&retracted); err != nil {
		t.Fatalf("count the prevented_by retractions: %v", err)
	}
	return retracted
}

// TestTheBackfillRetractsThePreventedByOfChecksNobodyRuns holds the backfill to what this build
// retracts and a process of the release before does not: a prevented_by at a check of a document
// it retired, and one at a check its version dropped (seedEndedPreventions). The backfill retracts
// both, signed by the run and caused by no event, and leaves the prevented_by at the check that
// stands. A second backfill finds nothing more to retract and retracts nothing.
func TestTheBackfillRetractsThePreventedByOfChecksNobodyRuns(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seed := seedEndedPreventions(t, database)

	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	if done := reportedKind(t, report, "prevented_by_retracted"); done.Rows != 2 || done.Tuples != 2 || done.Remaining != 0 || !done.Complete {
		t.Errorf("the backfill reports %#v for the prevented_by at checks nobody runs, want 2 retracted and none remaining", done)
	}
	for _, risk := range []string{retiredCheckRisk, goneCheckRisk} {
		if ends := preventedBy(t, database, risk); len(ends) != 0 {
			t.Errorf("after the backfill %s is prevented by %v, want nothing", risk, ends)
		}
	}
	if ends := preventedBy(t, database, keptCheckRisk); !slices.Equal(ends, []string{seed.keptCheck}) {
		t.Errorf("after the backfill %s is prevented by %v, want the check that stands, %s", keptCheckRisk, ends, seed.keptCheck)
	}
	for risk, check := range map[string]string{retiredCheckRisk: seed.retiredCheck, goneCheckRisk: seed.goneCheck} {
		history, err := graphstore.History(ctx, database.Pool, graphstore.KindRisk, risk, graphstore.TuplePreventedBy)
		if err != nil {
			t.Fatalf("read the prevented_by history of %s: %v", risk, err)
		}
		var retractions []graphstore.Tuple
		for _, tuple := range history {
			if !tuple.Asserted {
				retractions = append(retractions, tuple)
			}
		}
		if len(retractions) != 1 {
			t.Fatalf("the prevented_by history of %s is %+v, want one retraction", risk, history)
		}
		retraction := retractions[0]
		t.Logf("%s: the retraction of its prevented_by at %s, at %s by %+v under event %d", risk, retraction.ToID, retraction.At, retraction.Who, retraction.SourceEvent)
		if retraction.ToKind != refs.BlockNodeKind || retraction.ToID != check || !retraction.Who.SameCredential(backfillActor) ||
			retraction.SourceEvent != graphstore.NoEvent {
			t.Errorf("%s's prevented_by was retracted as %+v, want at %s, by %+v, under no event", risk, retraction, check, backfillActor)
		}
	}
	if retracted := retractedPreventions(t, database); retracted != 2 {
		t.Errorf("the store holds %d prevented_by retractions after the backfill, want 2", retracted)
	}

	second, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store again: %v", err)
	}
	if done := reportedKind(t, second, "prevented_by_retracted"); done.Rows != 0 || done.Tuples != 0 || done.Remaining != 0 || !done.Complete {
		t.Errorf("the second backfill reports %#v for the prevented_by at checks nobody runs, want nothing retracted and nothing remaining", done)
	}
	if retracted := retractedPreventions(t, database); retracted != 2 {
		t.Errorf("the store holds %d prevented_by retractions after the second backfill, want the first run's 2 and no more", retracted)
	}
}

// The two risks seedRoomCheckPreventions writes, each prevented by one check of an issue room: one
// of the document a process of the release before retired, and one of the document that stands.
const (
	roomRetiredCheckRisk = "risk-of-a-room-check-of-a-retired-document"
	roomKeptCheckRisk    = "risk-of-a-room-check-that-stands"
)

// seedRoomCheckPreventions writes, under project ROOM and its issue ROOM-1, what the block index of
// an issue born converted leaves of two checks of the issue's room: the tree artifact, the room's
// documents verification-md and notes joined to it, each check's document_blocks row keyed by the
// tree, k9 of verification-md and k1 of notes, and each check's issue_room_blocks row naming the
// document whose fragment holds it. This build wrote a risk prevented by each check, at the
// address the tuple stores: the tree's ref_key, '#' and the block id. A process of the release
// before then retired verification-md, setting its retired_at and retracting no prevented_by. It
// answers the address of each check.
func seedRoomCheckPreventions(t *testing.T, database *store.Store) (retiredCheck, keptCheck string) {
	t.Helper()
	const project, issue = "ROOM", "ROOM-1"
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("create the project", `insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`, project)
	exec("create the issue", `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'Checks of a room', '{"kind":"user","id":"alice"}', 'n')
	`, issue, project)
	artifact := func(slug, kind string) (id, refKey string) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, $2, $3, $3 || '.md', $4, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, issue, project, slug, kind).Scan(&id, &refKey); err != nil {
			t.Fatalf("create the artifact %s: %v", slug, err)
		}
		return id, refKey
	}
	treeID, treeKey := artifact("tree", "tree")
	verificationID, _ := artifact("verification-md", "doc")
	notesID, _ := artifact("notes", "doc")
	retiredCheck, keptCheck = treeKey+"#k9", treeKey+"#k1"
	exec("record the issue's tree", `insert into issue_trees (issue_key, artifact_id) values ($1, $2::uuid)`, issue, treeID)
	exec("join the documents to the room", `
		insert into document_rooms (artifact_id, room) values ($1::uuid, $3::uuid), ($2::uuid, $3::uuid)
	`, verificationID, notesID, treeID)

	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	node := func(kind, id string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			Project: project, MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			node(refs.BlockNodeKind, retiredCheck), node(refs.BlockNodeKind, keptCheck),
			node(graphstore.KindRisk, roomRetiredCheckRisk), node(graphstore.KindRisk, roomKeptCheckRisk),
		}, nil)
	}); err != nil {
		t.Fatalf("write the checks and the risks: %v", err)
	}
	exec("index the room's checks under its tree", `
		insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'k9', $2, $4, $2, 'check', '', '', 1, now()), ($1::uuid, 'k1', $3, $4, $3, 'check', '', '', 1, now())
	`, treeID, retiredCheck, keptCheck, refs.BlockNodeKind)
	exec("record the document whose fragment holds each check", `
		insert into issue_room_blocks (tree_artifact_id, block_id, document_id)
		values ($1::uuid, 'k9', $2::uuid), ($1::uuid, 'k1', $3::uuid)
	`, treeID, verificationID, notesID)
	exec("assert each risk's prevented_by", `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select $1::text, p.risk, $2::text, $3::text, p.check_id, $4::timestamptz, '{"kind":"user","id":"alice"}'::jsonb, 0, true
		from unnest($5::text[], $6::text[]) as p (risk, check_id)
	`, graphstore.KindRisk, graphstore.TuplePreventedBy, refs.BlockNodeKind, at,
		[]string{roomRetiredCheckRisk, roomKeptCheckRisk}, []string{retiredCheck, keptCheck})
	exec("retire verification-md as the release before retires it", `
		update artifacts set retired_at = now(), role = null where id = $1::uuid
	`, verificationID)
	return retiredCheck, keptCheck
}

// TestTheBackfillRetractsThePreventedByAtARoomCheckOfARetiredDocument holds the backfill to a check
// of an issue room (seedRoomCheckPreventions): its row and its address are the room's tree
// artifact's, which stands, and the document whose fragment holds it is the one retired, so the
// backfill judges the check by that document. It retracts the prevented_by at the check of the
// retired document, signed by the run and caused by no event, and leaves the one at the check of
// the document that stands; a second pass retracts nothing. The test runs the pass alone
// (backfillEndedPreventions), as requireTheEndedPreventionsLockOrder does, so no other pass of
// Backfill reads the room the seed made by hand.
func TestTheBackfillRetractsThePreventedByAtARoomCheckOfARetiredDocument(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	retiredCheck, keptCheck := seedRoomCheckPreventions(t, database)

	done, err := backfillEndedPreventions(ctx, database)
	if err != nil {
		t.Fatalf("backfill the prevented_by at checks nobody runs: %v", err)
	}
	if done.Rows != 1 || done.Tuples != 1 || done.Remaining != 0 || !done.Complete {
		t.Errorf("the backfill reports %#v for the prevented_by at checks nobody runs, want 1 retracted and none remaining", done)
	}
	if ends := preventedBy(t, database, roomRetiredCheckRisk); len(ends) != 0 {
		t.Errorf("after the backfill %s is prevented by %v, want nothing: %s is a check of the retired verification-md", roomRetiredCheckRisk, ends, retiredCheck)
	}
	if ends := preventedBy(t, database, roomKeptCheckRisk); !slices.Equal(ends, []string{keptCheck}) {
		t.Errorf("after the backfill %s is prevented by %v, want the check of notes, which stands, %s", roomKeptCheckRisk, ends, keptCheck)
	}
	history, err := graphstore.History(ctx, database.Pool, graphstore.KindRisk, roomRetiredCheckRisk, graphstore.TuplePreventedBy)
	if err != nil {
		t.Fatalf("read the prevented_by history of %s: %v", roomRetiredCheckRisk, err)
	}
	if last := len(history) - 1; len(history) != 2 || history[last].Asserted || history[last].ToID != retiredCheck ||
		!history[last].Who.SameCredential(backfillActor) || history[last].SourceEvent != graphstore.NoEvent {
		t.Errorf("the prevented_by history of %s is %+v, want its assertion and then its retraction at %s by %+v under no event",
			roomRetiredCheckRisk, history, retiredCheck, backfillActor)
	}

	second, err := backfillEndedPreventions(ctx, database)
	if err != nil {
		t.Fatalf("backfill the prevented_by at checks nobody runs again: %v", err)
	}
	if second.Rows != 0 || second.Tuples != 0 || second.Remaining != 0 || !second.Complete {
		t.Errorf("the second backfill reports %#v for the prevented_by at checks nobody runs, want nothing retracted and nothing remaining", second)
	}
}

// requireTheEndedPreventionsLockOrder holds writeEndedPreventions to its steps. The checks of both
// documents stand under issue PREV-1, whose owner lock is the owner lock of each: the batch takes
// it first, as the index pass that drops a check and the retirement of its document take it, and
// while it waits there it has read nothing of the store and appended nothing. It then appends the
// retractions through the store's writer, which takes no node row, so it never queues behind the
// rows of the risks or of their checks, held `for update` for the whole run, and it appends no
// event. The test follows the pass alone (backfillEndedPreventions), so a lock another pass of
// Backfill takes is not read as this batch's.
func requireTheEndedPreventionsLockOrder(t *testing.T) {
	database := storetest.Open(t)
	seed := seedEndedPreventions(t, database)
	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue " + seed.issue + "'s owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, store.IssueOwner(seed.issue))
	holds := []*lockstep.Hold{owner}
	for _, held := range []struct{ kind, id string }{
		{refs.BlockNodeKind, seed.goneCheck}, {refs.BlockNodeKind, seed.retiredCheck},
		{graphstore.KindRisk, goneCheckRisk}, {graphstore.KindRisk, retiredCheckRisk},
	} {
		holds = append(holds, lockstep.HoldRow(t, database, &lockstep.Hold{Name: "the node row of " + held.kind + " " + held.id, Step: lockstep.StepNode},
			`select 1 from graph_nodes where kind = $1 and id = $2 for update`, held.kind, held.id))
	}
	holds = append(holds, lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project PREV's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "PREV"))

	report := followTheRun(t, database, "writeEndedPreventions", func(ctx context.Context, database *store.Store) (BackfillReport, error) {
		done, err := backfillEndedPreventions(ctx, database)
		return BackfillReport{Kinds: []KindBackfill{done}}, err
	}, holds)
	if done := reportedKind(t, report, "prevented_by_retracted"); done.Rows != 2 || !done.Complete {
		t.Errorf("the backfill reports %#v for the prevented_by at checks nobody runs, want 2 and none remaining", done)
	}
	for _, risk := range []string{retiredCheckRisk, goneCheckRisk} {
		if ends := preventedBy(t, database, risk); len(ends) != 0 {
			t.Errorf("after the backfill %s is prevented by %v, want nothing: the run the test followed is not one that committed", risk, ends)
		}
	}
}
