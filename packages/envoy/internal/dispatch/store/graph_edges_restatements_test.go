package store

import (
	"context"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

// Two of the migrations that restate graph_edges after 20261109215947: the one that adds the
// tuple arm's class `store` and the arm answering a finding's relations as `lens_block`, and the
// one that restates the view last, with every change of the restatements before it.
const (
	storeClassRestatement = "20261117020856_graph_edges_asks_and_store_class"
	lastRestatement       = "20261121104002_graph_edges_store_class_last"
)

// The ids of the rows writeEveryGraphEdgesArm writes that the tests read back.
const (
	restatedSpec            = "aaaaaaaa-0000-4000-8000-000000000001"
	restatedNotes           = "aaaaaaaa-0000-4000-8000-000000000002"
	restatedBlockAsk        = "bbbbbbbb-0000-4000-8000-000000000001"
	restatedOwnedAsk        = "bbbbbbbb-0000-4000-8000-000000000002"
	restatedPartedAsk       = "bbbbbbbb-0000-4000-8000-000000000003"
	restatedStoredAsk       = "bbbbbbbb-0000-4000-8000-000000000004"
	restatedStoredAnchored  = "cccccccc-0000-4000-8000-000000000006"
	restatedStoredOwned     = "cccccccc-0000-4000-8000-000000000007"
	restatedStoredBlock     = "CORE-1/spec#b2"
	restatedRequirement     = "CORE-1/spec#r1"
	restatedLens            = "eeeeeeee-0000-4000-8000-000000000001"
	restatedTriggeredLens   = "eeeeeeee-0000-4000-8000-000000000002"
	restatedGoneLens        = "eeeeeeee-0000-4000-8000-000000000003"
	restatedFinding         = "lens/" + restatedLens + "#f1"
	restatedRisk            = "ffffffff-0000-4000-8000-000000000001"
	restatedConstraint      = "ffffffff-0000-4000-8000-000000000002"
	restatedRun             = "ffffffff-0000-4000-8000-000000000003"
	restatedAuthor          = "alice@d.example"
	restatedStoredFollower  = "s-stored"
	restatedSession         = "s1"
	restatedNotesFirstBlock = "CORE/notes#b1"
)

// The last restatement of graph_edges (20261121104002) answers every row 20261117020856's view
// answered, and no other. The fixture gives every arm of the migrated view at least one row. It
// holds a tuple of each kind of the class `store` (changed_by_run, has_kind, triggered_by,
// constrained_by and prevented_by, and the part_of and authored_by of a lens, a risk, a
// constraint and an interaction), a finding's relations, which the arm after the tuple arm
// answers as `lens_block`, an ask's relations from the asks arms and from the tuple arm, and
// tuples neither view answers. The test reads the view under the migrated definition, then
// restates it with 20261117020856's own text in a transaction it rolls back, and reads it again.
// The two answers are equal row for row, so a database that ran 20261117020856 alone reads the
// same rows after it runs 20261121104002.
func TestTheLastGraphEdgesRestatementAnswersWhatTheStoreClassRestatementAnswered(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storeClassView, err := migrationFiles.ReadFile("migrations/" + storeClassRestatement + ".up.sql")
	if err != nil {
		t.Fatalf("read %s: %v", storeClassRestatement, err)
	}
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	writeEveryGraphEdgesArm(t, tx)
	requireEveryGraphEdgesArmAnswers(t, tx)
	migrated := graphEdgesInOrder(t, tx)

	if _, err := tx.Exec(ctx, string(storeClassView)); err != nil {
		t.Fatalf("restate graph_edges with %s: %v", storeClassRestatement, err)
	}
	restated := graphEdgesInOrder(t, tx)
	answered := graphEdgesAnswered(t, tx)
	for _, row := range [][5]string{
		// The tuple arm's classes that consult a projection row, each row naming its node.
		{"block", restatedStoredBlock, "part_of", "artifact", "CORE-1/spec"},
		{"block", restatedStoredBlock, "mentions", "issue", "CORE-2"},
		{"block", restatedStoredBlock, "depends_on", "issue", "CORE-1"},
		{"comment", restatedStoredAnchored, "anchored_to", "artifact", "CORE-1/spec"},
		{"comment", restatedStoredOwned, "owned_by", "artifact", "CORE/notes"},
		// An ask's relations, from the tuple arm for the ask whose row names its node and from
		// the asks and ask_followers arms for the asks whose rows name none.
		{"ask", restatedStoredAsk, "anchored_to", "artifact", "CORE/notes"},
		{"ask", restatedStoredAsk, "owned_by", "artifact", "CORE/notes"},
		{"ask", restatedStoredAsk, "followed_by", "session", restatedStoredFollower},
		{"ask", restatedStoredAsk, "shaped", "block", restatedNotesFirstBlock},
		{"ask", restatedBlockAsk, "anchored_to", "artifact", "CORE-1/spec"},
		{"ask", restatedOwnedAsk, "owned_by", "artifact", "CORE/notes"},
		{"ask", restatedPartedAsk, "followed_by", "session", restatedSession},
		// The class `store`.
		{"lens", restatedLens, "changed_by_run", "interaction", restatedRun},
		{"lens", restatedLens, "has_kind", "lens_kind", "pre-mortem"},
		{"lens", restatedTriggeredLens, "triggered_by", "lens", restatedLens},
		{"block", restatedRequirement, "constrained_by", "constraint", restatedConstraint},
		{"risk", restatedRisk, "prevented_by", "block", "CORE-1/spec#b1"},
		{"lens", restatedLens, "part_of", "issue", "CORE-1"},
		{"lens", restatedLens, "authored_by", "person", restatedAuthor},
		{"risk", restatedRisk, "part_of", "lens", restatedLens},
		{"risk", restatedRisk, "authored_by", "person", restatedAuthor},
		{"constraint", restatedConstraint, "part_of", "issue", "CORE-1"},
		{"constraint", restatedConstraint, "authored_by", "person", restatedAuthor},
		{"interaction", restatedRun, "part_of", "issue", "CORE-1"},
		{"interaction", restatedRun, "authored_by", "session", restatedSession},
		// A finding's relations.
		{"lens_block", restatedFinding, "part_of", "lens", restatedLens},
		{"lens_block", restatedFinding, "authored_by", "person", restatedAuthor},
		{"lens_block", restatedFinding, "changed_by_run", "interaction", restatedRun},
	} {
		if !answered[row] {
			t.Errorf("%s's graph_edges answers no %v: the fixture holds no row of that kind", storeClassRestatement, row)
		}
	}
	if !slices.Equal(migrated, restated) {
		t.Errorf("graph_edges as migrated answers %d rows and as %s states it %d\nonly as migrated: %q\nonly as %s states it: %q",
			len(migrated), storeClassRestatement, len(restated),
			rowsMissingFrom(migrated, restated), storeClassRestatement, rowsMissingFrom(restated, migrated))
	}
}

// graph_edges ends on one definition whatever order a database ran its restatements in. The
// first database runs the set without 20261117020856 and 20261121104002, as a database that ran
// 20261120122700 and 20261121034242 before 20261117020856 did, and then the whole set, so
// 20261117020856 runs after 20261121034242. The second runs the whole set from empty, so
// 20261121034242 runs after 20261117020856. Both views have one definition.
func TestGraphEdgesEndsOnOneDefinitionWhateverOrderItsRestatementsRan(t *testing.T) {
	ctx := context.Background()
	ranLaterRestatementsFirst := openEmptyTestStore(t)
	if err := ranLaterRestatementsFirst.migrate(ctx, migrationsWithout(t, storeClassRestatement, lastRestatement)); err != nil {
		t.Fatalf("migrate without %s and %s: %v", storeClassRestatement, lastRestatement, err)
	}
	if err := ranLaterRestatementsFirst.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest of the set: %v", err)
	}
	fromEmpty := openEmptyTestStore(t)
	if err := fromEmpty.Migrate(ctx); err != nil {
		t.Fatalf("migrate from empty: %v", err)
	}
	const laterFirst = "20261120122700 and 20261121034242 before 20261117020856"
	definitions := map[string]string{}
	for name, database := range map[string]*Store{laterFirst: ranLaterRestatementsFirst, "from empty": fromEmpty} {
		var definition string
		if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges', true)`).Scan(&definition); err != nil {
			t.Fatalf("read graph_edges' definition %s: %v", name, err)
		}
		definitions[name] = definition
	}
	if definitions[laterFirst] != definitions["from empty"] {
		t.Errorf("graph_edges' definition differs by the order its restatements ran in\n%s:\n%s\nfrom empty:\n%s",
			laterFirst, definitions[laterFirst], definitions["from empty"])
	}
}

// migrationsWithout is the embedded set without every file of the migrations it names, each
// named by its file's stem (<version>_<name>). It names at least one: the whole set is
// Store.Migrate's. A name that matches no file fails the test: such a set would be the whole set
// under another name.
func migrationsWithout(t *testing.T, names ...string) fstest.MapFS {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("migrationsWithout names no migration to leave out: migrate the whole set with Store.Migrate")
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, name := range names {
		left[name] = true
	}
	set := fstest.MapFS{}
	for _, entry := range entries {
		stem, _, _ := strings.Cut(entry.Name(), ".")
		if slices.Contains(names, stem) {
			delete(left, stem)
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		set["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	for name := range left {
		t.Fatalf("migrations/ holds no file of %s", name)
	}
	return set
}

// writeEveryGraphEdgesArm writes, in tx, at least one row each arm of graph_edges answers, and
// tuples of every kind the tuple arm and the arm over a finding read: the kinds whose class
// consults a projection row, each row of which names its node, the kinds of the class `store`,
// a finding's relations, newer tuples of a relation that retract it and assert it again, and
// tuples neither view answers - out of a node that is gone, retracted, of a kind no class holds,
// a finding's mention, a mention no block_refs row names, and a value.
func writeEveryGraphEdgesArm(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	made := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for _, node := range []struct{ kind, id, state string }{
		{"ask", restatedStoredAsk, "live"},
		{"comment", restatedStoredAnchored, "live"},
		{"comment", restatedStoredOwned, "live"},
		{"block", restatedStoredBlock, "live"},
		{"block", restatedRequirement, "live"},
		{"block", restatedFinding, "live"},
		{"lens", restatedLens, "live"},
		{"lens", restatedTriggeredLens, "live"},
		{"lens", restatedGoneLens, "gone"},
		{"risk", restatedRisk, "live"},
		{"constraint", restatedConstraint, "live"},
		{"interaction", restatedRun, "live"},
	} {
		if _, err := tx.Exec(ctx, `
			insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
			values ($1, $2, $1, 'project', $3, $4, '{"kind":"user","id":"alice"}', 0)
		`, node.kind, node.id, node.state, made); err != nil {
			t.Fatalf("write the %s node %s: %v", node.kind, node.id, err)
		}
	}

	// The rows the arms over Dispatch's own tables answer, and the rows that name a node of the
	// store, which the tuple arm's classes consult.
	rows := strings.NewReplacer(
		"@spec", restatedSpec,
		"@notes", restatedNotes,
		"@blockAsk", restatedBlockAsk,
		"@ownedAsk", restatedOwnedAsk,
		"@partedAsk", restatedPartedAsk,
		"@storedAsk", restatedStoredAsk,
		"@anchoredComment", "cccccccc-0000-4000-8000-000000000001",
		"@ownedComment", "cccccccc-0000-4000-8000-000000000002",
		"@replyComment", "cccccccc-0000-4000-8000-000000000003",
		"@askReply", "cccccccc-0000-4000-8000-000000000004",
		"@blockReply", "cccccccc-0000-4000-8000-000000000005",
		"@storedAnchored", restatedStoredAnchored,
		"@storedOwned", restatedStoredOwned,
		"@rootMessage", "dddddddd-0000-4000-8000-000000000001",
		"@messageReply", "dddddddd-0000-4000-8000-000000000002",
		"@messageBlockReply", "dddddddd-0000-4000-8000-000000000003",
	).Replace(`
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice@d.example');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'Parent', '{"kind":"user","id":"alice"}', 'A');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key)
		values ('CORE-2', 'CORE', 2, 'Child', '{"kind":"user","id":"alice"}', 'B', 'CORE-1');
		insert into issue_links (issue_key, kind, target_key) values ('CORE-2', 'blocked_by', 'CORE-1');
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('@spec', 'CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}');
		insert into artifacts (id, project_key, slug, name, kind, created_by)
		values ('@notes', 'CORE', 'notes', 'Notes', 'doc', '{"kind":"user","id":"alice"}');

		insert into asks (id, issue_key, author, question, block_id, block_artifact_id)
		values ('@blockAsk', 'CORE-1', '{"kind":"user","id":"alice"}', 'block?', 'b1', '@spec');
		insert into asks (id, artifact_id, author, question)
		values ('@ownedAsk', '@notes', '{"kind":"user","id":"alice"}', 'owned?');
		insert into asks (id, issue_key, author, question)
		values ('@partedAsk', 'CORE-1', '{"kind":"user","id":"alice"}', 'parted?');
		insert into ask_followers (ask_id, session_id) values ('@partedAsk', 's1');
		insert into asks (id, artifact_id, author, question, anchor, node_kind, node_address)
		values ('@storedAsk', '@notes', '{"kind":"user","id":"alice"}', 'held?',
		        '{"artifact_id": "@notes"}', 'ask', '@storedAsk');
		insert into ask_followers (ask_id, session_id, node_kind, node_address)
		values ('@storedAsk', 's-stored', 'ask', '@storedAsk');

		insert into comments (id, issue_key, author, body, anchor)
		values ('@anchoredComment', 'CORE-1', '{"kind":"user","id":"alice"}', 'anchored',
		        '{"artifact_id": "@spec", "mark_id": "m1"}');
		insert into comments (id, artifact_id, author, body)
		values ('@ownedComment', '@notes', '{"kind":"user","id":"alice"}', 'owned');
		insert into comments (id, issue_key, author, body, reply_to, answers)
		values ('@replyComment', 'CORE-1', '{"kind":"user","id":"alice"}', 'reply', '@anchoredComment', '@ownedComment');
		insert into comments (id, issue_key, author, body, ask_id, replies_to_block)
		values ('@askReply', 'CORE-1', '{"kind":"user","id":"alice"}', 'about that part', '@partedAsk', 'ask/@partedAsk#b1');
		insert into comments (id, issue_key, author, body, replies_to_block)
		values ('@blockReply', 'CORE-1', '{"kind":"user","id":"alice"}', 'about that paragraph', 'comment/@ownedComment#b1');
		insert into comments (id, issue_key, author, body, anchor, node_kind, node_address)
		values ('@storedAnchored', 'CORE-1', '{"kind":"user","id":"alice"}', 'held anchored',
		        '{"artifact_id": "@spec"}', 'comment', '@storedAnchored');
		insert into comments (id, artifact_id, author, body, node_kind, node_address)
		values ('@storedOwned', '@notes', '{"kind":"user","id":"alice"}', 'held owned', 'comment', '@storedOwned');

		insert into messages (id, issue_key, author, body)
		values ('@rootMessage', 'CORE-1', '{"kind":"session","id":"s1"}', 'hello');
		insert into messages (id, issue_key, author, body, in_reply_to)
		values ('@messageReply', 'CORE-1', '{"kind":"user","id":"alice"}', 'hi', '@rootMessage');
		insert into messages (id, issue_key, author, body, replies_to_block)
		values ('@messageBlockReply', 'CORE-1', '{"kind":"user","id":"alice"}', 'about that paragraph', 'message/@rootMessage#b1');
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq)
		values ('message', '@rootMessage', 'issue', 'CORE-2', 42);

		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'acme/core', 'main', 1, '{"kind":"user","id":"alice"}');
		insert into architecture_snapshots (id, project_key, commit, files) values (900, 'CORE', 'c1', '{}');
		insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
		values ('CORE', 'core', 'Core', '', null, 900, (select id from architecture_sources where project_key = 'CORE'));
		insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
		values ('CORE', 'api', 'API', '', 'core', 900, (select id from architecture_sources where project_key = 'CORE'));
		insert into component_depends (project_key, from_id, to_id) values ('CORE', 'api', 'core');
		insert into issue_components (issue_key, mode) values ('CORE-2', 'explicit');
		insert into issue_component_members (issue_key, project_key, component_id) values ('CORE-2', 'CORE', 'api');

		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ('@spec', 'b1', 'CORE-1/spec#b1', 'paragraph', 'x', 'x', 1, now()),
		       ('@notes', 'b1', 'CORE/notes#b1', 'paragraph', 'y', 'y', 1, now());
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at,
		                             node_kind, node_address)
		values ('@spec', 'b2', 'CORE-1/spec#b2', 'paragraph', 'z', 'z', 1, now(), 'block', 'CORE-1/spec#b2');
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, source_seq)
		values ('@spec', 'b1', 'CORE-1/spec#b1', 'issue', 'CORE-2', 'mentions', 1, 7);
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, source_seq,
		                        node_kind, node_address)
		values ('@spec', 'b2', 'CORE-1/spec#b2', 'issue', 'CORE-2', 'mentions', 1, 8, 'block', 'CORE-1/spec#b2'),
		       ('@spec', 'b2', 'CORE-1/spec#b2', 'issue', 'CORE-1', 'depends_on', 1, null, 'block', 'CORE-1/spec#b2'),
		       ('@spec', 'b1', 'CORE-1/spec#b1', 'block', 'CORE/notes#b1', 'shaped', 1, null, 'ask', '@storedAsk');

		insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, made_at,
		                            changed_revision, changed_at, removed_at)
		values ('@ownedComment', 'b1', 'comment/@ownedComment#b1', 0, 'paragraph', 'x', 'x', now(), 1, now(), null),
		       ('@ownedComment', 'b2', 'comment/@ownedComment#b2', null, 'paragraph', 'y', 'y', now(), 1, now(), now());
		insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id, source_seq)
		values ('@ownedComment', 'b1', 'comment/@ownedComment#b1', 'issue', 'CORE-2', 9),
		       ('@ownedComment', 'b2', 'comment/@ownedComment#b2', 'issue', 'CORE-1', 10);
		insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at)
		values ('@rootMessage', 'b1', 'message/@rootMessage#b1', 0, 'paragraph', 'z', 'z', now(), now());
		insert into message_block_refs (message_id, block_id, from_node, to_kind, to_id, source_seq)
		values ('@rootMessage', 'b1', 'message/@rootMessage#b1', 'issue', 'CORE-2', 11);
		insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, made_at,
		                        changed_revision, changed_at)
		values ('@partedAsk', 'b1', 'ask/@partedAsk#b1', 0, 'question', 'q', 'q', now(), 1, now());
		insert into ask_block_refs (ask_id, block_id, from_node, to_kind, to_id, source_seq)
		values ('@partedAsk', 'b1', 'ask/@partedAsk#b1', 'issue', 'CORE-2', 12);
	`)
	if _, err := tx.Exec(ctx, rows); err != nil {
		t.Fatalf("write the rows of the view's arms: %v", err)
	}

	// The tuples, in the order the store appends them: a later tuple of one relation is the newer.
	for index, tuple := range []struct {
		fromKind, fromID, kind, toKind, toID string
		asserted                             bool
		sourceSeq                            int64
	}{
		{"block", restatedStoredBlock, "in_document", "artifact", "CORE-1/spec", true, 0},
		{"block", restatedStoredBlock, "mentions", "issue", "CORE-2", true, 8},
		{"block", restatedStoredBlock, "depends_on", "issue", "CORE-1", true, 0},
		{"comment", restatedStoredAnchored, "anchored_to", "artifact", "CORE-1/spec", true, 0},
		{"comment", restatedStoredOwned, "owned_by", "artifact", "CORE/notes", true, 0},
		{"ask", restatedStoredAsk, "anchored_to", "artifact", "CORE/notes", true, 0},
		{"ask", restatedStoredAsk, "owned_by", "artifact", "CORE/notes", true, 0},
		{"ask", restatedStoredAsk, "followed_by", "session", restatedStoredFollower, true, 0},
		{"ask", restatedStoredAsk, "shaped", "block", restatedNotesFirstBlock, true, 0},

		{"lens", restatedLens, "part_of", "issue", "CORE-1", true, 0},
		{"lens", restatedLens, "authored_by", "person", restatedAuthor, true, 0},
		{"lens", restatedLens, "has_kind", "lens_kind", "pre-mortem", true, 0},
		{"lens", restatedLens, "changed_by_run", "interaction", restatedRun, true, 0},
		{"lens", restatedTriggeredLens, "part_of", "artifact", "CORE-1/spec", true, 0},
		{"lens", restatedTriggeredLens, "triggered_by", "lens", restatedLens, true, 0},
		{"risk", restatedRisk, "part_of", "lens", restatedLens, true, 0},
		{"risk", restatedRisk, "authored_by", "person", restatedAuthor, true, 0},
		{"risk", restatedRisk, "prevented_by", "block", "CORE-1/spec#b1", true, 0},
		{"constraint", restatedConstraint, "part_of", "issue", "CORE-1", true, 0},
		{"constraint", restatedConstraint, "authored_by", "person", restatedAuthor, true, 0},
		{"block", restatedRequirement, "constrained_by", "constraint", restatedConstraint, true, 0},
		{"interaction", restatedRun, "part_of", "issue", "CORE-1", true, 0},
		{"interaction", restatedRun, "authored_by", "session", restatedSession, true, 0},

		{"block", restatedFinding, "part_of", "lens", restatedLens, true, 0},
		{"block", restatedFinding, "authored_by", "person", restatedAuthor, true, 0},
		{"block", restatedFinding, "changed_by_run", "interaction", restatedRun, true, 0},

		// A relation retracted and asserted again, answered at its newest tuple.
		{"lens", restatedTriggeredLens, "triggered_by", "lens", restatedLens, false, 0},
		{"lens", restatedTriggeredLens, "triggered_by", "lens", restatedLens, true, 0},
		// Tuples neither view answers.
		{"lens", restatedGoneLens, "part_of", "issue", "CORE-1", true, 0},
		{"constraint", restatedConstraint, "part_of", "issue", "CORE-2", true, 0},
		{"constraint", restatedConstraint, "part_of", "issue", "CORE-2", false, 0},
		{"comment", restatedStoredAnchored, "authored_by", "person", restatedAuthor, true, 0},
		{"block", restatedStoredBlock, "part_of", "block", "CORE-1/spec#b1", true, 0},
		{"block", restatedStoredBlock, "mentions", "issue", "CORE-1", true, 0},
		{"block", restatedFinding, "mentions", "issue", "CORE-2", true, 0},
		{"lens", restatedLens, "state", "value", "live", true, 0},
	} {
		if _, err := tx.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
			values ($1, $2, $3, $4, $5, $6, '{"kind":"user","id":"alice"}', 0, nullif($7::bigint, 0), $8)
		`, tuple.fromKind, tuple.fromID, tuple.kind, tuple.toKind, tuple.toID,
			made.Add(time.Duration(index+1)*time.Minute), tuple.sourceSeq, tuple.asserted); err != nil {
			t.Fatalf("write the tuple %s %s %s %s %s: %v", tuple.fromKind, tuple.fromID, tuple.kind, tuple.toKind, tuple.toID, err)
		}
	}
}

// requireEveryGraphEdgesArmAnswers runs each arm of graph_edges as the database states it
// (pg_get_viewdef) on its own and fails the test for each that answers no row.
func requireEveryGraphEdgesArmAnswers(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	var definition string
	if err := tx.QueryRow(ctx, `select pg_get_viewdef('graph_edges', true)`).Scan(&definition); err != nil {
		t.Fatalf("read graph_edges' definition: %v", err)
	}
	for index, arm := range strings.Split(definition, "UNION ALL") {
		arm = strings.TrimSuffix(strings.TrimSpace(arm), ";")
		var rows int
		if err := tx.QueryRow(ctx, `select count(*) from (`+arm+`) arm`).Scan(&rows); err != nil {
			t.Fatalf("count arm %d of graph_edges: %v\n%s", index+1, err, arm)
		}
		if rows == 0 {
			t.Errorf("arm %d of graph_edges answers no row of the fixture:\n%s", index+1, arm)
		}
	}
}

// graphEdgesInOrder is every row of graph_edges as text, ordered by every column.
func graphEdgesInOrder(t *testing.T, tx pgx.Tx) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		select edge::text from graph_edges edge
		order by edge.from_kind, edge.from_id, edge.kind, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq`)
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	edges, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	return edges
}

// graphEdgesAnswered is each (from_kind, from_id, kind, to_kind, to_id) graph_edges answers.
func graphEdgesAnswered(t *testing.T, tx pgx.Tx) map[[5]string]bool {
	t.Helper()
	rows, err := tx.Query(context.Background(), `select from_kind, from_id, kind, to_kind, to_id from graph_edges`)
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	defer rows.Close()
	answered := map[[5]string]bool{}
	for rows.Next() {
		var row [5]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3], &row[4]); err != nil {
			t.Fatalf("read graph_edges: %v", err)
		}
		answered[row] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	return answered
}

// rowsMissingFrom is each row of rows that other does not hold.
func rowsMissingFrom(rows, other []string) []string {
	var missing []string
	for _, row := range rows {
		if !slices.Contains(other, row) {
			missing = append(missing, row)
		}
	}
	return missing
}
