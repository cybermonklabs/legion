package record

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// basisOf is the approved basis of ancestor's document at version, registered at registered: what
// a root's architect registers when the ancestor holds a person's approval of that version.
func basisOf(ancestor string, version int, registered time.Time) GateBasis {
	return GateBasis{
		Issue: ancestor, ArtifactID: "artifact-" + ancestor, Version: version,
		Approval: EventMark{Seq: 31, At: registered.Add(-time.Hour)}, Approver: "user:alice",
		Serves: []string{fmt.Sprintf("dispatch://%s/spec#b-r1-ancestor@v%d", ancestor, version)},
		Reason: "the root's checks serve r1 of the ancestor", Session: "ses_architect_208",
		RegisteredAt: registered,
	}
}

// A step a basis opened reads open to every reader of whether a step is open, with no rule of
// their own: GateOpen, the tree's gate and its first opening. The waiting line's moment for that
// opening is the daemon's own time of the registration (k1 of dispatch://INFRA-3320), never a
// zero time, since the basis's approval is a place in the ancestor's event log and the step
// records no approval of its own; a step a person opened keeps the moment of that approval, and
// the tree's moment is the later of its steps'.
func TestABasisOpeningTakesTheRegistrationsMomentInTheLine(t *testing.T) {
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	approvedAt := registered.Add(-2 * time.Hour)
	closed := DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4}
	opened := OpenOnBasis(closed, basisOf("LEGION-100", 3, registered))

	if !GateOpen(opened) || !opened.OnBasis() || opened.ApprovedVersion == nil || *opened.ApprovedVersion != 4 || opened.ApprovedMark != (EventMark{}) {
		t.Fatalf("the step a basis opened = %#v, want it open at the version the root registered, 4, with no mark of a person's approval", opened)
	}
	single := TreeGate{Rows: map[DesignStep]DesignGate{DesignStepSpec: opened}}
	if !single.Open() {
		t.Fatalf("the tree whose one step a basis opened reads closed, want it open as after a person's approval")
	}
	if got := single.OpenedAt(); !got.Equal(registered) {
		t.Fatalf("the tree's moment = %v, want the daemon's time of the registration %v, never zero", got, registered)
	}

	// PutGate lights EverOpened from GateOpen; read here as the store reads it.
	opened.EverOpened = GateOpen(opened)
	criteria := DesignGate{Issue: "LEGION-208", Step: DesignStepCriteria, ArtifactID: "artifact-criteria", LatestVersion: 2,
		ApprovedVersion: new(2), ApprovedMark: EventMark{Seq: 12, At: approvedAt}, EverOpened: true}
	both := TreeGate{Steps: []DesignStep{DesignStepCriteria, DesignStepSpec},
		Rows: map[DesignStep]DesignGate{DesignStepCriteria: criteria, DesignStepSpec: opened}}
	if !both.Open() || !both.EverOpened() {
		t.Fatalf("a tree whose steps a person and a basis opened reads open %t, ever opened %t; want both", both.Open(), both.EverOpened())
	}
	if got := both.OpenedAt(); !got.Equal(registered) {
		t.Fatalf("the two-step tree's moment = %v, want the later of the person's approval %v and the registration %v", got, approvedAt, registered)
	}

	// A new version of the root's own document ends the basis: the step reads closed, gives the
	// line no moment, and still reads as having opened once in this generation.
	newer := CloseBasis(opened)
	newer.LatestVersion = 5
	closedTree := TreeGate{Rows: map[DesignStep]DesignGate{DesignStepSpec: newer}}
	if closedTree.Open() || !closedTree.EverOpened() {
		t.Fatalf("the tree whose basis closed reads open %t, ever opened %t; want it closed and opened once", closedTree.Open(), closedTree.EverOpened())
	}
	if got := closedTree.OpenedAt(); !got.IsZero() {
		t.Fatalf("the moment of a step whose basis closed = %v, want zero for a gate that is not open", got)
	}
}

// The transitions keep a person's approval apart while a basis stands, and fall back to it, or
// to none, never to the version the basis opened (R2, R3). A step a person's approval holds open
// at its latest version stays on that approval. Each ancestor a basis opens the step on is one it
// rests on for the generation, and a pending wait rests on its ancestor only while it waits.
func TestTheBasisTransitionsKeepThePersonsApprovalApart(t *testing.T) {
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	personal := EventMark{Seq: 9, At: registered.Add(-48 * time.Hour)}
	earlier := DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
		ApprovedVersion: new(2), ApprovedMark: personal}

	opened := OpenOnBasis(earlier, basisOf("LEGION-100", 3, registered))
	if opened.KeptPerson == nil || *opened.KeptPerson != (PersonApproval{Version: 2, Mark: personal}) {
		t.Fatalf("the person's approval of version 2 is kept as %#v, want it held apart with its mark", opened.KeptPerson)
	}
	if approval := opened.ApprovedByPerson(); approval == nil || approval.Version != 2 || approval.Mark != personal {
		t.Fatalf("the person's approval read off the open step = %#v, want version 2, not the version the basis opened", approval)
	}
	if !slices.Equal(opened.RestsOn, []string{"LEGION-100"}) || !opened.RestsOnIssue("LEGION-100") || opened.RestsOnIssue("LEGION-101") {
		t.Fatalf("the open step rests on %q, want LEGION-100 alone", opened.RestsOn)
	}
	// OpenOnBasis takes the gate by value, so only what it shares with the gate it was given could
	// be written through: the approved version, through its pointer, and the rests, through the
	// slice's backing array. A gate whose rests have room to grow, opened once on each of two other
	// ancestors, shows the second: the first opening still rests on its own ancestor, not the second's.
	if *earlier.ApprovedVersion != 2 {
		t.Fatalf("the transition wrote through the approved version of the gate it was given: %#v", earlier)
	}
	roomy := earlier
	roomy.RestsOn = append(make([]string, 0, 2), "LEGION-100")
	first := OpenOnBasis(roomy, basisOf("LEGION-101", 3, registered))
	second := OpenOnBasis(roomy, basisOf("LEGION-102", 3, registered))
	if !slices.Equal(first.RestsOn, []string{"LEGION-100", "LEGION-101"}) || !slices.Equal(second.RestsOn, []string{"LEGION-100", "LEGION-102"}) ||
		!slices.Equal(roomy.RestsOn, []string{"LEGION-100"}) {
		t.Fatalf("two openings of one gate rest on %q and %q, and the gate on %q; want each opening on LEGION-100 and its own ancestor, the gate as it was",
			first.RestsOn, second.RestsOn, roomy.RestsOn)
	}

	closed := CloseBasis(opened)
	if closed.Basis != nil || closed.KeptPerson != nil || closed.ApprovedVersion == nil || *closed.ApprovedVersion != 2 ||
		closed.ApprovedMark != personal || GateOpen(closed) {
		t.Fatalf("the step whose basis closed = %#v, want it closed on the person's own approval of version 2", closed)
	}
	if !slices.Equal(closed.RestsOn, []string{"LEGION-100"}) {
		t.Fatalf("the step whose basis closed rests on %q, want LEGION-100 until the generation ends", closed.RestsOn)
	}
	never := CloseBasis(OpenOnBasis(DesignGate{Issue: "LEGION-209", ArtifactID: "artifact-209", LatestVersion: 1}, basisOf("LEGION-100", 3, registered)))
	if never.ApprovedVersion != nil || never.ApprovedMark != (EventMark{}) {
		t.Fatalf("a step with no person's approval falls back to %#v, want none, never the version the basis opened", never)
	}

	// A second basis on another ancestor keeps the approval the first kept, and the step rests on
	// both ancestors.
	again := OpenOnBasis(opened, basisOf("LEGION-101", 5, registered.Add(time.Minute)))
	if again.KeptPerson == nil || again.KeptPerson.Version != 2 || !slices.Equal(again.RestsOn, []string{"LEGION-100", "LEGION-101"}) ||
		!again.RestsOnIssue("LEGION-101") || !again.RestsOnIssue("LEGION-100") {
		t.Fatalf("the step opened again on another ancestor = %#v, want the kept approval of version 2 and both rests", again)
	}

	// A person's approval at the latest version outranks any basis.
	personOpen := DesignGate{Issue: "LEGION-210", ArtifactID: "artifact-210", LatestVersion: 4, ApprovedVersion: new(4), ApprovedMark: personal}
	if got := OpenOnBasis(personOpen, basisOf("LEGION-100", 3, registered)); !reflect.DeepEqual(got, personOpen) {
		t.Fatalf("a basis over a step a person opened = %#v, want the step left on the person's approval", got)
	}
	if got := PendOnBasis(personOpen, basisOf("LEGION-100", 3, registered), "ask-1"); !reflect.DeepEqual(got, personOpen) {
		t.Fatalf("a pending basis over a step a person opened = %#v, want the step left on the person's approval", got)
	}

	// A pending step stays closed on the person's approval it holds, rests on its ancestor while
	// it waits, and leaves no rest when the wait ends without opening.
	pending := PendOnBasis(earlier, basisOf("LEGION-102", 3, registered), "ask-1")
	if !pending.PendingOnBasis() || pending.OnBasis() || GateOpen(pending) || pending.Basis.Pending.Ask != "ask-1" ||
		pending.Basis.Approval != (EventMark{}) || pending.Basis.Approver != "" || pending.KeptPerson != nil {
		t.Fatalf("the pending step = %#v, want it closed, waiting on ask-1 with no approval of the ancestor's", pending)
	}
	if !pending.RestsOnIssue("LEGION-102") || pending.RestsOn != nil {
		t.Fatalf("the pending step rests on LEGION-102 %t with RestsOn %q, want it resting there through its basis alone", pending.RestsOnIssue("LEGION-102"), pending.RestsOn)
	}
	if approval := pending.ApprovedByPerson(); approval == nil || approval.Version != 2 {
		t.Fatalf("the person's approval read off the pending step = %#v, want version 2", approval)
	}
	ended := EndPendingBasis(pending)
	if ended.Basis != nil || ended.RestsOnIssue("LEGION-102") || ended.ApprovedVersion == nil || *ended.ApprovedVersion != 2 {
		t.Fatalf("the step whose wait ended = %#v, want it registered with no basis, resting on nothing, on the person's approval of version 2", ended)
	}

	approvedAt := EventMark{Seq: 44, At: registered.Add(time.Hour)}
	answered := OpenPendingBasis(pending, approvedAt, "user:bob")
	if !answered.OnBasis() || !GateOpen(answered) || answered.Basis.Approval != approvedAt || answered.Basis.Approver != "user:bob" ||
		!answered.Basis.RegisteredAt.Equal(registered) || answered.KeptPerson == nil || answered.KeptPerson.Version != 2 ||
		!slices.Equal(answered.RestsOn, []string{"LEGION-102"}) {
		t.Fatalf("the pending step a person's approval of the ancestor's version answered = %#v, want it open on that approval at the registration's moment, keeping the person's own and resting on LEGION-102", answered)
	}
	if got := OpenPendingBasis(earlier, approvedAt, "user:bob"); !reflect.DeepEqual(got, earlier) {
		t.Fatalf("an approval over a step that waits on nothing = %#v, want it left as it is", got)
	}
}

// The gate carries its basis, the person's approval it keeps apart and the ancestors it rests on
// through the store, and the three reads find a step by its basis document, by the request it
// waits on and by an ancestor it rests on, across trees. rests_on only grows: a write that carries
// none takes nothing away. A basis is written whole or refused, and a row a release before the
// basis moved reads as that move's end of the basis.
func TestTheGateRoundTripsItsBasis(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	registered := time.Date(2026, 11, 14, 6, 0, 0, 123456000, time.UTC)
	personal := EventMark{Seq: 9, At: registered.Add(-48 * time.Hour)}
	issue := func(key string) Issue {
		issue := issueFixture(key)
		issue.Tree, issue.Phase, issue.Status = key, phase.Admitted, "in_progress"
		return issue
	}
	open := OpenOnBasis(DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
		ApprovedVersion: new(2), ApprovedMark: personal}, basisOf("LEGION-100", 3, registered))
	pending := PendOnBasis(DesignGate{Issue: "LEGION-209", Step: DesignStepSpec, ArtifactID: "artifact-209", LatestVersion: 1},
		basisOf("LEGION-100", 3, registered), "ask-1")
	unserved := basisOf("LEGION-101", 2, registered)
	unserved.Serves = nil
	elsewhere := PendOnBasis(DesignGate{Issue: "LEGION-210", Step: DesignStepSpec, ArtifactID: "artifact-210", LatestVersion: 2},
		unserved, "ask-2")
	plain := DesignGate{Issue: "LEGION-211", Step: DesignStepSpec, ArtifactID: "artifact-211", LatestVersion: 1}

	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211"} {
			must(t, records.PutIssue(ctx, tx, issue(key)))
		}
		for _, gate := range []DesignGate{open, pending, elsewhere, plain} {
			must(t, records.PutGate(ctx, tx, gate))
		}
	})
	// Each row reads back with its basis counted once, the one a new row carries.
	openBack, pendingBack, elsewhereBack := open, pending, elsewhere
	openBack.EverOpened = true
	openBack.BasisMoves, pendingBack.BasisMoves, elsewhereBack.BasisMoves = 1, 1, 1
	read := func(tx pgx.Tx, key string) DesignGate {
		t.Helper()
		gates, err := records.GatesOf(ctx, tx, key)
		must(t, err)
		return *SpecGate(gates)
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, want := range []DesignGate{openBack, pendingBack, elsewhereBack, plain} {
			if got := read(tx, want.Issue); !reflect.DeepEqual(got, want) {
				t.Fatalf("the gate of %s reads back as %#v, want %#v", want.Issue, got, want)
			}
		}
		byDocument, err := records.GatesForBasisArtifact(ctx, tx, "artifact-LEGION-100")
		must(t, err)
		if !reflect.DeepEqual(byDocument, []DesignGate{openBack, pendingBack}) {
			t.Fatalf("the gates on LEGION-100's document = %#v, want the open and the pending step", byDocument)
		}
		byAsk, err := records.GatesByPendingAsk(ctx, tx, "ask-2")
		must(t, err)
		if !reflect.DeepEqual(byAsk, []DesignGate{elsewhereBack}) {
			t.Fatalf("the gates pending on ask-2 = %#v, want LEGION-210's alone", byAsk)
		}
		resting, err := records.GatesRestingOn(ctx, tx, "LEGION-100")
		must(t, err)
		if !reflect.DeepEqual(resting, []DesignGate{openBack, pendingBack}) {
			t.Fatalf("the gates resting on LEGION-100 = %#v, want the open and the pending step", resting)
		}
		none, err := records.GatesByPendingAsk(ctx, tx, "ask-9")
		must(t, err)
		if none != nil {
			t.Fatalf("the gates pending on a request none waits on = %#v, want none", none)
		}
	})

	// The open step's basis closes and the pending step's wait ends: the one still rests on
	// LEGION-100, the other no longer does, and neither is found by the ancestor's document.
	inTx(t, st, func(tx pgx.Tx) {
		closed := CloseBasis(read(tx, "LEGION-208"))
		closed.LatestVersion = 5
		must(t, records.PutGate(ctx, tx, closed))
		must(t, records.PutGate(ctx, tx, EndPendingBasis(read(tx, "LEGION-209"))))
		// A write that carries no rest, as a stale copy would, keeps it.
		carriesNone := read(tx, "LEGION-208")
		carriesNone.RestsOn = nil
		must(t, records.PutGate(ctx, tx, carriesNone))
		got := read(tx, "LEGION-208")
		if got.Basis != nil || got.KeptPerson != nil || got.ApprovedVersion == nil || *got.ApprovedVersion != 2 ||
			got.ApprovedMark != personal || !slices.Equal(got.RestsOn, []string{"LEGION-100"}) {
			t.Fatalf("the step whose basis closed reads back as %#v, want it on the person's approval of version 2, resting on LEGION-100", got)
		}
		byDocument, err := records.GatesForBasisArtifact(ctx, tx, "artifact-LEGION-100")
		must(t, err)
		if byDocument != nil {
			t.Fatalf("the gates on LEGION-100's document after both ended = %#v, want none", byDocument)
		}
		resting, err := records.GatesRestingOn(ctx, tx, "LEGION-100")
		must(t, err)
		if len(resting) != 1 || resting[0].Issue != "LEGION-208" {
			t.Fatalf("the gates resting on LEGION-100 after both ended = %#v, want LEGION-208's alone", resting)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		for _, tc := range []struct {
			name   string
			change func(*GateBasis)
			want   string
		}{
			{"no ancestor", func(b *GateBasis) { b.Issue = "" }, "a basis names its ancestor, the ancestor's document and a version of it"},
			{"no document", func(b *GateBasis) { b.ArtifactID = "" }, "a basis names its ancestor, the ancestor's document and a version of it"},
			{"no version", func(b *GateBasis) { b.Version = 0 }, "a basis names its ancestor, the ancestor's document and a version of it"},
			{"no registration time", func(b *GateBasis) { b.RegisteredAt = time.Time{} }, "a basis names the daemon's own time of its registration"},
			{"a pending basis naming no request", func(b *GateBasis) { b.Pending = &PendingBasis{} }, "a pending basis names the approval request it waits on"},
		} {
			gate := read(tx, "LEGION-211")
			basis := basisOf("LEGION-100", 3, registered)
			tc.change(&basis)
			gate.Basis = &basis
			if err := records.PutGate(ctx, tx, gate); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PutGate of a basis with %s = %v, want %q", tc.name, err, tc.want)
			}
		}
	})
}

// A step rests on every ancestor a basis of its generation opened it on (R8), not on the first
// alone: a root that opened its step on a far ancestor's approval and then registered on a nearer
// ancestor's newly approved document rests on both once the step closes. The transitions keep each
// rest, and so does the store, whose writes come from a registration that builds the step afresh,
// carrying only the ancestor it opens the step on; the read of the gates resting on either finds
// the step.
func TestAStepRestsOnEachAncestorABasisOpenedItOn(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	fresh := DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4}
	far := OpenOnBasis(fresh, basisOf("LEGION-100", 3, registered))

	if closed := CloseBasis(OpenOnBasis(far, basisOf("LEGION-150", 2, registered.Add(time.Hour)))); !closed.RestsOnIssue("LEGION-100") || !closed.RestsOnIssue("LEGION-150") {
		t.Fatalf("the step opened on LEGION-100 and then on LEGION-150, closed since = %#v, want it resting on both", closed)
	}

	issue := issueFixture("LEGION-208")
	issue.Tree, issue.Phase, issue.Status = "LEGION-208", phase.Admitted, "in_progress"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutGate(ctx, tx, far))
		must(t, records.PutGate(ctx, tx, OpenOnBasis(fresh, basisOf("LEGION-150", 2, registered.Add(time.Hour)))))
		gates, err := records.GatesOf(ctx, tx, "LEGION-208")
		must(t, err)
		closed := CloseBasis(*SpecGate(gates))
		closed.LatestVersion = 5
		must(t, records.PutGate(ctx, tx, closed))
	})
	inTx(t, st, func(tx pgx.Tx) {
		gates, err := records.GatesOf(ctx, tx, "LEGION-208")
		must(t, err)
		if got := SpecGate(gates); got.Basis != nil || !got.RestsOnIssue("LEGION-100") || !got.RestsOnIssue("LEGION-150") {
			t.Fatalf("the closed step reads back as %#v, want it with no basis, resting on LEGION-100 and LEGION-150", got)
		}
		for _, ancestor := range []string{"LEGION-100", "LEGION-150"} {
			resting, err := records.GatesRestingOn(ctx, tx, ancestor)
			must(t, err)
			if len(resting) != 1 || resting[0].Issue != "LEGION-208" {
				t.Fatalf("the gates resting on %s = %#v, want LEGION-208's", ancestor, resting)
			}
		}
	})
}

// The row counts each write that moves its basis and no other (DesignGate.BasisMoves): a new
// row with no basis starts at zero; a pending basis set on it, the person's approval answering its
// request, another ancestor's basis registered over it and its end each add one. A write of the
// same basis again, with another reason and registration time, adds nothing, and neither does a
// write of no basis over a row that holds none.
func TestTheGateCountsEachMoveOfItsBasis(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	fresh := DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4}
	asked := basisOf("LEGION-100", 4, registered)
	askedAgain := basisOf("LEGION-100", 4, registered.Add(time.Hour))
	askedAgain.Reason = "the root's checks serve r1 and r2 of the ancestor"
	answer := EventMark{Seq: 44, At: registered.Add(2 * time.Hour)}
	issue := issueFixture("LEGION-208")
	issue.Tree, issue.Phase, issue.Status = "LEGION-208", phase.Admitted, "in_progress"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		for _, write := range []struct {
			what string
			gate DesignGate
			want int
		}{
			{"a row with no basis", fresh, 0},
			{"no basis again", fresh, 0},
			{"a pending basis", PendOnBasis(fresh, asked, "ask-1"), 1},
			{"the same pending basis again, with another reason", PendOnBasis(fresh, askedAgain, "ask-1"), 1},
			{"the person's approval answering its request", OpenPendingBasis(PendOnBasis(fresh, asked, "ask-1"), answer, "user:bob"), 2},
			{"the same open basis again", OpenPendingBasis(PendOnBasis(fresh, askedAgain, "ask-1"), answer, "user:bob"), 2},
			{"another ancestor's basis over it", OpenOnBasis(fresh, basisOf("LEGION-150", 2, registered)), 3},
			{"its end", fresh, 4},
			{"no basis again after the end", fresh, 4},
		} {
			must(t, records.PutGate(ctx, tx, write.gate))
			gates, err := records.GatesOf(ctx, tx, "LEGION-208")
			must(t, err)
			if got := SpecGate(gates).BasisMoves; got != write.want {
				t.Fatalf("after %s the row counts %d moves of its basis, want %d", write.what, got, write.want)
			}
		}
	})
}

// A release before the basis names none of its columns, so a move it makes of a step a basis
// opened leaves the basis behind. This release reads such a row as that move's end of the basis,
// as its own rule would have made it: a newer version falls back to the person's kept approval,
// never to the version the basis opened; a changes request clears the approval; a person's
// approval stands as the person's. A pending step a person's approval opened has ended its wait.
// The earlier release counts nothing in basis_moves, so the row reads back with the one basis
// its first write carried.
func TestARowAnEarlierReleaseMovedReadsItsBasisEnded(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	personal := EventMark{Seq: 9, At: registered.Add(-48 * time.Hour)}
	key := "LEGION-208"
	open := OpenOnBasis(DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
		ApprovedVersion: new(2), ApprovedMark: personal}, basisOf("LEGION-100", 3, registered))
	for _, tc := range []struct {
		name  string
		moved string
		want  DesignGate
	}{
		{"a newer version", `update design_gates set latest_version = 5 where issue = $1`,
			DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 5,
				ApprovedVersion: new(2), ApprovedMark: personal, EverOpened: true, RestsOn: []string{"LEGION-100"}, BasisMoves: 1}},
		{"a changes request", `update design_gates set approved_version = null where issue = $1`,
			DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4, EverOpened: true, RestsOn: []string{"LEGION-100"},
				BasisMoves: 1}},
		{"a person's approval", `update design_gates set approved_version = 4, approved_seq = 50 where issue = $1`,
			DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
				ApprovedVersion: new(4), ApprovedMark: EventMark{Seq: 50}, EverOpened: true, RestsOn: []string{"LEGION-100"}, BasisMoves: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, st, func(tx pgx.Tx) {
				issue := issueFixture(key)
				issue.Phase = phase.Admitted
				must(t, records.PutIssue(ctx, tx, issue))
				must(t, records.PutGate(ctx, tx, open))
				if _, err := tx.Exec(ctx, tc.moved, key); err != nil {
					t.Fatalf("move the step as an earlier release does: %v", err)
				}
				gates, err := records.GatesOf(ctx, tx, key)
				must(t, err)
				if got := SpecGate(gates); !reflect.DeepEqual(got, &tc.want) {
					t.Fatalf("the moved step reads back as %#v, want %#v", got, tc.want)
				}
				if _, err := tx.Exec(ctx, `delete from design_gates where issue = $1`, key); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
	t.Run("a person's approval of a pending step", func(t *testing.T) {
		inTx(t, st, func(tx pgx.Tx) {
			issue := issueFixture(key)
			issue.Phase = phase.Admitted
			must(t, records.PutIssue(ctx, tx, issue))
			must(t, records.PutGate(ctx, tx, PendOnBasis(DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4},
				basisOf("LEGION-100", 3, registered), "ask-1")))
			if _, err := tx.Exec(ctx, `update design_gates set approved_version = 4, approved_seq = 50 where issue = $1`, key); err != nil {
				t.Fatalf("approve the step as an earlier release does: %v", err)
			}
			gates, err := records.GatesOf(ctx, tx, key)
			must(t, err)
			want := DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
				ApprovedVersion: new(4), ApprovedMark: EventMark{Seq: 50}, BasisMoves: 1}
			if got := SpecGate(gates); !reflect.DeepEqual(got, &want) {
				t.Fatalf("the pending step a person's approval opened reads back as %#v, want %#v", got, want)
			}
			// Its basis_issue still names LEGION-100, which the store's condition selects on, but
			// the step as read rests on nothing.
			resting, err := records.GatesRestingOn(ctx, tx, "LEGION-100")
			must(t, err)
			if len(resting) != 0 {
				t.Fatalf("the gates resting on LEGION-100 = %#v, want none: the step's wait ended", resting)
			}
			// Its basis_artifact_id still names LEGION-100's document, but the step as read is no
			// step on that document, standing or pending.
			byDocument, err := records.GatesForBasisArtifact(ctx, tx, "artifact-LEGION-100")
			must(t, err)
			if len(byDocument) != 0 {
				t.Fatalf("the gates on LEGION-100's document = %#v, want none: the step's wait ended", byDocument)
			}
		})
	})
}

// The ancestor an issue's spec is a view on is the record's: PutIssue stores it and every read
// behind issueColumns gives it back, and a write that carries none, as a registration that found
// no ancestor to be a view on makes, takes it away.
func TestStoreRoundTripsTheAncestorAnIssueIsAViewOn(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	view := issueFixture("LEGION-217")
	view.Phase, view.ViewOf = phase.Admitted, "LEGION-100"

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, view))
		got, err := records.Issue(ctx, tx, view.Key)
		must(t, err)
		if !reflect.DeepEqual(written(t, got), &view) {
			t.Fatalf("the view's issue = %#v, want %#v", got, view)
		}
		issues, err := records.TreeIssues(ctx, tx, view.Key)
		must(t, err)
		if !reflect.DeepEqual(writtenAll(t, issues), []Issue{view}) {
			t.Fatalf("tree issues = %#v, want %#v", issues, []Issue{view})
		}
	})
	none := view
	none.ViewOf = ""
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, none))
		got, err := records.Issue(ctx, tx, none.Key)
		must(t, err)
		if !reflect.DeepEqual(written(t, got), &none) {
			t.Fatalf("the issue after a write that names no view = %#v, want %#v", got, none)
		}
	})
}
