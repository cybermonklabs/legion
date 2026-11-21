package graphstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheStoreRefusesAuthorOnAKindItDoesNotAdmit holds every node write of the store to the kinds
// the read registry admits as ones that may hold a node reserved for its author
// (spec requirement r-node-visibility): a node at visibility author of any other kind
// is refused with ErrVisibilityNotAdmitted, as a VisibilityNotAdmittedError whose words name the
// kind, before the write's first statement - the transaction of the refused write still commits
// and holds no row and no tuple of the node. A block's kind is read off its id, so a finding of a
// lens is refused as a lens_block and a block of a document as a block. This package loads no
// registry, so the store here admits the one test kind its tests hand it (authorOnlyTestKind) and
// no other: a program that never loads the registry takes no author-only node at all. A node of
// an admitted kind at author, and a node of any kind at project, are written.
func TestTheStoreRefusesAuthorOnAKindItDoesNotAdmit(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedSupertags(t, ctx, database, authorOnlyTestKind)
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	reserved := func(kind, id string) Node {
		return Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: VisibilityAuthor, State: StateLive,
			MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
		}
	}
	if got := AuthorOnlyKinds(); len(got) != 1 || got[0] != authorOnlyTestKind {
		t.Fatalf("the store admits %v, want the test kind this package hands it and no other", got)
	}

	writers := []struct {
		name  string
		write func(context.Context, pgx.Tx, Node) error
	}{
		{"Write", func(ctx context.Context, tx pgx.Tx, node Node) error {
			return Write(ctx, tx, []Node{node}, nil)
		}},
		{"PlanUncaused and QueueNodes", func(ctx context.Context, tx pgx.Tx, node Node) error {
			write := PlanUncaused([]Node{node}, nil)
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				QueueNodes(b, write)
				return nil
			}); err != nil {
				return err
			}
			return Caused(ctx, tx, write, NoEvent)
		}},
		{"WriteArms", func(ctx context.Context, tx pgx.Tx, node Node) error {
			arms, params, err := WriteArms(1, []Node{node}, nil, nil)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, "with "+arms+" select 1", params...)
			return err
		}},
	}
	restating := func(node Node) Node {
		node.Restates = true
		return node
	}
	for _, refused := range []struct {
		name string
		node Node
		kind string
	}{
		{"a comment", reserved(KindComment, "11111111-1111-4111-8111-111111111111"), KindComment},
		{"a comment its writer restates", restating(reserved(KindComment, "22222222-2222-4111-8111-222222222222")), KindComment},
		{"an issue", reserved(KindIssue, "CORE-1"), KindIssue},
		{"a person", reserved(KindPerson, "alice"), KindPerson},
		{"a block of a document", reserved(blockKind, "CORE-1/spec#first"), blockKind},
		{"a finding of a lens", reserved(blockKind, KindLens+"/33333333-3333-4111-8111-333333333333#first"), KindLensBlock},
		{"a lens, which no registry here flags", reserved(KindLens, "44444444-4444-4111-8111-444444444444"), KindLens},
		// A kind the catalogue's seed does not hold, since the store holds the seeded ones.
		{"a lens kind", reserved(KindLensKind, "graphstore-test-kind"), KindLensKind},
	} {
		for _, writer := range writers {
			t.Run(writer.name+", "+refused.name, func(t *testing.T) {
				tx, err := database.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer tx.Rollback(ctx)
				err = writer.write(ctx, tx, refused.node)
				requireVisibilityNotAdmitted(t, err, refused.kind)
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("the transaction of the refused write does not commit, so the write sent a statement: %v", err)
				}
				requireNoNode(t, ctx, database, refused.node.Kind, refused.node.ID)
			})
		}
	}

	t.Run("a node of the admitted kind, reserved for its author", func(t *testing.T) {
		node := reserved(authorOnlyTestKind, "admitted")
		writeNode(t, ctx, database, node)
		held, found, err := ReadNode(ctx, database.Pool, node.Kind, node.ID)
		if err != nil || !found || held.Visibility != VisibilityAuthor {
			t.Fatalf("the admitted kind's node reads %+v (found %t, %v), want it reserved for its author", held, found, err)
		}
	})
	t.Run("a node of a kind not admitted, project-visible", func(t *testing.T) {
		node := reserved(KindComment, "55555555-5555-4111-8111-555555555555")
		node.Visibility = VisibilityProject
		writeNode(t, ctx, database, node)
		held, found, err := ReadNode(ctx, database.Pool, node.Kind, node.ID)
		if err != nil || !found || held.Visibility != VisibilityProject {
			t.Fatalf("the project comment reads %+v (found %t, %v), want it written", held, found, err)
		}
	})
	t.Run("CheckVisibilityAdmitted, the rule a writer that keeps a thing out of the store asks", func(t *testing.T) {
		requireVisibilityNotAdmitted(t, CheckVisibilityAdmitted(KindComment, "c1", VisibilityAuthor), KindComment)
		for _, admitted := range []struct{ kind, id, visibility string }{
			{KindComment, "c1", VisibilityProject},
			{KindComment, "c1", ""},
			{authorOnlyTestKind, "t1", VisibilityAuthor},
		} {
			if err := CheckVisibilityAdmitted(admitted.kind, admitted.id, admitted.visibility); err != nil {
				t.Errorf("CheckVisibilityAdmitted(%q, %q, %q) = %v, want none", admitted.kind, admitted.id, admitted.visibility, err)
			}
		}
	})
}

// TestABlocksAddressKindIsReadOffItsID: the kind the graph addresses a node by is its own, but a
// block whose id names a lens, which is a finding of that lens and lens_block, in Go and in SQL.
// The id FindingID builds is the one ParseFindingID reads back and FindingIDSQL tells, and no
// other block's id is one to either.
func TestABlocksAddressKindIsReadOffItsID(t *testing.T) {
	database := storetest.Open(t)
	const lens = "33333333-3333-4111-8111-333333333333"
	finding := FindingID(lens, "first")
	if finding != KindLens+"/"+lens+"#first" {
		t.Fatalf("FindingID(%q, %q) = %q, want lens/<uuid>#<block id>", lens, "first", finding)
	}
	if gotLens, gotBlock, ok := ParseFindingID(finding); !ok || gotLens != lens || gotBlock != "first" {
		t.Errorf("ParseFindingID(%q) = %q, %q, %t; want %q, %q, true", finding, gotLens, gotBlock, ok, lens, "first")
	}
	for _, node := range []struct{ kind, id, want string }{
		{blockKind, finding, KindLensBlock},
		{blockKind, "CORE-1/spec#first", blockKind},
		{blockKind, "comment/33333333-3333-4111-8111-333333333333#b1", blockKind},
		{KindLens, "33333333-3333-4111-8111-333333333333", KindLens},
		{KindComment, "lens/33333333", KindComment},
	} {
		if got := AddressKind(node.kind, node.id); got != node.want {
			t.Errorf("AddressKind(%q, %q) = %q, want %q", node.kind, node.id, got, node.want)
		}
		var got string
		var findingSQL bool
		if err := database.Pool.QueryRow(context.Background(), `select `+AddressKindSQL("$1::text", "$2::text")+`, `+FindingIDSQL("$2::text"),
			node.kind, node.id).Scan(&got, &findingSQL); err != nil {
			t.Fatalf("read the address kind of %s %s in SQL: %v", node.kind, node.id, err)
		}
		if got != node.want {
			t.Errorf("AddressKindSQL of %q, %q = %q, want %q", node.kind, node.id, got, node.want)
		}
		if node.kind == blockKind {
			_, _, parsed := ParseFindingID(node.id)
			if want := node.want == KindLensBlock; parsed != want || findingSQL != want {
				t.Errorf("the block %q is a finding's to ParseFindingID (%t) and to FindingIDSQL (%t), want %t", node.id, parsed, findingSQL, want)
			}
		}
	}
}

// requireVisibilityNotAdmitted fails unless err is the store's refusal of a node of kind reserved
// for its author: the sentinel a route answers 400 VISIBILITY_NOT_ADMITTED for, and the typed
// refusal naming the kind, in words.
func requireVisibilityNotAdmitted(t *testing.T, err error, kind string) {
	t.Helper()
	if !errors.Is(err, ErrVisibilityNotAdmitted) {
		t.Fatalf("the write answered %v, want ErrVisibilityNotAdmitted", err)
	}
	var refusal *VisibilityNotAdmittedError
	if !errors.As(err, &refusal) {
		t.Fatalf("the write answered %v, which is no VisibilityNotAdmittedError", err)
	}
	if refusal.Kind != kind {
		t.Errorf("the refusal names the kind %q, want %q", refusal.Kind, kind)
	}
	if !strings.Contains(refusal.Error(), "the node kind "+kind+" takes no node reserved for its author") {
		t.Errorf("the refusal reads %q, which does not name the kind %s", refusal.Error(), kind)
	}
}
