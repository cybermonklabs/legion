package graphstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The project, the issue and the nodes the pointer rules are tried over. The lenses are named
// after the kind each holds; the kindless one holds none, for the race of two writes over it.
const (
	pointerProject = "PTR"
	pointerIssue   = "PTR-1"

	lensPreMortem     = "lens-pre-mortem"
	lensPreMortemBare = "lens-pre-mortem-bare"
	lensInversion     = "lens-inversion"
	lensTradeOff      = "lens-trade-off"
	lensKindless      = "lens-kindless"
	riskPlaced        = "risk-placed"
	riskAstray        = "risk-astray"
	constraintHeld    = "constraint-held"
	kindInversion     = "inversion"
	kindTradeOff      = "trade-off"
)

var (
	pointerAt  = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	pointerWho = model.Actor{Kind: "user", ID: "alice"}
)

// pointerFixture is the store the rules are tried over: one issue's spec holding a requirement,
// a check and a paragraph block, each with the document_blocks row its type is read off, and a
// second check whose row names no node (legacyCheck), as a row written before the store's
// backfill or by a pod of the previous release leaves node_kind and node_address null; the three
// lens kinds, a lens of each kind (two pre-mortems, one of which nothing hangs under), a lens of
// no kind, two risks and a constraint. The lenses are invoked on the spec in one write, in the
// order lensPreMortem, lensPreMortemBare, lensInversion, lensTradeOff and lensKindless, which is
// the order of their part_of tuples.
type pointerFixture struct {
	database                                   *store.Store
	requirement, check, paragraph, legacyCheck string
}

func pointerNode(kind, id string) Node {
	return Node{
		Kind: kind, ID: id, Supertag: kind, Visibility: VisibilityProject, State: StateLive,
		Project: pointerProject, MadeAt: pointerAt, MadeBy: pointerWho, SourceEvent: NoEvent,
	}
}

func pointerTuple(fromKind, fromID, kind, toKind, toID string) Tuple {
	return Tuple{
		FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
		At: pointerAt, Who: pointerWho, SourceEvent: NoEvent, Asserted: true,
	}
}

func newPointerFixture(t *testing.T) pointerFixture {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ($1, 'Pointers', 'alice')`, pointerProject); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	artifact, refKey := pointerSpec(t, ctx, database, pointerProject, pointerIssue, 1, "The pointer rules")
	fixture := pointerFixture{
		database:    database,
		requirement: refKey + "#req", check: refKey + "#chk", paragraph: refKey + "#para",
		legacyCheck: refKey + "#chk-old",
	}

	var nodes []Node
	for _, kind := range []string{LensKindPreMortem, kindInversion, kindTradeOff} {
		nodes = append(nodes, pointerNode(KindLensKind, kind))
	}
	var tuples []Tuple
	for _, lens := range []struct{ id, kind string }{
		{lensPreMortem, LensKindPreMortem}, {lensPreMortemBare, LensKindPreMortem},
		{lensInversion, kindInversion}, {lensTradeOff, kindTradeOff}, {lensKindless, ""},
	} {
		nodes = append(nodes, pointerNode(KindLens, lens.id))
		tuples = append(tuples, pointerTuple(KindLens, lens.id, TuplePartOf, KindArtifact, refKey))
		if lens.kind != "" {
			tuples = append(tuples, pointerTuple(KindLens, lens.id, TupleHasKind, KindLensKind, lens.kind))
		}
	}
	nodes = append(nodes, pointerNode(KindRisk, riskPlaced), pointerNode(KindRisk, riskAstray),
		pointerNode(KindConstraint, constraintHeld))
	for _, block := range []string{fixture.requirement, fixture.check, fixture.paragraph} {
		nodes = append(nodes, pointerNode(blockKind, block))
	}
	// Each lens and its one kind, written as the lens route writes them: has_kind from a lens to
	// a lens_kind, one at a time, is the rule's own case.
	writeInOneTransaction(t, ctx, database.Pool, nodes, tuples, "the nodes the pointer rules are tried over")
	for _, block := range []struct{ id, blockID, blockType string }{
		{fixture.requirement, "req", blockTypeRequirement},
		{fixture.check, "chk", BlockTypeCheck},
		{fixture.paragraph, "para", "paragraph"},
	} {
		pointerBlockRow(t, ctx, database, artifact, block.blockID, block.id, block.blockType)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'chk-old', $2, $3, '', '', 1, now())
	`, artifact, fixture.legacyCheck, BlockTypeCheck); err != nil {
		t.Fatalf("write the row of the check that names no node: %v", err)
	}
	return fixture
}

// pointerSpec makes issue, number number of project, titled title, holding a spec, and answers the
// id and the ref_key of that spec.
func pointerSpec(t *testing.T, ctx context.Context, database *store.Store, project, issue string, number int, title string) (artifact, refKey string) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, $3, $4, '{"kind":"user","id":"alice"}', 'n')
	`, issue, project, number, title); err != nil {
		t.Fatalf("create the issue %s: %v", issue, err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, issue, project).Scan(&artifact, &refKey); err != nil {
		t.Fatalf("create the spec of %s: %v", issue, err)
	}
	return artifact, refKey
}

// pointerBlockRow writes the document_blocks row of the block blockID of the document artifact, of
// type blockType, naming its node id, which the store holds already: the row's foreign key names it.
func pointerBlockRow(t *testing.T, ctx context.Context, database *store.Store, artifact, blockID, id, blockType string) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, $4, $3, $5, '', '', 1, now())
	`, artifact, blockID, id, blockKind, blockType); err != nil {
		t.Fatalf("write the row of the %s block %s: %v", blockType, id, err)
	}
}

// TestTheStoreHoldsEachPointerToItsTypeRule holds every write of the store to the type rules of
// the pointer tuples and of a risk's place (spec requirement r-constraint-risk,
// pointerRules): each rule takes the kinds it allows, and refuses every other far end and near
// end with ErrInvalidPointer, as an InvalidPointerError naming the tuple kind, its far end's kind
// and the kinds the rule allows - with no statement of the write failing, so the transaction of a
// refused write still runs one, and once its caller abandons it the log holds no tuple more than
// it held (abandonRefusedWrite). A block's type is read off its document_blocks row whether or
// not that row names its node yet. A second has_kind of a lens is ErrLensKindAsserted. Write
// (QueueWrite) and Caused (QueueCaused) each hold the rule, Caused as well after PlanUncaused and
// QueueNodes, and WriteArms refuses a ruled tuple it cannot check. A has_kind retracted with none in
// its place is refused out of a lens that stays live, the write restating it live or not, and taken
// out of a lens the same write states gone, a pre-mortem among them, as R7's delete retracts every
// tuple out of a node it takes. A retarget of a lens's kind appends a retraction and an assertion
// and touches no part_of tuple; a retarget of a pre-mortem's away from pre-mortem is
// ErrLensKindKept in one body whether a risk hangs under it or not, and a retarget to pre-mortem
// is taken. Two writes asserting a kind of one kindless lens queue at the lens's row, and the
// second is refused.
func TestTheStoreHoldsEachPointerToItsTypeRule(t *testing.T) {
	fixture := newPointerFixture(t)
	database := fixture.database
	ctx := context.Background()

	t.Run("the kinds each rule allows", func(t *testing.T) {
		for _, accepted := range []struct {
			name  string
			tuple Tuple
		}{
			{"a risk under a pre-mortem", pointerTuple(KindRisk, riskPlaced, TuplePartOf, KindLens, lensPreMortem)},
			{"a lens triggered by a lens invoked before it", pointerTuple(KindLens, lensTradeOff, TupleTriggeredBy, KindLens, lensInversion)},
			{"a requirement held to a constraint", pointerTuple(blockKind, fixture.requirement, TupleConstrainedBy, KindConstraint, constraintHeld)},
			{"a lens kind held to a constraint", pointerTuple(KindLensKind, LensKindPreMortem, TupleConstrainedBy, KindConstraint, constraintHeld)},
			{"a risk prevented by a check", pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, fixture.check)},
			{"a risk prevented by a check whose row names no node", pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, fixture.legacyCheck)},
		} {
			writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{accepted.tuple}, accepted.name)
			requireHolds(t, ctx, database, accepted.tuple)
		}
		// A pre-mortem and a risk under it in one write, the risk's place stated before the
		// lens's kind: the place is read against the kind the whole write leaves the lens.
		lens, risk := "lens-written-with-its-risk", "risk-written-with-its-lens"
		place := pointerTuple(KindRisk, risk, TuplePartOf, KindLens, lens)
		writeInOneTransaction(t, ctx, database.Pool,
			[]Node{pointerNode(KindLens, lens), pointerNode(KindRisk, risk)},
			[]Tuple{place, pointerTuple(KindLens, lens, TupleHasKind, KindLensKind, LensKindPreMortem)},
			"a pre-mortem and its risk")
		requireHolds(t, ctx, database, place)
	})

	t.Run("each refusal, through every append path", func(t *testing.T) {
		hasKindAtAConstraint := pointerTuple(KindLens, lensTradeOff, TupleHasKind, KindConstraint, constraintHeld)
		// What a refusal names besides its tuple's kind, part by part (pointerNames): in its near
		// end and in its far end the kind of that end, and in its rule the kinds the rule allows -
		// a lens and a lens_kind for has_kind, a requirement, a lens_kind and a constraint for
		// constrained_by, a risk and a check for prevented_by, a risk, a lens and the pre-mortem
		// kind for a risk's part_of. These are the has_kind from a lens at a constraint's, the
		// constrained_by from a paragraph's, and the part_of from a risk at a lens of kind
		// inversion's, which names that kind in its far end.
		var (
			hasKindRule      = []string{KindLens, KindLensKind}
			constrainedRule  = []string{blockTypeRequirement, KindLensKind, KindConstraint}
			hasKindNames     = pointerNames{from: []string{KindLens}, to: []string{KindConstraint}, rule: hasKindRule}
			constrainedNames = pointerNames{from: []string{"paragraph"}, to: []string{KindConstraint}, rule: constrainedRule}
			underInversion   = pointerNames{from: []string{KindRisk}, to: []string{KindLens, kindInversion},
				rule: []string{KindRisk, KindLens, LensKindPreMortem}}
		)
		for _, refused := range []struct {
			name  string
			write func(context.Context, pgx.Tx) error
			// kind is the refused tuple's kind, and names what each part of the refusal names
			// besides it.
			kind  string
			names pointerNames
			// retracted marks the retraction of a lens's has_kind with none asserted in its place.
			retracted bool
			// sentinel is the refusal the write answers when it is not an InvalidPointerError.
			sentinel error
		}{
			{name: "Write, has_kind at a node that is no lens_kind",
				write: tuplesOnly(hasKindAtAConstraint), kind: TupleHasKind, names: hasKindNames},
			{name: "Write, has_kind out of a node that is no lens",
				write: tuplesOnly(pointerTuple(KindRisk, riskPlaced, TupleHasKind, KindLensKind, kindInversion)),
				kind:  TupleHasKind, names: pointerNames{from: []string{KindRisk}, to: []string{KindLensKind}, rule: hasKindRule}},
			{name: "Write, a second has_kind",
				write:    tuplesOnly(pointerTuple(KindLens, lensInversion, TupleHasKind, KindLensKind, kindTradeOff)),
				sentinel: ErrLensKindAsserted},
			{name: "Write, a has_kind retracted with none asserted in its place",
				write: tuplesOnly(retracted(pointerTuple(KindLens, lensTradeOff, TupleHasKind, KindLensKind, kindTradeOff))),
				kind:  TupleHasKind, names: pointerNames{from: []string{KindLens}}, retracted: true},
			{name: "Write, a has_kind retracted with none asserted in its place out of a lens the write restates live",
				write: func(ctx context.Context, tx pgx.Tx) error {
					return Write(ctx, tx, []Node{pointerNode(KindLens, lensTradeOff)},
						[]Tuple{retracted(pointerTuple(KindLens, lensTradeOff, TupleHasKind, KindLensKind, kindTradeOff))})
				},
				kind: TupleHasKind, names: pointerNames{from: []string{KindLens}}, retracted: true},
			{name: "Write, a lens triggered by itself",
				write: tuplesOnly(pointerTuple(KindLens, lensInversion, TupleTriggeredBy, KindLens, lensInversion)),
				kind:  TupleTriggeredBy, names: triggerNames(lensInversion, lensInversion)},
			{name: "Write, constrained_by out of a paragraph",
				write: tuplesOnly(pointerTuple(blockKind, fixture.paragraph, TupleConstrainedBy, KindConstraint, constraintHeld)),
				kind:  TupleConstrainedBy, names: constrainedNames},
			{name: "Write, constrained_by at a node that is no constraint",
				write: tuplesOnly(pointerTuple(blockKind, fixture.requirement, TupleConstrainedBy, KindLens, lensInversion)),
				kind:  TupleConstrainedBy, names: pointerNames{from: []string{blockTypeRequirement}, to: []string{KindLens}, rule: constrainedRule}},
			{name: "Write, prevented_by at a block that is no check",
				write: tuplesOnly(pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, fixture.paragraph)),
				kind:  TuplePreventedBy, names: pointerNames{from: []string{KindRisk}, to: []string{"paragraph"}, rule: []string{KindRisk, BlockTypeCheck}}},
			{name: "Write, a risk under an inversion",
				write: tuplesOnly(pointerTuple(KindRisk, riskAstray, TuplePartOf, KindLens, lensInversion)),
				kind:  TuplePartOf, names: underInversion},
			{name: "Caused, has_kind held for the cause at a node that is no lens_kind",
				write: func(ctx context.Context, tx pgx.Tx) error {
					return Caused(ctx, tx, Uncaused{Tuples: []UncausedTuple{{Tuple: hasKindAtAConstraint}}}, NoEvent)
				},
				kind: TupleHasKind, names: hasKindNames},
			{name: "PlanUncaused and QueueNodes then Caused, constrained_by out of a paragraph",
				write: func(ctx context.Context, tx pgx.Tx) error {
					write := PlanUncaused(nil, []Tuple{
						pointerTuple(blockKind, fixture.paragraph, TupleConstrainedBy, KindConstraint, constraintHeld),
					})
					if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
						QueueNodes(b, write)
						return nil
					}); err != nil {
						return err
					}
					return Caused(ctx, tx, write, NoEvent)
				},
				kind: TupleConstrainedBy, names: constrainedNames},
		} {
			t.Run(refused.name, func(t *testing.T) {
				before := countTuples(t, ctx, database)
				tx, err := database.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer tx.Rollback(ctx)
				err = refused.write(ctx, tx)
				if refused.sentinel != nil {
					requireAlreadyAsserted(t, err, refused.sentinel)
				} else if refusal := requirePointerRefused(t, err, refused.kind, refused.names); refusal.Retracted != refused.retracted {
					t.Errorf("the refusal %q is a retraction's %t, want %t", refusal.Error(), refusal.Retracted, refused.retracted)
				}
				// The refused write's transaction is abandoned, as its caller abandons it, and
				// holds no tuple the refused write stated (abandonRefusedWrite).
				abandonRefusedWrite(t, ctx, tx)
				if after := countTuples(t, ctx, database); after != before {
					t.Errorf("the log holds %d tuples after the refused write, %d before it", after, before)
				}
			})
		}

		t.Run("Write, a risk written with a lens that is no pre-mortem", func(t *testing.T) {
			lens, risk := "lens-astray", "risk-under-lens-astray"
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			err = Write(ctx, tx, []Node{pointerNode(KindLens, lens), pointerNode(KindRisk, risk)}, []Tuple{
				pointerTuple(KindLens, lens, TupleHasKind, KindLensKind, kindInversion),
				pointerTuple(KindRisk, risk, TuplePartOf, KindLens, lens),
			})
			requirePointerRefused(t, err, TuplePartOf, underInversion)
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("roll the refused write back: %v", err)
			}
			requireNoNode(t, ctx, database, KindLens, lens)
			requireNoNode(t, ctx, database, KindRisk, risk)
		})

		t.Run("WriteArms, a ruled tuple or claim", func(t *testing.T) {
			if _, _, err := WriteArms(1, nil, []Tuple{hasKindAtAConstraint}, nil); !errors.Is(err, errPointerInArms) {
				t.Errorf("WriteArms of a has_kind tuple = %v, want errPointerInArms", err)
			}
			claim := Claim{
				FromKind: KindRisk, FromID: riskAstray, Kind: TuplePartOf, ToKind: KindLens, ToID: lensPreMortem,
				At: pointerAt, Who: pointerWho, SourceEvent: NoEvent,
			}
			if _, _, _, err := WriteArmsUncaused(1, nil, nil, []Claim{claim}); !errors.Is(err, errPointerInArms) {
				t.Errorf("WriteArmsUncaused of a risk's part_of claim = %v, want errPointerInArms", err)
			}
			// A part_of out of any node but a risk is a place no rule holds.
			page := Claim{
				FromKind: KindPage, FromID: RolePageID("worker"), Kind: TuplePartOf, ToKind: KindRole, ToID: "worker",
				At: pointerAt, Who: pointerWho, SourceEvent: NoEvent,
			}
			if _, _, err := WriteArms(1, nil, nil, []Claim{page}); err != nil {
				t.Errorf("WriteArms of a page's part_of = %v, want it taken", err)
			}
		})
	})

	t.Run("a retarget", func(t *testing.T) {
		kindOf := func(lens string) Tuple {
			return pointerTuple(KindLens, lens, TupleHasKind, KindLensKind, "")
		}
		retarget := func(lens, from, to string) error {
			pointer := kindOf(lens)
			pointer.ToID = from
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			if err := Retarget(ctx, tx, pointer, KindLensKind, to); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}

		placesBefore := len(historyOf(t, ctx, database, KindLens, lensTradeOff, TuplePartOf))
		kindsBefore := len(historyOf(t, ctx, database, KindLens, lensTradeOff, TupleHasKind))
		if err := retarget(lensTradeOff, kindTradeOff, kindInversion); err != nil {
			t.Fatalf("retarget the trade-off analysis at inversion: %v", err)
		}
		kinds := historyOf(t, ctx, database, KindLens, lensTradeOff, TupleHasKind)
		if len(kinds) != kindsBefore+2 {
			t.Fatalf("the retarget appended %d has_kind tuples, want a retraction and an assertion", len(kinds)-kindsBefore)
		}
		if moved := kinds[len(kinds)-2:]; moved[0].Asserted || moved[0].ToID != kindTradeOff || !moved[1].Asserted || moved[1].ToID != kindInversion {
			t.Errorf("the retarget appended %+v, want the retraction of %s then the assertion of %s", moved, kindTradeOff, kindInversion)
		}
		requireKinds(t, ctx, database, lensTradeOff, kindInversion)
		if places := len(historyOf(t, ctx, database, KindLens, lensTradeOff, TuplePartOf)); places != placesBefore {
			t.Errorf("the retarget moved the lens's part_of: %d tuples, %d before", places, placesBefore)
		}

		before := countTuples(t, ctx, database)
		kept := retarget(lensPreMortem, LensKindPreMortem, kindInversion)
		bare := retarget(lensPreMortemBare, LensKindPreMortem, kindInversion)
		requireAlreadyAsserted(t, kept, ErrLensKindKept)
		requireAlreadyAsserted(t, bare, ErrLensKindKept)
		if kept.Error() != bare.Error() {
			t.Errorf("a pre-mortem a risk hangs under reads %q, one nothing hangs under %q: want one body", kept, bare)
		}
		if after := countTuples(t, ctx, database); after != before {
			t.Errorf("the refused retargets appended %d tuples", after-before)
		}
		requireKinds(t, ctx, database, lensPreMortem, LensKindPreMortem)

		if err := retarget(lensTradeOff, kindInversion, LensKindPreMortem); err != nil {
			t.Fatalf("retarget a lens at pre-mortem: %v", err)
		}
		requireKinds(t, ctx, database, lensTradeOff, LensKindPreMortem)

		if err := retarget(lensInversion, kindTradeOff, LensKindPreMortem); !errors.Is(err, ErrPointerNotHeld) {
			t.Errorf("a retarget from a kind the lens does not hold = %v, want ErrPointerNotHeld", err)
		}
		place := pointerTuple(KindRisk, riskPlaced, TuplePartOf, KindLens, lensPreMortem)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := Retarget(ctx, tx, place, KindLens, lensPreMortemBare); err == nil {
			t.Error("a retarget of a risk's part_of was taken, want it left to the parent route")
		}
	})

	t.Run("a lens the write states gone", func(t *testing.T) {
		for _, lens := range []struct{ id, kind string }{
			{"lens-trade-off-deleted", kindTradeOff}, {"lens-pre-mortem-deleted", LensKindPreMortem},
		} {
			kind := pointerTuple(KindLens, lens.id, TupleHasKind, KindLensKind, lens.kind)
			writeInOneTransaction(t, ctx, database.Pool, []Node{pointerNode(KindLens, lens.id)}, []Tuple{kind}, "the lens "+lens.id)
			gone := pointerNode(KindLens, lens.id)
			gone.State = StateGone
			writeInOneTransaction(t, ctx, database.Pool, []Node{gone}, []Tuple{retracted(kind)}, "the delete of the lens "+lens.id)
			if held, err := CurrentOut(ctx, database.Pool, KindLens, lens.id, []string{TupleHasKind}); err != nil || len(held) != 0 {
				t.Errorf("the deleted lens %s holds has_kind %+v (%v), want none", lens.id, held, err)
			}
		}
	})

	t.Run("two writes asserting a kind of one kindless lens", func(t *testing.T) {
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the first write: %v", err)
		}
		defer holder.Rollback(ctx)
		if err := Write(ctx, holder, nil, []Tuple{pointerTuple(KindLens, lensKindless, TupleHasKind, KindLensKind, kindInversion)}); err != nil {
			t.Fatalf("the first write of the lens's kind: %v", err)
		}
		conn, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a connection for the second write: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			defer conn.Release()
			tx, err := conn.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			defer tx.Rollback(ctx)
			if err := Write(ctx, tx, nil, []Tuple{pointerTuple(KindLens, lensKindless, TupleHasKind, KindLensKind, kindTradeOff)}); err != nil {
				done <- err
				return
			}
			done <- tx.Commit(ctx)
		}()
		requireQueued(t, holder, conn, done, "the second write of the lens's kind")
		if err := holder.Commit(ctx); err != nil {
			t.Fatalf("commit the first write of the lens's kind: %v", err)
		}
		select {
		case err := <-done:
			requireAlreadyAsserted(t, err, ErrLensKindAsserted)
		case <-time.After(unblockedWait):
			t.Fatalf("the second write did not answer within %s of the first committing", unblockedWait)
		}
		requireKinds(t, ctx, database, lensKindless, kindInversion)
	})
}

// TestTheArmsRefuseEveryPartOfOutOfALens: a lens's part_of is appended only through QueueWrite or
// QueueCaused, whose write records the row of the document it places the lens on
// (queueLensArtifactIDs), so the arms of a spliced write refuse it (refusePointersInArms): WriteArms
// of a part_of tuple out of a lens at a document, WriteArms of one at a document block, and
// WriteArmsUncaused of a part_of claim out of a lens at a document each answer errPointerInArms,
// naming the tuple's kind, part_of, and the kind it goes out of, lens, and build no arm. WriteArms
// of a page's part_of claim at a role is still taken. Each that does not hold fails naming the
// call, what it was given and the kind of the far end.
func TestTheArmsRefuseEveryPartOfOutOfALens(t *testing.T) {
	const lens = "lens-placed-in-arms"
	document, block := "PTR-1/notes-md", "PTR-1/notes-md#paragraph"
	tuple := func(toKind, toID string) Tuple {
		return Tuple{
			FromKind: KindLens, FromID: lens, Kind: TuplePartOf, ToKind: toKind, ToID: toID,
			At: pointerAt, Who: pointerWho, SourceEvent: NoEvent, Asserted: true,
		}
	}
	claim := Claim{
		FromKind: KindLens, FromID: lens, Kind: TuplePartOf, ToKind: KindArtifact, ToID: document,
		At: pointerAt, Who: pointerWho, SourceEvent: NoEvent,
	}
	for _, call := range []struct {
		name, form, kind string
		write            func() (string, error)
	}{
		{"WriteArms", "tuple", KindArtifact, func() (string, error) {
			arms, _, err := WriteArms(1, nil, []Tuple{tuple(KindArtifact, document)}, nil)
			return arms, err
		}},
		{"WriteArms", "tuple", blockKind, func() (string, error) {
			arms, _, err := WriteArms(1, nil, []Tuple{tuple(blockKind, block)}, nil)
			return arms, err
		}},
		{"WriteArmsUncaused", "claim", KindArtifact, func() (string, error) {
			arms, _, _, err := WriteArmsUncaused(1, nil, nil, []Claim{claim})
			return arms, err
		}},
	} {
		if arms, err := call.write(); !errors.Is(err, errPointerInArms) || !graphstoretest.NamesAsAWord(err.Error(), TuplePartOf, KindLens) || arms != "" {
			t.Errorf("%s of a lens's part_of %s at %s = %v, want errPointerInArms naming %s and %s", call.name, call.form, call.kind, err, TuplePartOf, KindLens)
		}
	}
	page := Claim{
		FromKind: KindPage, FromID: RolePageID("worker"), Kind: TuplePartOf, ToKind: KindRole, ToID: "worker",
		At: pointerAt, Who: pointerWho, SourceEvent: NoEvent,
	}
	if _, _, err := WriteArms(1, nil, nil, []Claim{page}); err != nil {
		t.Errorf("WriteArms of a page's part_of claim at a role = %v, want it taken", err)
	}
}

// TestTheStoreHoldsARisksCheckToTheRisksProject holds a risk's prevented_by to a check of the
// risk's own project (spec requirement r-constraint-risk, pointerRules' ownProject): a
// check of another issue of that project is taken, as the pre-mortem conversion writes one, and a
// check of another project is refused, before anything is appended, with an InvalidPointerError
// naming both projects and the check - through Write, through a write that makes its risk, and
// through Retarget, which leaves the pointer where it was. NamedAt names that check by the address
// a route read it at, and names no other far end and no other refusal.
func TestTheStoreHoldsARisksCheckToTheRisksProject(t *testing.T) {
	fixture := newPointerFixture(t)
	database := fixture.database
	ctx := context.Background()
	_, ownProjects := checkOf(t, ctx, database, pointerProject, 2)
	_, foreign := checkOf(t, ctx, database, "FAR", 1)
	const address = "dispatch://FAR-1/spec#b-chk"

	held := pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, ownProjects)
	writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{held}, "a risk prevented by a check of another issue of its project")
	requireHolds(t, ctx, database, held)

	before := countTuples(t, ctx, database)
	for _, write := range []struct {
		name  string
		write func(context.Context, pgx.Tx) error
	}{
		{"Write, a stored risk at a check of another project",
			tuplesOnly(pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, foreign))},
		{"Write, a risk written with its check of another project", func(ctx context.Context, tx pgx.Tx) error {
			return Write(ctx, tx, []Node{pointerNode(KindRisk, "risk-astray-project")}, []Tuple{
				pointerTuple(KindRisk, "risk-astray-project", TuplePartOf, KindLens, lensPreMortem),
				pointerTuple(KindRisk, "risk-astray-project", TuplePreventedBy, blockKind, foreign),
			})
		}},
		{"Retarget, from the risk's check to one of another project", func(ctx context.Context, tx pgx.Tx) error {
			return Retarget(ctx, tx, held, blockKind, foreign)
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			// The refusal names both projects, which tell it from the refusal of a retired
			// document's check.
			requireCheckRefused(t, ctx, database, write.write, foreign, address, ownProjects, before, []string{pointerProject}, []string{"FAR"})
		})
	}
	requireNoNode(t, ctx, database, KindRisk, "risk-astray-project")
	requireHolds(t, ctx, database, held)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var typeRefusal *InvalidPointerError
	if !errors.As(Write(ctx, tx, nil, []Tuple{pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, fixture.paragraph)}), &typeRefusal) {
		t.Fatal("a prevented_by at a paragraph was taken")
	}
	if named, ok := typeRefusal.NamedAt(blockKind, fixture.paragraph, address); ok {
		t.Errorf("a refusal of the far end's type named at %s reads %q, want it not named", address, named.Error())
	}
}

// TestTheStoreHoldsATriggerToALensInvokedBefore holds a lens's triggered_by to a lens invoked
// before it (spec requirement r-lens-trigger, pointerRules' earlier), in the order the
// lens read answers: the seq of each lens's first part_of (InvokedSeqSQL). The fixture invokes its
// lenses in one write, the pre-mortem, the bare pre-mortem, the inversion, the trade-off analysis
// and the kindless lens, in that order. A trigger of an earlier lens is taken, through Write and
// through a write that invokes the lens it goes out of, whose own part_of puts it after every lens
// the store holds; two lenses one write invokes stand in the order of their part_of tuples there. A
// trigger of a lens invoked later, and a retarget at one or at the lens itself, are refused before
// anything is appended, with an InvalidPointerError naming both lenses, and a retarget leaves the
// pointer where it was. A lens moved to another parent, which appends a later part_of, keeps its
// place in the order. NamedAt names each lens by the address a route read it at, and the one lens
// of a self-trigger twice.
func TestTheStoreHoldsATriggerToALensInvokedBefore(t *testing.T) {
	fixture := newPointerFixture(t)
	database := fixture.database
	ctx := context.Background()
	addressOf := func(lens string) string { return "dispatch://lens/" + lens }
	spec := strings.TrimSuffix(fixture.requirement, "#req")

	held := pointerTuple(KindLens, lensTradeOff, TupleTriggeredBy, KindLens, lensInversion)
	writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{held}, "a trigger of a lens invoked before")
	requireHolds(t, ctx, database, held)
	invoked := "lens-invoked-with-its-trigger"
	made := pointerTuple(KindLens, invoked, TupleTriggeredBy, KindLens, lensKindless)
	writeInOneTransaction(t, ctx, database.Pool, []Node{pointerNode(KindLens, invoked)}, []Tuple{
		pointerTuple(KindLens, invoked, TuplePartOf, KindArtifact, spec), made,
	}, "a lens invoked with its trigger of the lens the store invoked last")
	requireHolds(t, ctx, database, made)
	first, second := "lens-invoked-first-in-one-write", "lens-invoked-second-in-one-write"
	pair := pointerTuple(KindLens, second, TupleTriggeredBy, KindLens, first)
	writeInOneTransaction(t, ctx, database.Pool, []Node{pointerNode(KindLens, first), pointerNode(KindLens, second)}, []Tuple{
		pointerTuple(KindLens, first, TuplePartOf, KindArtifact, spec), pointerTuple(KindLens, second, TuplePartOf, KindArtifact, spec), pair,
	}, "two lenses invoked in one write, the second triggered by the first")
	requireHolds(t, ctx, database, pair)
	// The inversion moves under the issue: its newest part_of is later than the trade-off
	// analysis's, and its first is not.
	writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{
		retracted(pointerTuple(KindLens, lensInversion, TuplePartOf, KindArtifact, spec)),
		pointerTuple(KindLens, lensInversion, TuplePartOf, KindIssue, pointerIssue),
	}, "the inversion moved under the issue")

	before := countTuples(t, ctx, database)
	for _, refused := range []struct {
		name     string
		write    func(context.Context, pgx.Tx) error
		near, to string
	}{
		{"Write, a trigger of a lens invoked later, by a lens moved since",
			tuplesOnly(pointerTuple(KindLens, lensInversion, TupleTriggeredBy, KindLens, lensTradeOff)), lensInversion, lensTradeOff},
		{"Write, a trigger of the lens a write invokes after it", func(ctx context.Context, tx pgx.Tx) error {
			return Write(ctx, tx, []Node{pointerNode(KindLens, "lens-astray-first"), pointerNode(KindLens, "lens-astray-second")}, []Tuple{
				pointerTuple(KindLens, "lens-astray-first", TuplePartOf, KindArtifact, spec),
				pointerTuple(KindLens, "lens-astray-second", TuplePartOf, KindArtifact, spec),
				pointerTuple(KindLens, "lens-astray-first", TupleTriggeredBy, KindLens, "lens-astray-second"),
			})
		}, "lens-astray-first", "lens-astray-second"},
		{"Write, a trigger of a stored lens by a lens the write makes and places nowhere",
			func(ctx context.Context, tx pgx.Tx) error {
				return Write(ctx, tx, []Node{pointerNode(KindLens, "lens-astray-unplaced")}, []Tuple{
					pointerTuple(KindLens, lensPreMortem, TupleTriggeredBy, KindLens, "lens-astray-unplaced"),
				})
			}, lensPreMortem, "lens-astray-unplaced"},
		{"Retarget, from a lens invoked before to a lens invoked later", func(ctx context.Context, tx pgx.Tx) error {
			return Retarget(ctx, tx, held, KindLens, lensKindless)
		}, lensTradeOff, lensKindless},
		{"Retarget, from a lens invoked before to the lens itself", func(ctx context.Context, tx pgx.Tx) error {
			return Retarget(ctx, tx, held, KindLens, lensTradeOff)
		}, lensTradeOff, lensTradeOff},
	} {
		t.Run(refused.name, func(t *testing.T) {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			err = refused.write(ctx, tx)
			refusal := requirePointerRefused(t, err, TupleTriggeredBy, triggerNames(refused.near, refused.to))
			named, ok := refusal.NamedAt(KindLens, refused.near, addressOf(refused.near))
			if ok && refused.to != refused.near {
				named, ok = named.NamedAt(KindLens, refused.to, addressOf(refused.to))
			}
			// Named at both lenses' addresses, the refusal names each lens by its address, in the
			// end it is, and neither lens by its id.
			if !ok || graphstoretest.NamesAsAWord(named.Error(), refused.near) || graphstoretest.NamesAsAWord(named.Error(), refused.to) {
				t.Errorf("the refusal named at both lenses' addresses reads %v (named %t), want it named, and naming neither lens by its id", named, ok)
			} else {
				requireNamedByRole(t, named, triggerNames(addressOf(refused.near), addressOf(refused.to)))
			}
			if other, ok := refusal.NamedAt(KindLens, lensPreMortemBare, addressOf(lensPreMortemBare)); ok {
				t.Errorf("the refusal named at a lens it does not name reads %q, want it not named", other.Error())
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("roll the refused write back: %v", err)
			}
			if after := countTuples(t, ctx, database); after != before {
				t.Errorf("the log holds %d tuples after the refused write, %d before it", after, before)
			}
		})
	}
	for _, lens := range []string{"lens-astray-first", "lens-astray-second", "lens-astray-unplaced"} {
		requireNoNode(t, ctx, database, KindLens, lens)
	}
	requireHolds(t, ctx, database, held)
	if triggers := historyOf(t, ctx, database, KindLens, lensInversion, TupleTriggeredBy); len(triggers) != 0 {
		t.Errorf("the inversion holds the triggered_by tuples %+v after the refusals, want none", triggers)
	}
}

// TestTheStoreHoldsARisksCheckToADocumentThatIsNotRetired holds a risk's prevented_by to a check
// of a document that is not retired (spec requirement r-constraint-risk, pointerRules'
// unretired): a check of a retired document, whose checks nobody runs, is refused before anything
// is appended, with an InvalidPointerError naming the check and its document as retired, through
// Write, through a write that makes its risk, and through Retarget, which leaves the pointer where
// it was. The document_blocks row of the retired document's check stays, so its type is read all
// the same. NamedAt names the check by the address a route read it at, and names no other far end.
func TestTheStoreHoldsARisksCheckToADocumentThatIsNotRetired(t *testing.T) {
	fixture := newPointerFixture(t)
	database := fixture.database
	ctx := context.Background()
	artifact, check := checkOf(t, ctx, database, pointerProject, 3)
	standing := pointerTuple(KindRisk, riskAstray, TuplePreventedBy, blockKind, check)
	writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{standing}, "a risk prevented by the check before its document is retired")
	requireHolds(t, ctx, database, standing)
	// The document retired as docs.RetireDocument retires it, its retired_at set and its role
	// cleared; the retirement's own retraction of the tuple above is docs'.
	if _, err := database.Pool.Exec(ctx, `update artifacts set retired_at = now(), role = null where id = $1::uuid`, artifact); err != nil {
		t.Fatalf("retire the document: %v", err)
	}
	const address = "dispatch://PTR-3/spec#b-chk"
	held := pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, fixture.check)
	writeInOneTransaction(t, ctx, database.Pool, nil, []Tuple{held}, "a risk prevented by a check of a document that stands")
	requireHolds(t, ctx, database, held)

	before := countTuples(t, ctx, database)
	for _, write := range []struct {
		name  string
		write func(context.Context, pgx.Tx) error
	}{
		{"Write, a stored risk at a check of a retired document",
			tuplesOnly(pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, check))},
		{"Write, a risk written with its check of a retired document", func(ctx context.Context, tx pgx.Tx) error {
			return Write(ctx, tx, []Node{pointerNode(KindRisk, "risk-astray-retired")}, []Tuple{
				pointerTuple(KindRisk, "risk-astray-retired", TuplePartOf, KindLens, lensPreMortem),
				pointerTuple(KindRisk, "risk-astray-retired", TuplePreventedBy, blockKind, check),
			})
		}},
		{"Retarget, from the risk's check to one of a retired document", func(ctx context.Context, tx pgx.Tx) error {
			return Retarget(ctx, tx, held, blockKind, check)
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			// The refusal names its check's document as retired, which tells it from the refusal of
			// a check of another project.
			requireCheckRefused(t, ctx, database, write.write, check, address, fixture.check, before, nil, []string{"retired"})
		})
	}
	requireNoNode(t, ctx, database, KindRisk, "risk-astray-retired")
	requireHolds(t, ctx, database, held)
}

// tuplesOnly is a write of tuples alone, through Write.
func tuplesOnly(tuples ...Tuple) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error { return Write(ctx, tx, nil, tuples) }
}

// checkOf makes issue number number of project, whose spec holds the check block chk with the
// document_blocks row its type is read off, and answers the spec's id and the check's: a project
// other than pointerProject is made first.
func checkOf(t *testing.T, ctx context.Context, database *store.Store, project string, number int) (artifact, check string) {
	t.Helper()
	if project != pointerProject {
		if _, err := database.Pool.Exec(ctx,
			`insert into projects (key, name, default_assignee) values ($1, 'Elsewhere', 'alice')`, project); err != nil {
			t.Fatalf("create the project %s: %v", project, err)
		}
	}
	issue := fmt.Sprintf("%s-%d", project, number)
	artifact, refKey := pointerSpec(t, ctx, database, project, issue, number, "A check of its own")
	node := pointerNode(blockKind, refKey+"#chk")
	node.Project = project
	writeInOneTransaction(t, ctx, database.Pool, []Node{node}, nil, "the check of "+issue)
	pointerBlockRow(t, ctx, database, artifact, "chk", node.ID, BlockTypeCheck)
	return artifact, node.ID
}

// requireCheckRefused runs write in a transaction of its own and fails unless the store refuses
// it before anything is appended: an InvalidPointerError of a prevented_by tuple whose near end
// names each of near and whose far end names the check block check and each of far, which NamedAt
// names at address, its far end naming the check there by that address and each of far, its near
// end each of near, and the refusal naming the check by its id no more, and naming no other far
// end, other, there; and the log holding before tuples once the write is rolled back.
func requireCheckRefused(t *testing.T, ctx context.Context, database *store.Store, write func(context.Context, pgx.Tx) error,
	check, address, other string, before int, near, far []string) {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	err = write(ctx, tx)
	refusal := requirePointerRefused(t, err, TuplePreventedBy, pointerNames{from: near, to: append([]string{check}, far...)})
	if named, ok := refusal.NamedAt(blockKind, check, address); !ok || graphstoretest.NamesAsAWord(named.Error(), check) {
		t.Errorf("the refusal named at %s reads %v (named %t), want it named, and naming %s no more", address, named, ok, check)
	} else {
		requireNamedByRole(t, named, pointerNames{from: near, to: append([]string{address}, far...)})
	}
	if named, ok := refusal.NamedAt(blockKind, other, address); ok {
		t.Errorf("the refusal named at another far end reads %q, want it not named", named.Error())
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the refused write back: %v", err)
	}
	if after := countTuples(t, ctx, database); after != before {
		t.Errorf("the log holds %d tuples after the refused write, %d before it", after, before)
	}
}

// abandonRefusedWrite is what the caller of a refused write does with its transaction. A refusal
// that reads the store is the callback of a statement of the write phase, whose later statements
// - the write's append among them - have run by the time Send reads it, so the caller ends the
// transaction rather than commit it; a refusal decided in Go sends nothing. Either is no failed
// statement: a statement that failed would have aborted the transaction, which then runs nothing
// more, so the transaction still runs one before it is rolled back.
func abandonRefusedWrite(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `select 1`); err != nil {
		t.Fatalf("the transaction of the refused write runs no statement, so a statement of the write failed: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the refused write back: %v", err)
	}
}

func retracted(tuple Tuple) Tuple {
	tuple.Asserted = false
	return tuple
}

// pointerNames is what a test holds each part of a refusal of a pointer to name, each a word of its
// own (graphstoretest.NamesAsAWord): from its near end (InvalidPointerError.From), to its far end
// (To), and rule what its rule lets through (Rule). A part is asked for no name it does not carry.
type pointerNames struct{ from, to, rule []string }

// triggerNames is what the refusal of a triggered_by from the lens near at the lens far names:
// near in its near end and far in its far end, each beside the kind lens, and that kind in its
// rule. A lens that triggers itself is named in both ends.
func triggerNames(near, far string) pointerNames {
	return pointerNames{from: []string{KindLens, near}, to: []string{KindLens, far}, rule: []string{KindLens}}
}

// requirePointerRefused fails unless err is the store's refusal of a pointer at a node its rule
// does not allow: the sentinel a route answers 400 INVALID_POINTER for, as the typed refusal of a
// tuple of kind each of whose parts names what names holds it to (requireNamedByRole). It answers
// that refusal.
func requirePointerRefused(t *testing.T, err error, kind string, names pointerNames) *InvalidPointerError {
	t.Helper()
	if !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("the write answered %v, want ErrInvalidPointer", err)
	}
	var refusal *InvalidPointerError
	if !errors.As(err, &refusal) {
		t.Fatalf("the write answered %v, which is no InvalidPointerError", err)
	}
	if refusal.Kind != kind {
		t.Errorf("the refusal %q is of a %s tuple, want a %s tuple's", refusal.Error(), refusal.Kind, kind)
	}
	requireNamedByRole(t, refusal, names)
	return refusal
}

// requireNamedByRole fails unless the near end of refusal names each of names.from, its far end
// each of names.to and its rule each of names.rule, naming each part that does not: a refusal that
// names its far end where its near end belongs names both, so only each part read on its own
// tells it from one that names each where it belongs.
func requireNamedByRole(t *testing.T, refusal *InvalidPointerError, names pointerNames) {
	t.Helper()
	for _, part := range []struct {
		role, words string
		names       []string
	}{
		{"near end (From)", refusal.From, names.from},
		{"far end (To)", refusal.To, names.to},
		{"rule (Rule)", refusal.Rule, names.rule},
	} {
		if !graphstoretest.NamesAsAWord(part.words, part.names...) {
			t.Errorf("the refusal %q reads %q in its %s, want it to name %q there", refusal.Error(), part.words, part.role, part.names)
		}
	}
}

// requireAlreadyAsserted fails unless err is want, which a route answers 409
// POINTER_ALREADY_ASSERTED for.
func requireAlreadyAsserted(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, ErrPointerAlreadyAsserted) || !errors.Is(err, want) {
		t.Fatalf("the write answered %v, want %v", err, want)
	}
}

// requireHolds fails unless the store holds tuple asserted now.
func requireHolds(t *testing.T, ctx context.Context, database *store.Store, tuple Tuple) {
	t.Helper()
	held, err := CurrentOut(ctx, database.Pool, tuple.FromKind, tuple.FromID, []string{tuple.Kind})
	if err != nil {
		t.Fatalf("read the %s tuples of %s/%s: %v", tuple.Kind, tuple.FromKind, tuple.FromID, err)
	}
	if !slices.ContainsFunc(held, func(current Tuple) bool { return current.ToKind == tuple.ToKind && current.ToID == tuple.ToID }) {
		t.Errorf("%s/%s holds no %s to %s/%s after its write: %+v", tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, held)
	}
}

// requireKinds fails unless the lens holds exactly the one kind.
func requireKinds(t *testing.T, ctx context.Context, database *store.Store, lens, kind string) {
	t.Helper()
	held, err := CurrentOut(ctx, database.Pool, KindLens, lens, []string{TupleHasKind})
	if err != nil {
		t.Fatalf("read the kind of %s: %v", lens, err)
	}
	if len(held) != 1 || held[0].ToKind != KindLensKind || held[0].ToID != kind {
		t.Errorf("%s holds %+v, want the one kind %s", lens, held, kind)
	}
}

func countTuples(t *testing.T, ctx context.Context, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&count); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	return count
}
