package graphstore

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAnUncausedWriteStatesEachAttributesOwnValue holds what a writer with no cause yet has
// appended once its cause comes (Caused) to what the store states for a node it makes: its
// visibility and state, its project or its parties where it carries one, each under its own value
// and the cause, and nothing of an attribute it does not carry. Both uncaused writers leave the
// mark that names those attributes (nodeMarks): the node write PlanUncaused plans and QueueNodes
// queues, and the arms of WriteArmsUncaused.
//
// A node the write makes whose state its caller states itself (PlanUncaused's stated tuples) is
// one an upload makes where it read an orphaned node standing and a delete outside the builds took
// that node before the upload's write: the caller's gone and live come first, and the node write
// states every other base attribute of the node it made, as it states them for any node it makes.
// Only the node write takes such tuples; the arms state no caller's base attribute.
func TestAnUncausedWriteStatesEachAttributesOwnValue(t *testing.T) {
	database := storetest.Open(t)
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const cause = int64(9500002)
	writers := map[string]func(context.Context, pgx.Tx, Node, []Tuple) (Uncaused, error){
		"QueueNodes": func(ctx context.Context, tx pgx.Tx, node Node, stated []Tuple) (Uncaused, error) {
			write := PlanUncaused([]Node{node}, stated)
			return write, store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				QueueNodes(b, write)
				return nil
			})
		},
		"WriteArmsUncaused": func(ctx context.Context, tx pgx.Tx, node Node, _ []Tuple) (Uncaused, error) {
			arms, params, held, err := WriteArmsUncaused(1, []Node{node}, nil, nil)
			if err != nil {
				return Uncaused{}, err
			}
			_, err = tx.Exec(ctx, "with "+arms+"\nselect count(*) from graph_nodes_written", params...)
			return held, err
		},
	}
	// stateOf is a state of node as its caller states it, in a tuple of its own.
	stateOf := func(node Node, state string) Tuple {
		return Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: AttributeState, ToKind: KindValue, ToID: state,
			At: node.MadeAt, Who: node.MadeBy, Asserted: true,
		}
	}
	for name, test := range map[string]struct {
		node func(suffix string) Node
		// stated is what the caller states of the node itself, nil for none; a row that states
		// something runs through QueueNodes alone.
		stated func(Node) []Tuple
		// want is every value tuple of the node, "kind=value", in the order the log holds them.
		want []string
	}{
		"a node in a project": {node: func(suffix string) Node { return IssueNode("CORE-9"+suffix, "CORE", who, at) }, want: []string{
			AttributeVisibility + "=" + VisibilityProject, AttributeState + "=" + StateLive, AttributeProject + "=CORE",
		}},
		"a node between parties": {node: func(suffix string) Node {
			return MessageNode("m-9"+suffix, "", []string{PartySession("s-9"), PartyPerson("Bob")}, who, at)
		}, want: []string{
			AttributeVisibility + "=" + VisibilityProject, AttributeState + "=" + StateLive, AttributeParties + "=person:bob session:s-9",
		}},
		"a node of neither": {node: func(suffix string) Node { return PersonNode("carol"+suffix, who, at, NoEvent) }, want: []string{
			AttributeVisibility + "=" + VisibilityProject, AttributeState + "=" + StateLive,
		}},
		"a node the write makes whose state its caller states": {
			node: func(suffix string) Node { return ArtifactNode("agent/s-9/orphan"+suffix+"-md", "", "s-9", who, at) },
			stated: func(node Node) []Tuple {
				return []Tuple{stateOf(node, StateGone), stateOf(node, StateLive)}
			},
			want: []string{
				AttributeState + "=" + StateGone, AttributeState + "=" + StateLive,
				AttributeVisibility + "=" + VisibilityProject, AttributeParties + "=session:s-9",
			},
		},
	} {
		for writer, write := range writers {
			if test.stated != nil && writer != "QueueNodes" {
				continue
			}
			t.Run(name+" through "+writer, func(t *testing.T) {
				node := test.node("-" + writer)
				var stated []Tuple
				if test.stated != nil {
					stated = test.stated(node)
				}
				runInOneTransaction(t, database, writer, func(ctx context.Context, tx pgx.Tx) error {
					held, err := write(ctx, tx, node, stated)
					if err != nil {
						return err
					}
					return Caused(ctx, tx, held, cause)
				})
				var held []string
				rows, err := database.Pool.Query(context.Background(), `
					select kind, to_id, source_event from graph_tuples
					where from_kind = $1 and from_id = $2 and to_kind = $3
					order by seq
				`, node.Kind, node.ID, KindValue)
				if err != nil {
					t.Fatalf("read the base attributes of %s/%s: %v", node.Kind, node.ID, err)
				}
				var kind, value string
				var event int64
				if _, err := pgx.ForEachRow(rows, []any{&kind, &value, &event}, func() error {
					if event != cause {
						t.Errorf("%s/%s states %s under event %d, want the cause %d", node.Kind, node.ID, kind, event, cause)
					}
					held = append(held, kind+"="+value)
					return nil
				}); err != nil {
					t.Fatalf("read the base attributes of %s/%s: %v", node.Kind, node.ID, err)
				}
				if !slices.Equal(held, test.want) {
					t.Errorf("%s states %v of %s/%s, want %v", writer, held, node.Kind, node.ID, test.want)
				}
				var source int64
				if err := database.Pool.QueryRow(context.Background(), `
					select source_event from graph_nodes where kind = $1 and id = $2
				`, node.Kind, node.ID).Scan(&source); err != nil || source != cause {
					t.Errorf("the row of %s/%s is at event %d (%v), want the cause %d", node.Kind, node.ID, source, err, cause)
				}
			})
		}
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
// neither, through Write, QueueNodes and WriteArms alike.
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

	t.Run("an uncaused node write and Caused state the parties with the other base attributes", func(t *testing.T) {
		node := ArtifactNode("agent/s-2/photo-png", "", "s-2", who, at)
		runInOneTransaction(t, database, "the uncaused write", func(ctx context.Context, tx pgx.Tx) error {
			write := PlanUncaused([]Node{node}, nil)
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				QueueNodes(b, write)
				return nil
			}); err != nil {
				return err
			}
			return Caused(ctx, tx, write, 9500003)
		})
		requireProjectRow(t, ctx, database, node.Kind, node.ID, "", []string{"session:s-2"})
		requireStatedOnce(t, ctx, database, node.Kind, node.ID, AttributeParties, "session:s-2")
		requireNeverStated(t, ctx, database, node.Kind, node.ID, AttributeProject)
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

// The arm over refs answers a document's mention whose far end is past the store's bound on an
// address, of which no tuple is stated (store.ReferenceNotStated), and that bound is
// MaxAddressBytes, which package store writes out as text because this package imports it.
func TestTheArmOverRefsTakesTheStoresAddressBound(t *testing.T) {
	if bound := "octet_length(r.to_id) > " + strconv.Itoa(MaxAddressBytes) + "\n"; !strings.Contains(store.ReferenceNotStated, bound) {
		t.Fatalf("the arm over refs bounds a far end otherwise than the store's %d bytes:\n%s", MaxAddressBytes, store.ReferenceNotStated)
	}
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
