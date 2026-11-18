package graphstore

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestBaseAttributeFactsHoldEachAttributesOwnValue holds the facts a writer with no cause yet
// hands to Caused (WriteArmsUncaused) to what the node arm states for a node it makes: its
// visibility and state, its project or its parties where it carries one, each under its own
// value, and nothing of an attribute it does not carry.
func TestBaseAttributeFactsHoldEachAttributesOwnValue(t *testing.T) {
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	for name, test := range map[string]struct {
		node Node
		want map[string]string
	}{
		"a node in a project": {IssueNode("CORE-9", "CORE", who, at), map[string]string{
			AttributeVisibility: VisibilityProject, AttributeState: StateLive, AttributeProject: "CORE",
		}},
		"a node between parties": {MessageNode("m-9", "", []string{PartySession("s-9"), PartyPerson("Bob")}, who, at), map[string]string{
			AttributeVisibility: VisibilityProject, AttributeState: StateLive, AttributeParties: "person:bob session:s-9",
		}},
		"a node of neither": {PersonNode("carol", who, at, NoEvent), map[string]string{
			AttributeVisibility: VisibilityProject, AttributeState: StateLive,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			held := map[string]string{}
			for _, fact := range BaseAttributeFacts([]Node{test.node}) {
				held[fact.Kind] = fact.ToID
			}
			if !maps.Equal(held, test.want) {
				t.Errorf("BaseAttributeFacts holds %v, want %v", held, test.want)
			}
		})
	}
}

// A node's project and the parties it is between are base attributes of the store, as its
// visibility and state are, so a read can be limited to a project: each is a value tuple the
// write that stores the node appends, and graph_nodes.project_key and graph_nodes.parties are
// their projection. The tests below read the store as a reader does - CurrentValue and History,
// which answer the tuples alone - beside the row, which is what the census and the read rule
// bind.

// TestANodeStatesItsProjectOnce holds the write to stating a node's project and parties once: the
// write that makes the node states both, a node an earlier build stored without a project takes
// it from the first write that names one, and a node written again with nothing moved states
// neither, through Write, WriteUncaused and WriteArms alike.
func TestANodeStatesItsProjectOnce(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}

	t.Run("a node made with its project", func(t *testing.T) {
		node := IssueNode("CORE-1", "CORE", who, at)
		writeNode(t, ctx, database, node)
		writeNode(t, ctx, database, node)
		requireProjectRow(t, ctx, database, node.Kind, node.ID, "CORE", nil)
		requireStatedOnce(t, ctx, database, node.Kind, node.ID, AttributeProject, "CORE")
		requireNeverStated(t, ctx, database, node.Kind, node.ID, AttributeParties)
	})

	t.Run("a node made with no project takes it from the first write that names one", func(t *testing.T) {
		// What the comment writer stored before nodes named their project: a comment node with
		// no project.
		bare, _ := CommentFacts(nil, "11111111-1111-4111-8111-111111111111", who, at, NoEvent, "", "", "", nil)
		writeNode(t, ctx, database, bare)
		requireProjectRow(t, ctx, database, bare.Kind, bare.ID, "", nil)
		requireNeverStated(t, ctx, database, bare.Kind, bare.ID, AttributeProject)

		filled := bare
		filled.Project = "CORE"
		filled.MadeAt = at.Add(time.Minute)
		writeNode(t, ctx, database, filled)
		writeNode(t, ctx, database, filled)
		requireProjectRow(t, ctx, database, bare.Kind, bare.ID, "CORE", nil)
		requireStatedOnce(t, ctx, database, bare.Kind, bare.ID, AttributeProject, "CORE")
		// The fill moved neither visibility nor state, so it stated neither again.
		for _, attribute := range []string{AttributeVisibility, AttributeState} {
			if history := historyOf(t, ctx, database, bare.Kind, bare.ID, attribute); len(history) != 1 {
				t.Errorf("the fill of the project stated %s again: %d tuples, want the first write's 1", attribute, len(history))
			}
		}
	})

	t.Run("a node no project owns states its parties, sorted", func(t *testing.T) {
		node := MessageNode("22222222-2222-4222-8222-222222222222", "",
			[]string{PartySession("s-1"), PartyPerson("Alice"), PartySession("s-1")}, who, at)
		writeNode(t, ctx, database, node)
		writeNode(t, ctx, database, node)
		want := []string{"person:alice", "session:s-1"}
		requireProjectRow(t, ctx, database, node.Kind, node.ID, "", want)
		requireStatedOnce(t, ctx, database, node.Kind, node.ID, AttributeParties, "person:alice session:s-1")
		requireNeverStated(t, ctx, database, node.Kind, node.ID, AttributeProject)
	})

	t.Run("WriteUncaused holds the project and parties with the other base attributes", func(t *testing.T) {
		node := ArtifactNode("agent/s-2/photo-png", "", "s-2", who, at)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		write, err := WriteUncaused(ctx, tx, []Node{node}, nil)
		if err != nil {
			t.Fatalf("write the node: %v", err)
		}
		stated := map[string]string{}
		for _, tuple := range write.Tuples {
			stated[tuple.Kind] = tuple.ToID
		}
		wantStated := map[string]string{
			AttributeVisibility: VisibilityProject, AttributeState: StateLive, AttributeParties: "session:s-2",
		}
		if !maps.Equal(stated, wantStated) {
			t.Errorf("WriteUncaused holds %v, want %v", stated, wantStated)
		}
	})

	t.Run("the publisher's arms state the project of a node they make", func(t *testing.T) {
		node := AskNode("33333333-3333-4333-8333-333333333333", "CORE", nil, who, at)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		arms, params, err := WriteArms(1, []Node{node}, nil, nil)
		if err != nil {
			t.Fatalf("build the arms: %v", err)
		}
		if _, err := tx.Exec(ctx, `with `+arms+` select 1`, params...); err != nil {
			t.Fatalf("run the arms: %v", err)
		}
		if _, err := tx.Exec(ctx, `with `+arms+` select 1`, params...); err != nil {
			t.Fatalf("run the arms again: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		requireProjectRow(t, ctx, database, node.Kind, node.ID, "CORE", nil)
		requireStatedOnce(t, ctx, database, node.Kind, node.ID, AttributeProject, "CORE")
	})
}

// TestANodeCannotMoveProject holds a stored node to the project its first naming write gave it: a
// write naming another fails with ErrProjectMoved and the row keeps its project, while a write
// that names no project - a writer that knows none, such as a person node's - leaves it as it is.
func TestANodeCannotMoveProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	node := IssueNode("CORE-7", "CORE", who, at)
	writeNode(t, ctx, database, node)

	moved := node
	moved.Project = "OTHER"
	// A move that also moves another column, and one that moves the project alone, are refused
	// alike: the refusal reads the stored row, not whether the upsert fired.
	stateMoved := moved
	stateMoved.State = StateGone
	for name, write := range map[string]Node{"the project alone": moved, "the project and the state": stateMoved} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		err = Write(ctx, tx, []Node{write}, nil)
		_ = tx.Rollback(ctx)
		if !errors.Is(err, ErrProjectMoved) {
			t.Errorf("a write moving %s answered %v, want ErrProjectMoved", name, err)
		}
	}
	requireProjectRow(t, ctx, database, node.Kind, node.ID, "CORE", nil)

	unnamed := node
	unnamed.Project = ""
	unnamed.State = StateGone
	writeNode(t, ctx, database, unnamed)
	requireProjectRow(t, ctx, database, node.Kind, node.ID, "CORE", nil)
	requireStatedOnce(t, ctx, database, node.Kind, node.ID, AttributeProject, "CORE")
}

// TestArtifactRefKeyIsTheTriggers holds ArtifactRefKey to the address the artifacts_ref_key trigger
// (0071_agent_artifacts.up.sql) gives a row, for each of the three owners an artifact has: a
// document's node is written before its row, so the writer computes the address the row will
// take, and an address the trigger then wrote otherwise would be a node no row projects.
func TestArtifactRefKeyIsTheTriggers(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('REFK', 'Ref keys', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('REFK-1', 'REFK', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed project REFK and issue REFK-1: %v", err)
	}
	for _, owner := range []struct{ name, issue, project, session, slug string }{
		{"an issue's document", "REFK-1", "REFK", "", "notes-md"},
		{"a project's document", "", "REFK", "", "notes-md"},
		{"a conversation's file", "", "", "01a1e0fe-90a3-76fa-b47a-7d62f39b4520", "photo-png"},
	} {
		var stored string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, session_id, slug, name, kind, created_by)
			values (nullif($1, ''), nullif($2, ''), nullif($3, ''), $4, $4, 'doc', '{"kind":"user","id":"alice"}')
			returning ref_key
		`, owner.issue, owner.project, owner.session, owner.slug).Scan(&stored); err != nil {
			t.Fatalf("insert %s: %v", owner.name, err)
		}
		if computed := ArtifactRefKey(owner.issue, owner.project, owner.session, owner.slug); computed != stored {
			t.Errorf("ArtifactRefKey of %s is %q, but the trigger stored %q", owner.name, computed, stored)
		}
	}
}

// requireProjectRow fails unless the node row's project_key and parties are project (null where
// empty) and parties (null where nil).
func requireProjectRow(t *testing.T, ctx context.Context, database *store.Store, kind, id, project string, parties []string) {
	t.Helper()
	node, held, err := ReadNode(ctx, database.Pool, kind, id)
	if err != nil || !held {
		t.Fatalf("read node %s/%s: held %t, %v", kind, id, held, err)
	}
	if node.Project != project || !slices.Equal(node.Parties, parties) {
		t.Errorf("node %s/%s holds project %q and parties %v, want %q and %v", kind, id, node.Project, node.Parties, project, parties)
	}
	var nullProject, nullParties bool
	if err := database.Pool.QueryRow(ctx,
		`select project_key is null, parties is null from graph_nodes where kind = $1 and id = $2`, kind, id,
	).Scan(&nullProject, &nullParties); err != nil {
		t.Fatalf("read the columns of %s/%s: %v", kind, id, err)
	}
	if nullProject != (project == "") || nullParties != (parties == nil) {
		t.Errorf("node %s/%s: project_key null %t and parties null %t, want %t and %t",
			kind, id, nullProject, nullParties, project == "", parties == nil)
	}
}

// requireStatedOnce fails unless the log holds exactly one tuple of the attribute out of the node,
// carrying value, and the row is what that tuple says.
func requireStatedOnce(t *testing.T, ctx context.Context, database *store.Store, kind, id, attribute, value string) {
	t.Helper()
	history := historyOf(t, ctx, database, kind, id, attribute)
	if len(history) != 1 || history[0].ToID != value || history[0].ToKind != KindValue {
		t.Errorf("node %s/%s states %s %d time(s) (%+v), want once, as %q", kind, id, attribute, len(history), history, value)
	}
	current, held, err := CurrentValue(ctx, database.Pool, kind, id, attribute)
	if err != nil || !held || current != value {
		t.Errorf("the log's %s of %s/%s is %q (held %t, %v), want %q", attribute, kind, id, current, held, err, value)
	}
}

// requireNeverStated fails unless the log holds no tuple of the attribute out of the node.
func requireNeverStated(t *testing.T, ctx context.Context, database *store.Store, kind, id, attribute string) {
	t.Helper()
	if history := historyOf(t, ctx, database, kind, id, attribute); len(history) != 0 {
		t.Errorf("node %s/%s states %s %d time(s) (%+v), want never", kind, id, attribute, len(history), history)
	}
}

func historyOf(t *testing.T, ctx context.Context, database *store.Store, kind, id, attribute string) []Tuple {
	t.Helper()
	history, err := History(ctx, database.Pool, kind, id, attribute)
	if err != nil {
		t.Fatalf("read the %s history of %s/%s: %v", attribute, kind, id, err)
	}
	return history
}
