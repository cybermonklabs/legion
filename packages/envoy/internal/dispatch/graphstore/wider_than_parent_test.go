package graphstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The nodes reserved for their author that TestAPartOfOrOnTupleWiderThanItsParentIsRefused places
// nodes under and on: a pre-mortem invoked on the spec, and a constraint under the issue.
const (
	lensHidden       = "lens-hidden"
	constraintHidden = "constraint-hidden"
)

// TestAPartOfOrOnTupleWiderThanItsParentIsRefused holds the store to the rule that no node every
// reader may see stands under or on a node reserved for its author (spec requirement
// r-node-visibility, pointerCheck). A project risk's part_of moved to an author-only
// lens, and a project lens's part_of - the node it stands on - moved to an author-only
// constraint, each as a move to a new parent appends it, through the store's own tuple write, are
// refused with ErrVisibilityWiderThanParent, as a VisibilityWiderThanParentError naming both
// nodes, with no statement of the write failing: once its caller abandons the refused write's
// transaction, the log holds no tuple more than it held, and each node
// still stands where it stood. A write that makes a project risk under the hidden lens is refused
// too and leaves no node, Caused holds the rule as Write does, and WriteArms refuses such a
// part_of it cannot check. An author-only risk under the hidden lens is taken, and so is a
// triggered_by and a constrained_by out of a project node at a hidden one: the rule holds a
// node's place alone.
func TestAPartOfOrOnTupleWiderThanItsParentIsRefused(t *testing.T) {
	admitAuthorOnlyFor(t, KindLens, KindRisk, KindConstraint)
	fixture := newPointerFixture(t)
	database := fixture.database
	ctx := context.Background()
	refKey := strings.TrimSuffix(fixture.requirement, "#req")

	hidden := func(kind, id string) Node {
		node := pointerNode(kind, id)
		node.Visibility = VisibilityAuthor
		return node
	}
	writeInOneTransaction(t, ctx, database.Pool,
		[]Node{hidden(KindLens, lensHidden), hidden(KindConstraint, constraintHidden)},
		[]Tuple{
			pointerTuple(KindLens, lensHidden, TuplePartOf, KindArtifact, refKey),
			pointerTuple(KindLens, lensHidden, TupleHasKind, KindLensKind, LensKindPreMortem),
			pointerTuple(KindConstraint, constraintHidden, TuplePartOf, KindIssue, pointerIssue),
		}, "the lens and the constraint reserved for their author")
	placed := pointerTuple(KindRisk, riskPlaced, TuplePartOf, KindLens, lensPreMortem)
	writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{placed}, "the project risk under the project pre-mortem")
	onSpec := pointerTuple(KindLens, lensInversion, TuplePartOf, KindArtifact, refKey)

	for _, refused := range []struct {
		name                 string
		write                func(context.Context, pgx.Tx) error
		childKind, childID   string
		parentKind, parentID string
	}{
		{name: "Write, a project risk moved under a lens reserved for its author",
			write:     tuplesOnly(retracted(placed), pointerTuple(KindRisk, riskPlaced, TuplePartOf, KindLens, lensHidden)),
			childKind: KindRisk, childID: riskPlaced, parentKind: KindLens, parentID: lensHidden},
		{name: "Write, a project lens moved on to a constraint reserved for its author",
			write:     tuplesOnly(retracted(onSpec), pointerTuple(KindLens, lensInversion, TuplePartOf, KindConstraint, constraintHidden)),
			childKind: KindLens, childID: lensInversion, parentKind: KindConstraint, parentID: constraintHidden},
		{name: "Caused, a project risk's part_of held for the cause at a lens reserved for its author",
			write: func(ctx context.Context, tx pgx.Tx) error {
				return Caused(ctx, tx, Uncaused{Tuples: []UncausedTuple{
					{Tuple: pointerTuple(KindRisk, riskAstray, TuplePartOf, KindLens, lensHidden)},
				}}, NoEvent)
			},
			childKind: KindRisk, childID: riskAstray, parentKind: KindLens, parentID: lensHidden},
	} {
		t.Run(refused.name, func(t *testing.T) {
			before := countTuples(t, ctx, database)
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			err = refused.write(ctx, tx)
			requireWiderThanParent(t, err, refused.childKind, refused.childID, refused.parentKind, refused.parentID)
			// The refused write's transaction is abandoned, as its caller abandons it, and holds
			// no tuple the refused write stated (abandonRefusedWrite).
			abandonRefusedWrite(t, ctx, tx)
			if after := countTuples(t, ctx, database); after != before {
				t.Errorf("the log holds %d tuples after the refused write, %d before it", after, before)
			}
		})
	}
	requireStandsAt(t, ctx, database, KindRisk, riskPlaced, KindLens, lensPreMortem)
	requireStandsAt(t, ctx, database, KindLens, lensInversion, KindArtifact, refKey)
	requireStandsAt(t, ctx, database, KindRisk, riskAstray, "", "")

	t.Run("Write, a project risk made under a lens reserved for its author", func(t *testing.T) {
		risk := "risk-made-under-the-hidden-lens"
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		err = Write(ctx, tx, []Node{pointerNode(KindRisk, risk)},
			[]Tuple{pointerTuple(KindRisk, risk, TuplePartOf, KindLens, lensHidden)})
		requireWiderThanParent(t, err, KindRisk, risk, KindLens, lensHidden)
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("roll the refused write back: %v", err)
		}
		requireNoNode(t, ctx, database, KindRisk, risk)
	})

	t.Run("WriteArms, a part_of at a kind that takes a node reserved for its author", func(t *testing.T) {
		claim := Claim{
			FromKind: KindLens, FromID: lensInversion, Kind: TuplePartOf, ToKind: KindConstraint, ToID: constraintHidden,
			At: pointerAt, Who: pointerWho, SourceEvent: NoEvent,
		}
		if _, _, err := WriteArms(1, nil, nil, []Claim{claim}); !errors.Is(err, errPointerInArms) {
			t.Errorf("WriteArms of a lens's part_of at a constraint = %v, want errPointerInArms", err)
		}
	})

	t.Run("what the rule takes", func(t *testing.T) {
		// A node reserved for its author under its author's own, and the pointers that are no
		// place: each read leaves such a tuple out at both ends.
		risk, later := "risk-hidden", "lens-after-the-hidden-lens"
		taken := []Tuple{
			pointerTuple(KindRisk, risk, TuplePartOf, KindLens, lensHidden),
			pointerTuple(KindLens, later, TuplePartOf, KindArtifact, refKey),
			pointerTuple(KindLens, later, TupleHasKind, KindLensKind, kindInversion),
			pointerTuple(KindLens, later, TupleTriggeredBy, KindLens, lensHidden),
			pointerTuple(blockKind, fixture.requirement, TupleConstrainedBy, KindConstraint, constraintHidden),
		}
		writeInOneTransaction(t, ctx, database.Pool, []Node{hidden(KindRisk, risk), pointerNode(KindLens, later)}, taken,
			"an author-only risk under the hidden lens, and pointers at the hidden nodes")
		for _, tuple := range taken {
			requireHolds(t, ctx, database, tuple)
		}
	})
}

// admitAuthorOnlyFor admits kinds as the read registry's init does (AdmitAuthorOnly), for the
// test t alone, and takes them back when it ends: this package's tests load no registry, so the
// store takes no node reserved for its author of any kind but the one they admit for good
// (authorOnlyTestKind), which TestTheStoreRefusesAuthorOnAKindItDoesNotAdmit holds it to. No test
// of this package runs in parallel with another.
func admitAuthorOnlyFor(t *testing.T, kinds ...string) {
	t.Helper()
	AdmitAuthorOnly(kinds...)
	t.Cleanup(func() {
		for _, kind := range kinds {
			delete(authorOnlyKinds, kind)
		}
	})
}

// requireWiderThanParent fails unless err is the store's refusal of a node every reader may see
// placed under or on one reserved for its author, which a route answers 400
// VISIBILITY_WIDER_THAN_PARENT: the sentinel, as a VisibilityWiderThanParentError naming the child
// and the parent, whose words name both. A write the store took fails naming both nodes, and the
// test goes on to read what it stored.
func requireWiderThanParent(t *testing.T, err error, childKind, childID, parentKind, parentID string) {
	t.Helper()
	if !errors.Is(err, ErrVisibilityWiderThanParent) {
		t.Errorf("the part_of from the %s %s, visible to every reader of its project, to the %s %s, reserved for its author, "+
			"answered %v, want ErrVisibilityWiderThanParent (VISIBILITY_WIDER_THAN_PARENT)", childKind, childID, parentKind, parentID, err)
		return
	}
	var refusal *VisibilityWiderThanParentError
	if !errors.As(err, &refusal) {
		t.Fatalf("the write answered %v, which is no VisibilityWiderThanParentError", err)
	}
	if refusal.ChildKind != childKind || refusal.ChildID != childID || refusal.ParentKind != parentKind || refusal.ParentID != parentID {
		t.Errorf("the refusal names the %s %s under the %s %s, want the %s %s under the %s %s",
			refusal.ChildKind, refusal.ChildID, refusal.ParentKind, refusal.ParentID, childKind, childID, parentKind, parentID)
	}
	if !graphstoretest.NamesAsAWord(refusal.Error(), childKind, childID, parentKind, parentID) {
		t.Errorf("the refusal reads %q, want it to name the %s %s and the %s %s", refusal.Error(), childKind, childID, parentKind, parentID)
	}
	t.Logf("refused with VISIBILITY_WIDER_THAN_PARENT: %s", refusal.Error())
}

// requireStandsAt fails unless the node kind and id names holds exactly one current part_of, at
// the node toKind and toID name, or none where toKind is empty.
func requireStandsAt(t *testing.T, ctx context.Context, database *store.Store, kind, id, toKind, toID string) {
	t.Helper()
	held, err := CurrentOut(ctx, database.Pool, kind, id, []string{TuplePartOf})
	if err != nil {
		t.Fatalf("read where %s/%s stands: %v", kind, id, err)
	}
	switch {
	case toKind == "" && len(held) != 0:
		t.Errorf("%s/%s stands at %+v, want nowhere", kind, id, held)
	case toKind != "" && (len(held) != 1 || held[0].ToKind != toKind || held[0].ToID != toID):
		t.Errorf("%s/%s stands at %+v, want %s/%s alone", kind, id, held, toKind, toID)
	}
}
