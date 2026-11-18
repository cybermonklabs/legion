package store_test

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// interactionNodesMigration is the migration that gives every run recorded before it its node
// (20261116205055_interaction_nodes.up.sql).
const interactionNodesMigration = 20261116205055

// TestTheInteractionMigrationGivesEveryRecordedRunItsNode runs migration 20261116205055 over a
// database that holds runs as the release before records them: going_requests rows and their
// going_events rows, with no interaction node and no frame value. The runs are asked by a person's
// cookie (a login recorded in another spelling than its canonical one), by a session a broker's
// token proved its owner's, by a personal token whose act claims a session, by a person whose act
// names a session the server proved, and by nobody, the deployment's shared token. The migration
// writes each run's node, reserved for its author and live, with its words, its ends_at as Go's
// RFC 3339 text and its project; the authored_by tuple and the author's node where AuthorOf names
// one, the aliases the server proved and the raw login it was recorded under; and a frame value of
// each frame. A run whose author is nobody gets the node and no authored_by. One more run, asked
// after the migration with the same row as one before it, is given its node by graphcensus.WriteRun,
// the backfill's and the frame path's writer, and the two nodes and their tuples read the same,
// byte for byte, but for the run's id. The rest of the set is then migrated, since the census is
// this build's and reads the whole of its schema, and the store census reads zero differences.
func TestTheInteractionMigrationGivesEveryRecordedRunItsNode(t *testing.T) {
	ctx := context.Background()
	database := store.OpenEmptyTestStore(t)
	store.MigrateThrough(t, database, interactionNodesMigration-1)

	type run struct {
		name, author, project, endsAt string
		frames                        []string
		authoredBy                    string
	}
	runs := []run{
		{"a person's cookie", `{"kind": "user", "id": "Alice"}`, "CORE", "2026-11-16 10:00:00.1234+00",
			[]string{"Half an ", "answer."}, "person:alice"},
		{"a session proved its owner's", `{"kind": "session", "id": "sess-carol", "owner": "Carol", "act": {"session": "sess-carol"}}`,
			"", "2026-11-16 10:00:01+00", []string{"One."}, "session:sess-carol"},
		{"a personal token", `{"kind": "session", "id": "", "owner": "dave", "act": {"session": "claimed-in-a-body", "token_id": "tok-1"}}`,
			"", "2026-11-16 10:00:02.5+00", nil, "person:dave"},
		{"a person whose act names a session the server proved", `{"kind": "user", "id": "erin", "act": {"session": "sess-erin"}}`,
			"", "2026-11-16 10:00:03.000001+00", []string{"Two."}, "person:erin"},
		{"nobody", `{}`, "", "2026-11-16 10:00:04+00", []string{"Three."}, ""},
	}
	ids := make([]string, len(runs))
	insertRun := func(written run) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into going_requests (author, project, words, created_at, ends_at)
			values ($1::jsonb, $2, 'What does ' || $3 || ' ask?', '2026-11-16 09:59:30+00', $4::timestamptz)
			returning id::text
		`, written.author, written.project, written.name, written.endsAt).Scan(&id); err != nil {
			t.Fatalf("record the run of %s as the release before does: %v", written.name, err)
		}
		for index, frame := range written.frames {
			if _, err := database.Pool.Exec(ctx, `
				insert into going_events (request_id, sequence, name, data, created_at)
				values ($1::uuid, $2, 'text', to_jsonb($3::text), '2026-11-16 09:59:40+00'::timestamptz + make_interval(secs => $4))
			`, id, index+1, frame, float64(index+1)); err != nil {
				t.Fatalf("append frame %d of the run of %s: %v", index+1, written.name, err)
			}
		}
		return id
	}
	for index, written := range runs {
		ids[index] = insertRun(written)
	}

	store.MigrateThrough(t, database, interactionNodesMigration)

	for index, written := range runs {
		id := ids[index]
		node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindInteraction, id)
		if err != nil || !held {
			t.Fatalf("the run of %s reads no node after the migration (held %t, %v)", written.name, held, err)
		}
		var endsAt time.Time
		if err := database.Pool.QueryRow(ctx, `select ends_at from going_requests where id = $1::uuid`, id).Scan(&endsAt); err != nil {
			t.Fatalf("read the ends_at of the run of %s: %v", written.name, err)
		}
		if node.Visibility != graphstore.VisibilityAuthor || node.State != graphstore.StateLive || node.Project != written.project ||
			node.Attributes[graphstore.AttributeEndsAt] != endsAt.UTC().Format(time.RFC3339Nano) ||
			node.Attributes[graphstore.AttributeWords] != "What does "+written.name+" ask?" {
			t.Errorf("the run of %s reads node %+v, want it reserved for its author, live, in %q, ending %s", written.name, node,
				written.project, endsAt.UTC().Format(time.RFC3339Nano))
		}
		authors, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindInteraction, id, []string{graphstore.TupleAuthoredBy})
		if err != nil {
			t.Fatalf("read the authored_by of the run of %s: %v", written.name, err)
		}
		var named []string
		for _, author := range authors {
			named = append(named, author.ToKind+":"+author.ToID)
		}
		if want := slices.DeleteFunc([]string{written.authoredBy}, func(s string) bool { return s == "" }); !slices.Equal(named, want) {
			t.Errorf("the run of %s states authored_by %v, want %v", written.name, named, want)
		}
		var frames, values int
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from going_events where request_id = $1::uuid),
			       (select count(*) from going_events e where e.request_id = $1::uuid and exists (
			          select 1 from graph_tuples v where v.from_kind = $2 and v.from_id = $5 and v.kind = $3 and v.to_kind = $4
			            and v.to_id = jsonb_build_object('sequence', e.sequence, 'name', e.name, 'data', e.data)::text
			            and v.at = e.created_at and v.who = '{"kind": "system", "id": "going"}'::jsonb))
		`, id, graphstore.KindInteraction, graphstore.AttributeFrame, graphstore.KindValue, id).Scan(&frames, &values); err != nil {
			t.Fatalf("count the frames of the run of %s: %v", written.name, err)
		}
		if frames != len(written.frames) || values != frames {
			t.Errorf("the run of %s holds %d frames and %d frame values stating them, want %d of each", written.name, frames, values, len(written.frames))
		}
	}

	rows, err := database.Pool.Query(ctx, `
		select from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id from graph_tuples
		where kind in ($1, $2) order by 1`, graphstore.TupleAliasOf, graphstore.TupleRecordedAs)
	if err != nil {
		t.Fatalf("read the alias tuples: %v", err)
	}
	facts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the alias tuples: %v", err)
	}
	want := []string{"person:alice recorded_as value:Alice", "session:sess-carol alias_of person:carol", "session:sess-erin alias_of person:erin"}
	if !slices.Equal(facts, want) {
		t.Errorf("the migration states the aliases %v, want %v", facts, want)
	}
	var people, sessions []string
	for kind, into := range map[string]*[]string{graphstore.KindPerson: &people, graphstore.KindSession: &sessions} {
		nodes, err := graphstore.NodesOfKind(ctx, database.Pool, kind)
		if err != nil {
			t.Fatalf("read the %s nodes: %v", kind, err)
		}
		for _, node := range nodes {
			*into = append(*into, node.ID)
		}
	}
	if !slices.Equal(people, []string{"alice", "carol", "dave", "erin"}) || !slices.Equal(sessions, []string{"sess-carol"}) {
		t.Errorf("the migration made person nodes %v and session nodes %v, want alice, carol, dave and erin, and sess-carol", people, sessions)
	}

	// The same row recorded after the migration, given its node by the writer every later run of
	// such a row goes through, reads the same as the migration's.
	after := insertRun(runs[0])
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the node of the run asked after the migration: %v", err)
	}
	if held, err := graphcensus.WriteRun(ctx, tx, after); err != nil || !held {
		_ = tx.Rollback(ctx)
		t.Fatalf("write the node of the run asked after the migration: held %t, %v", held, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the node of the run asked after the migration: %v", err)
	}
	shape := func(id string) string {
		t.Helper()
		var node string
		if err := database.Pool.QueryRow(ctx, `
			select concat_ws(' | ', supertag, visibility, state, attributes::text, made_at::text, made_by::text, source_event,
			                 project_key, parties::text)
			from graph_nodes where kind = $1 and id = $2`, graphstore.KindInteraction, id).Scan(&node); err != nil {
			t.Fatalf("read the node of run %s: %v", id, err)
		}
		rows, err := database.Pool.Query(ctx, `
			select concat_ws(' | ', kind, to_kind, to_id, at::text, who::text, source_event, legacy_source_seq, asserted)
			from graph_tuples where from_kind = $1 and from_id = $2 order by 1`, graphstore.KindInteraction, id)
		if err != nil {
			t.Fatalf("read the tuples of run %s: %v", id, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the tuples of run %s: %v", id, err)
		}
		return strings.Join(append([]string{node}, lines...), "\n")
	}
	if migrated, written := shape(ids[0]), shape(after); migrated != written {
		t.Errorf("the run the migration gave its node reads\n%s\nand the same row given its node by graphcensus.WriteRun reads\n%s", migrated, written)
	}

	// This build's census reads the schema every migration of this build gives, the columns of
	// migrations later than this one among them (document_block_versions.store_owed_at), so the
	// rest are applied before it; none of them writes a run, a frame or a node of either.
	store.MigrateThrough(t, database, math.MaxInt)
	census, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census after the migration reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	for _, kind := range census.Kinds {
		switch kind.Kind {
		case graphstore.KindInteraction, "frame", graphstore.KindPerson, graphstore.KindSession, "interaction_visibility_state":
			t.Logf("census kind=%s rows=%d nodes=%d", kind.Kind, kind.Rows, kind.Nodes)
		}
	}
	if census.Differences() != 0 {
		t.Errorf("the census after the migration reads %d difference(s), want none", census.Differences())
	}
}
