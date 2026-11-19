package graphstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
// no kind, two risks and a constraint.
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
// the pointer tuples and of a risk's place (dispatch://INFRA-1950/spec#b-r-constraint-risk,
// pointerRules): each rule takes the kinds it allows, and refuses every other far end and near
// end with ErrInvalidPointer, as an InvalidPointerError naming the tuple kind, its far end's kind
// and the kinds the rule allows, before the write appends anything - the transaction of a refused
// write that wrote no node still commits, and the log holds no tuple more than it held. A block's
// type is read off its document_blocks row whether or not that row names its node yet. A second
// has_kind of a lens is ErrLensKindAsserted. Write, WriteUncaused and Caused each hold the rule,
// and WriteArms refuses a ruled tuple it cannot check. A retarget of a lens's kind appends a
// retraction and an assertion and touches no part_of tuple; a retarget of a pre-mortem's away
// from pre-mortem is ErrLensKindKept in one body whether a risk hangs under it or not, and a
// retarget to pre-mortem is taken. Two writes asserting a kind of one kindless lens queue at the
// lens's row, and the second is refused.
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
			{"a lens triggered by another lens", pointerTuple(KindLens, lensInversion, TupleTriggeredBy, KindLens, lensTradeOff)},
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
		const (
			hasKindRule     = "has_kind points from a lens at a lens_kind"
			constrainedRule = "constrained_by points from a requirement block or a lens_kind at a constraint"
		)
		for _, refused := range []struct {
			name    string
			write   func(context.Context, pgx.Tx) error
			message string
			// sentinel is the refusal the write answers when it is not an InvalidPointerError.
			sentinel error
		}{
			{name: "Write, has_kind at a node that is no lens_kind",
				write:   tuplesOnly(hasKindAtAConstraint),
				message: "the has_kind tuple from a lens at a constraint breaks its type rule: " + hasKindRule},
			{name: "Write, has_kind out of a node that is no lens",
				write:   tuplesOnly(pointerTuple(KindRisk, riskPlaced, TupleHasKind, KindLensKind, kindInversion)),
				message: "the has_kind tuple from a risk at a lens_kind breaks its type rule: " + hasKindRule},
			{name: "Write, a second has_kind",
				write:    tuplesOnly(pointerTuple(KindLens, lensInversion, TupleHasKind, KindLensKind, kindTradeOff)),
				sentinel: ErrLensKindAsserted, message: ErrLensKindAsserted.Reason},
			{name: "Write, a has_kind retracted with none asserted in its place",
				write: tuplesOnly(retracted(pointerTuple(KindLens, lensTradeOff, TupleHasKind, KindLensKind, kindTradeOff))),
				message: "the has_kind tuple from a lens is retracted with none asserted in its place, which breaks its " +
					"type rule: a lens holds exactly one kind, so its has_kind tuple is retargeted, never retracted alone"},
			{name: "Write, a lens triggered by itself",
				write: tuplesOnly(pointerTuple(KindLens, lensInversion, TupleTriggeredBy, KindLens, lensInversion)),
				message: "the triggered_by tuple from a lens at that same lens breaks its type rule: " +
					"triggered_by points from a lens at another lens"},
			{name: "Write, constrained_by out of a paragraph",
				write:   tuplesOnly(pointerTuple(blockKind, fixture.paragraph, TupleConstrainedBy, KindConstraint, constraintHeld)),
				message: "the constrained_by tuple from a paragraph block at a constraint breaks its type rule: " + constrainedRule},
			{name: "Write, constrained_by at a node that is no constraint",
				write:   tuplesOnly(pointerTuple(blockKind, fixture.requirement, TupleConstrainedBy, KindLens, lensInversion)),
				message: "the constrained_by tuple from a requirement block at a lens breaks its type rule: " + constrainedRule},
			{name: "Write, prevented_by at a block that is no check",
				write: tuplesOnly(pointerTuple(KindRisk, riskPlaced, TuplePreventedBy, blockKind, fixture.paragraph)),
				message: "the prevented_by tuple from a risk at a paragraph block breaks its type rule: " +
					"prevented_by points from a risk at a check block"},
			{name: "Write, a risk under an inversion",
				write: tuplesOnly(pointerTuple(KindRisk, riskAstray, TuplePartOf, KindLens, lensInversion)),
				message: "the part_of tuple from a risk at a lens of kind inversion breaks its type rule: " +
					"a risk belongs under a lens of kind pre-mortem"},
			{name: "Caused, has_kind held for the cause at a node that is no lens_kind",
				write: func(ctx context.Context, tx pgx.Tx) error {
					return Caused(ctx, tx, Uncaused{Tuples: []UncausedTuple{{Tuple: hasKindAtAConstraint}}}, NoEvent)
				},
				message: "the has_kind tuple from a lens at a constraint breaks its type rule: " + hasKindRule},
			{name: "WriteUncaused then Caused, constrained_by out of a paragraph",
				write: func(ctx context.Context, tx pgx.Tx) error {
					write, err := WriteUncaused(ctx, tx, nil, []Tuple{
						pointerTuple(blockKind, fixture.paragraph, TupleConstrainedBy, KindConstraint, constraintHeld),
					})
					if err != nil {
						return err
					}
					return Caused(ctx, tx, write, NoEvent)
				},
				message: "the constrained_by tuple from a paragraph block at a constraint breaks its type rule: " + constrainedRule},
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
					if err.Error() != refused.message {
						t.Errorf("the refusal reads %q, want %q", err.Error(), refused.message)
					}
				} else {
					requirePointerRefused(t, err, refused.message)
				}
				// The refusal fails the write's batch, and its caller abandons the transaction
				// (Write): once it rolls back, the log holds no tuple the refused write stated.
				if err := tx.Rollback(ctx); err != nil {
					t.Fatalf("roll back the transaction of the refused write: %v", err)
				}
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
			requirePointerRefused(t, err, "the part_of tuple from a risk at a lens of kind inversion breaks its type rule: "+
				"a risk belongs under a lens of kind pre-mortem")
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

		placesBefore := len(readHistory(t, ctx, database, KindLens, lensTradeOff, TuplePartOf))
		kindsBefore := len(readHistory(t, ctx, database, KindLens, lensTradeOff, TupleHasKind))
		if err := retarget(lensTradeOff, kindTradeOff, kindInversion); err != nil {
			t.Fatalf("retarget the trade-off analysis at inversion: %v", err)
		}
		kinds := readHistory(t, ctx, database, KindLens, lensTradeOff, TupleHasKind)
		if len(kinds) != kindsBefore+2 {
			t.Fatalf("the retarget appended %d has_kind tuples, want a retraction and an assertion", len(kinds)-kindsBefore)
		}
		if moved := kinds[len(kinds)-2:]; moved[0].Asserted || moved[0].ToID != kindTradeOff || !moved[1].Asserted || moved[1].ToID != kindInversion {
			t.Errorf("the retarget appended %+v, want the retraction of %s then the assertion of %s", moved, kindTradeOff, kindInversion)
		}
		requireKinds(t, ctx, database, lensTradeOff, kindInversion)
		if places := len(readHistory(t, ctx, database, KindLens, lensTradeOff, TuplePartOf)); places != placesBefore {
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

// TestTheStoreHoldsARisksCheckToTheRisksProject holds a risk's prevented_by to a check of the
// risk's own project (dispatch://INFRA-1950/spec#b-r-constraint-risk, pointerRules' ownProject): a
// check of another issue of that project is taken, as the pre-mortem conversion writes one, and a
// check of another project is refused, before anything is appended, with an InvalidPointerError
// naming both projects and the check - through Write, through a write that makes its risk, and
// through Retarget, which leaves the pointer where it was. NamedAt names that check by the address
// a route read it at, and names no other far end and no other refusal.
func TestTheStoreHoldsARisksCheckToTheRisksProject(t *testing.T) {
	fixture := newPointerFixture(t)
	database := fixture.database
	ctx := context.Background()
	// checkOf is the check block chk of the spec of a new issue of project, number number in it.
	checkOf := func(project string, number int) string {
		t.Helper()
		if project != pointerProject {
			if _, err := database.Pool.Exec(ctx,
				`insert into projects (key, name, default_assignee) values ($1, 'Elsewhere', 'alice')`, project); err != nil {
				t.Fatalf("create the project %s: %v", project, err)
			}
		}
		issue := fmt.Sprintf("%s-%d", project, number)
		artifact, refKey := pointerSpec(t, ctx, database, project, issue, number, "A check of its own")
		check := pointerNode(blockKind, refKey+"#chk")
		check.Project = project
		writeInOneTransaction(t, ctx, database.Pool, []Node{check}, nil, "the check of "+issue)
		pointerBlockRow(t, ctx, database, artifact, "chk", check.ID, BlockTypeCheck)
		return check.ID
	}
	ownProjects, foreign := checkOf(pointerProject, 2), checkOf("FAR", 1)
	const (
		refused = "the prevented_by tuple from a risk of project " + pointerProject + " at the check block %s of project FAR " +
			"breaks its type rule: prevented_by points from a risk at a check block of the same project"
		address = "dispatch://FAR-1/spec#b-chk"
	)

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
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			err = write.write(ctx, tx)
			requirePointerRefused(t, err, fmt.Sprintf(refused, foreign))
			var refusal *InvalidPointerError
			errors.As(err, &refusal)
			if named, ok := refusal.NamedAt(blockKind, foreign, address); !ok || named.Error() != fmt.Sprintf(refused, address) {
				t.Errorf("the refusal named at %s reads %v (named %t), want %q", address, named, ok, fmt.Sprintf(refused, address))
			}
			if other, ok := refusal.NamedAt(blockKind, ownProjects, address); ok {
				t.Errorf("the refusal named at another far end reads %q, want it not named", other.Error())
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("roll the refused write back: %v", err)
			}
			if after := countTuples(t, ctx, database); after != before {
				t.Errorf("the log holds %d tuples after the refused write, %d before it", after, before)
			}
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

// tuplesOnly is a write of tuples alone, through Write.
func tuplesOnly(tuples ...Tuple) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error { return Write(ctx, tx, nil, tuples) }
}

func retracted(tuple Tuple) Tuple {
	tuple.Asserted = false
	return tuple
}

// requirePointerRefused fails unless err is the store's refusal of a pointer at a node its rule
// does not allow: the sentinel a route answers 400 INVALID_POINTER for, as the typed refusal
// reading message.
func requirePointerRefused(t *testing.T, err error, message string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("the write answered %v, want ErrInvalidPointer", err)
	}
	var refusal *InvalidPointerError
	if !errors.As(err, &refusal) {
		t.Fatalf("the write answered %v, which is no InvalidPointerError", err)
	}
	if refusal.Error() != message {
		t.Errorf("the refusal reads %q, want %q", refusal.Error(), message)
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

func readHistory(t *testing.T, ctx context.Context, database *store.Store, kind, id, tupleKind string) []Tuple {
	t.Helper()
	history, err := History(ctx, database.Pool, kind, id, tupleKind)
	if err != nil {
		t.Fatalf("read the %s history of %s/%s: %v", tupleKind, kind, id, err)
	}
	return history
}

func countTuples(t *testing.T, ctx context.Context, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&count); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	return count
}
