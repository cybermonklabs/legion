package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The work hold holds a tree's work while the tree's standing design gate stands closed: the first
// required step, in the configured order, whose row is registered and not approved at its latest
// version (the work hold's spec, "Under `work`"). It is the one predicate the message, the
// merger's start and the hold read (TreeGate.Standing), each under `work` alone, so it is held
// here for every step a deployment can gate, and for the shapes the standing gate has always
// passed over: a step nobody has registered, and a deployment that gates no step.
func TestWorkHoldPredicateHoldsWhileAGatedStepStandsClosed(t *testing.T) {
	row := func(step DesignStep, latest int, approvedAt *int) DesignGate {
		return DesignGate{Issue: "LEGION-208", Step: step, ArtifactID: "artifact-" + string(step), LatestVersion: latest, ApprovedVersion: approvedAt}
	}
	for _, step := range DesignSteps {
		t.Run(string(step), func(t *testing.T) {
			steps := []DesignStep{step}
			closed := row(step, 4, new(3))
			for _, tc := range []struct {
				name  string
				rows  map[DesignStep]DesignGate
				holds bool
			}{
				{name: "the step stands open", rows: map[DesignStep]DesignGate{step: row(step, 3, new(3))}},
				{name: "a new version stands with its reviewer", rows: map[DesignStep]DesignGate{step: closed}, holds: true},
				{name: "a version nobody approved yet", rows: map[DesignStep]DesignGate{step: row(step, 1, nil)}, holds: true},
				// A step nobody has registered names no document, so the standing gate passes it
				// over (classify.StandingGate), and so does the hold: there is nobody to ask.
				{name: "the step is registered by nothing", rows: nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					gate := TreeGate{Steps: steps, Rows: tc.rows}
					held, holds := gate.Standing()
					if holds != tc.holds {
						t.Fatalf("Standing() = %+v, %v; want %v", held, holds, tc.holds)
					}
					if tc.holds && !reflect.DeepEqual(held, tc.rows[step]) {
						t.Fatalf("the hold names %+v, want the closed row %+v, whose version the texts name", held, tc.rows[step])
					}
					if !tc.holds && !reflect.DeepEqual(held, DesignGate{}) {
						t.Fatalf("an open gate names %+v, want nothing", held)
					}
				})
			}
		})
	}

	t.Run("the first closed step of several, in the configured order", func(t *testing.T) {
		criteria, spec := row(DesignStepCriteria, 2, new(1)), row(DesignStepSpec, 4, new(3))
		gate := TreeGate{Steps: []DesignStep{DesignStepCriteria, DesignStepSpec}, Rows: map[DesignStep]DesignGate{
			DesignStepCriteria: criteria, DesignStepSpec: spec}}
		if held, holds := gate.Standing(); !holds || !reflect.DeepEqual(held, criteria) {
			t.Fatalf("Standing() = %+v, %v; want the criteria row, the first closed step", held, holds)
		}
		gate.Rows[DesignStepCriteria] = row(DesignStepCriteria, 2, new(2))
		if held, holds := gate.Standing(); !holds || !reflect.DeepEqual(held, spec) {
			t.Fatalf("Standing() with the criteria open = %+v, %v; want the spec row", held, holds)
		}
		gate.Rows[DesignStepSpec] = row(DesignStepSpec, 4, new(4))
		if held, holds := gate.Standing(); holds {
			t.Fatalf("Standing() with every step open = %+v, want nothing", held)
		}
	})

	// A criteria row the root's architect registered beside a deployment that gates spec and not
	// criteria joins the steps the tree is gated on (TreeSteps), so a new version of that
	// Verification document holds the work as a new spec version does, and the hold names its row.
	t.Run("a criteria row registered beside a deployment of spec alone", func(t *testing.T) {
		criteria := row(DesignStepCriteria, 2, new(1))
		gate := TreeGate{Steps: []DesignStep{DesignStepSpec}, Rows: map[DesignStep]DesignGate{
			DesignStepCriteria: criteria, DesignStepSpec: row(DesignStepSpec, 4, new(4))}}
		if held, holds := gate.Standing(); !holds || !reflect.DeepEqual(held, criteria) {
			t.Fatalf("Standing() = %+v, %v; want the registered criteria row, which the tree is gated on", held, holds)
		}
		gate.Rows[DesignStepCriteria] = row(DesignStepCriteria, 2, new(2))
		if held, holds := gate.Standing(); holds {
			t.Fatalf("Standing() with the criteria approved = %+v, want nothing", held)
		}
	})

	t.Run("a deployment that gates no step holds nothing", func(t *testing.T) {
		gate := TreeGate{Steps: []DesignStep{}, Rows: map[DesignStep]DesignGate{DesignStepSpec: row(DesignStepSpec, 4, new(3))}}
		if held, holds := gate.Standing(); holds {
			t.Fatalf("Standing() under `gates.design: off` = %+v, want nothing", held)
		}
	})
}

// A row a release before the work hold wrote carries none of the hold's keys, and is read as that
// release meant it: a suspend that names no source is the daemon's own stop, a start carries no
// opening sentences, and a role message is neither a wake nor a stop. Each row this release writes
// without the hold re-encodes to the very bytes the release before wrote, so a rollback reads it
// too. The hold's own rows round-trip, a close message's stop builds the hold's stop row with the
// deadline and the turn it is given, and a key of the hold on a row it does not belong to, a
// message's stop of no role of a tree, or a wake or a stop on a message that is no close message,
// is refused at the enqueue (validateOutboxPayload), as every other key is.
func TestWorkHoldOldSuspendRowsDecode(t *testing.T) {
	deadline := time.Date(2026, 11, 20, 23, 2, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		row  OutboxRow
		want OutboxPayload
	}{
		{
			name: "a suspend of the release before",
			row: OutboxRow{ID: 7, Kind: OutboxKindSupervise, Issue: "LEGION-209",
				Payload: []byte(`{"op":"suspend","tree":"LEGION-208","role":"implementer","generation":4,"reason":"the tree closed"}`)},
			want: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 4, Reason: "the tree closed"},
		},
		{
			name: "a start of the release before",
			row: OutboxRow{ID: 8, Kind: OutboxKindSupervise, Issue: "LEGION-209",
				Payload: []byte(`{"op":"start","tree":"LEGION-208","role":"implementer","task":"Implement the plan.","generation":4,"phase":"implementing"}`)},
			want: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Task: "Implement the plan.", Generation: 4, Phase: "implementing"},
		},
		{
			name: "a role message of the release before",
			row: OutboxRow{ID: 9, Kind: OutboxKindRoleMessage, Issue: "LEGION-209",
				Payload: []byte(`{"role":"legion-LEGION-209-implementer","body":"The design gate of LEGION-208 closed."}`)},
			want: RoleMessage{Role: "legion-LEGION-209-implementer", Body: "The design gate of LEGION-208 closed."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := DecodeOutboxPayload(tc.row)
			if err != nil {
				t.Fatalf("decode the row the release before wrote: %v", err)
			}
			if !reflect.DeepEqual(decoded, tc.want) {
				t.Fatalf("the row decodes to %#v, want %#v: no source, deadline, turn, opening, wake or stop", decoded, tc.want)
			}
			written, err := NewOutboxRow(tc.row.Issue, decoded, time.Time{})
			if err != nil {
				t.Fatalf("write the decoded row again: %v", err)
			}
			if string(written.Payload) != string(tc.row.Payload) {
				t.Fatalf("the row writes %s again, want the bytes the release before wrote, %s: a rollback decodes strictly", written.Payload, tc.row.Payload)
			}
		})
	}

	stop := SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 4,
		SuspendedBy: supervise.SuspendedByHold, Reason: "the work hold"}
	timed := stop
	timed.Deadline, timed.TurnAfter = deadline, deadline.Add(-2*time.Minute)
	versioned := stop
	versioned.HoldVersion = 4
	opening := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 4,
		Task: "The work hold stopped your turn.\n\nThe issue's other roles may ask you questions.", Opening: "The work hold stopped your turn."}
	messageStop := HoldStop{Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 4, Step: DesignStepSpec, Reason: "the work hold"}
	versionedStop := messageStop
	versionedStop.HoldVersion = 4
	t.Run("the hold's rows round-trip", func(t *testing.T) {
		for _, payload := range []OutboxPayload{
			stop, timed, versioned, opening,
			RoleMessage{Role: "legion-LEGION-209-implementer", Body: "End your turn now.", Close: true, Stop: &messageStop},
			RoleMessage{Role: "legion-LEGION-209-implementer", Body: "End your turn now.", Close: true, Stop: &versionedStop},
			RoleMessage{Role: "legion-LEGION-210-merger", Body: "End your turn now.", Close: true, Wake: true, Stop: &HoldStop{
				Tree: "LEGION-208", Role: claim.RoleMerger, Generation: 4, Step: DesignStepCriteria, Reason: "the work hold"}},
			RoleMessage{Role: "legion-LEGION-210-merger", Body: "Your phase goes on.", Close: true, Wake: true},
		} {
			row, err := NewOutboxRow("LEGION-209", payload, time.Time{})
			if err != nil {
				t.Fatalf("enqueue %#v: %v", payload, err)
			}
			decoded, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode %s: %v", row.Payload, err)
			}
			if !reflect.DeepEqual(decoded, payload) {
				t.Fatalf("%s decodes to %#v, want %#v", row.Payload, decoded, payload)
			}
		}
	})

	t.Run("a close message's stop builds the hold's stop row", func(t *testing.T) {
		timedStop := versioned
		timedStop.Deadline = deadline
		for _, tc := range []struct {
			stop                HoldStop
			deadline, turnAfter time.Time
			want                SuperviseRequest
		}{
			{stop: messageStop, want: stop},
			{stop: versionedStop, want: versioned},
			{stop: versionedStop, deadline: deadline, want: timedStop},
			{stop: messageStop, deadline: timed.Deadline, turnAfter: timed.TurnAfter, want: timed},
		} {
			if got := tc.stop.Suspend(tc.deadline, tc.turnAfter); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the stop %+v with the deadline %s and the turn %s builds %#v, want the hold's stop row %#v",
					tc.stop, tc.deadline, tc.turnAfter, got, tc.want)
			}
		}
	})

	t.Run("a key of the hold on a row it does not belong to is refused", func(t *testing.T) {
		withSource := func(by supervise.SuspendSource) SuperviseRequest {
			request := stop
			request.SuspendedBy = by
			return request
		}
		started := opening
		started.SuspendedBy = supervise.SuspendedByHold
		untimedTurn := stop
		untimedTurn.TurnAfter = deadline
		ungatedDeadline := stop
		ungatedDeadline.SuspendedBy, ungatedDeadline.Deadline = "", deadline
		openingOnStop := stop
		openingOnStop.Opening = "The work hold stopped your turn."
		openingWithoutItsTask := opening
		openingWithoutItsTask.Task = "The issue's other roles may ask you questions."
		versionOnAStart := opening
		versionOnAStart.HoldVersion = 4
		versionOnAnotherStop := stop
		versionOnAnotherStop.SuspendedBy, versionOnAnotherStop.HoldVersion = "", 4
		negativeVersion := versioned
		negativeVersion.HoldVersion = -4
		stopOf := func(stop HoldStop) RoleMessage {
			return RoleMessage{Role: "legion-LEGION-209-implementer", Body: "End your turn now.", Close: true, Stop: &stop}
		}
		wakeOfNoClose := RoleMessage{Role: "legion-LEGION-210-merger", Body: "The checks of your head settled.", Wake: true}
		stopOfNoClose := stopOf(versionedStop)
		stopOfNoClose.Close = false
		noTree, noRole, noStep, otherStep, negativeStop := versionedStop, versionedStop, versionedStop, versionedStop, versionedStop
		noTree.Tree = ""
		noRole.Role = claim.RoleController
		noStep.Step = ""
		otherStep.Step = "design"
		negativeStop.HoldVersion = -4
		for name, tc := range map[string]struct {
			payload OutboxPayload
			says    string
		}{
			"a source the hold does not write":              {withSource(supervise.SuspendedByDaemon), "hold"},
			"an operator's source":                          {withSource(supervise.SuspendedByOperator), "hold"},
			"a source on a start":                           {started, "suspend"},
			"a turn with no deadline":                       {untimedTurn, "deadline"},
			"a deadline on another stop":                    {ungatedDeadline, "hold"},
			"opening sentences on a stop":                   {openingOnStop, "opening"},
			"opening sentences the task lacks":              {openingWithoutItsTask, "opening"},
			"a version on a start":                          {versionOnAStart, "version"},
			"a version on another stop":                     {versionOnAnotherStop, "version"},
			"a negative version":                            {negativeVersion, "negative"},
			"a message whose stop names no tree":            {stopOf(noTree), "stop"},
			"a message whose stop names no role":            {stopOf(noRole), "stop"},
			"a message whose stop names no step":            {stopOf(noStep), "step"},
			"a message whose stop names no gated step":      {stopOf(otherStep), "step"},
			"a message whose stop names a negative version": {stopOf(negativeStop), "negative"},
			"a wake on a message that is no close":          {wakeOfNoClose, "close"},
			"a stop on a message that is no close":          {stopOfNoClose, "close"},
		} {
			if _, err := NewOutboxRow("LEGION-209", tc.payload, time.Time{}); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("%s: the enqueue answered %v, want a refusal naming %q", name, err, tc.says)
			}
		}
	})
}

// The work hold's record is one row per held tree: written at the close, read by the boot and the
// state, and deleted at the opening. A re-admission starts a new generation of the tree, and the
// hold of the generation that ended goes with the rest of it (ClearTreeGeneration), so it never
// holds the newer one.
func TestWorkHoldRecordGoesWithItsTreesGeneration(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root, other := issueFixture("LEGION-208"), issueFixture("LEGION-300")
	root.Tree, root.Parent, other.Tree, other.Parent = root.Key, nil, other.Key, nil
	closedAt := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	hold := WorkHold{Project: "LEGION", Tree: root.Key, RootGeneration: 4, Step: DesignStepSpec, ArtifactID: "artifact-208", Version: 4, ClosedAt: closedAt}
	held := WorkHold{Project: "LEGION", Tree: other.Key, RootGeneration: 1, Step: DesignStepCriteria, ArtifactID: "artifact-criteria", Version: 2, ClosedAt: closedAt}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, root))
		must(t, records.PutIssue(ctx, tx, other))
		must(t, records.PutWorkHold(ctx, tx, hold))
		must(t, records.PutWorkHold(ctx, tx, held))
	})
	inTx(t, st, func(tx pgx.Tx) {
		read, err := records.LockWorkHold(ctx, tx, root.Key)
		must(t, err)
		if read == nil || !reflect.DeepEqual(*read, hold) {
			t.Fatalf("the hold of %s reads %+v, want %+v", root.Key, read, hold)
		}
		all, err := records.WorkHolds(ctx, tx, "LEGION")
		must(t, err)
		if !reflect.DeepEqual(all, []WorkHold{hold, held}) {
			t.Fatalf("the project's holds read %+v, want both, by tree", all)
		}
		if none, err := records.WorkHolds(ctx, tx, "ACME"); err != nil || len(none) != 0 {
			t.Fatalf("another project's holds read %+v, %v; want none", none, err)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.ClearTreeGeneration(ctx, tx, root.Key, forgetNothing)
		must(t, err)
	})
	inTx(t, st, func(tx pgx.Tx) {
		if read, err := records.LockWorkHold(ctx, tx, root.Key); err != nil || read != nil {
			t.Fatalf("after the tree's new generation its hold reads %+v, %v; want none", read, err)
		}
		if read, err := records.LockWorkHold(ctx, tx, other.Key); err != nil || read == nil {
			t.Fatalf("another tree's hold reads %+v, %v; want it kept", read, err)
		}
		must(t, records.DeleteWorkHold(ctx, tx, other.Key))
		if read, err := records.LockWorkHold(ctx, tx, other.Key); err != nil || read != nil {
			t.Fatalf("the opened tree's hold reads %+v, %v; want it deleted", read, err)
		}
	})
}
