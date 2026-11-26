package admit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// slotsReadingSaid is a slots reading as its JSON reads, which is how the tick notice carries it
// and how a failure prints it.
func slotsReadingSaid(t *testing.T, reading record.SlotsReading) string {
	t.Helper()
	encoded, err := json.Marshal(reading)
	if err != nil {
		t.Fatalf("marshal the slots reading: %v", err)
	}
	return string(encoded)
}

// The controller's tick notice carries the project's slots reading (spec R6): a tick applied
// through intake.ApplyFact queues a tick notice whose `slots` counts the four holders by what each
// is doing — one at work, one held, one waiting on a person's answer to an ask, one that took its
// slot and has had no turn yet — under a full cap, names the queued root that waits for a slot
// and the three holders ranked below it, and carries the line that one function writes of it.
// The notice is read back through the outbox payload's validator, as the executor reads it before
// it publishes, and the reading is compared with the state's own over the same records: one
// reading read two ways, so neither answer is copied from the other.
func TestTickNoticeCarriesTheSlotsLine(t *testing.T) {
	const slotCap = 4
	pool := migratedPool(t)
	steps := config.DesignGate("").Steps()
	admission := newAdmissionForProject(t, acmeProject, slotCap, steps, silent())
	registerControllerFor(t, pool, acmeProject)

	// A holder at implementing that took its slot at fixedNow and has had no turn: idle since
	// its slot's admittedAt.
	seedSlottedInPhase(t, pool, acmeProject, "ACME-1", "a", phase.Implementing, nil)
	approvedGate(t, pool, "ACME-1")
	// A holder its architect held from implementing: stopped, at a time the record does not hold.
	seedSlottedInPhase(t, pool, acmeProject, "ACME-2", "c", phase.Held, func(issue *record.Issue) {
		issue.Hold = &record.Hold{From: phase.Implementing}
	})
	approvedGate(t, pool, "ACME-2")
	// A root waiting on a person's answer to an ask since two hours before fixedNow. Its design
	// gate is not open, so the pass's reconciliation answers nothing of its wait.
	personSince := fixedNow.Add(-2 * time.Hour)
	seedSlottedInPhase(t, pool, acmeProject, "ACME-3", "d", phase.AwaitingPerson, func(issue *record.Issue) {
		issue.Awaiting = &record.Awaiting{Ask: "ask-3", Since: record.EventMark{Seq: 5, At: personSince}}
	})
	// A holder whose implementer has been in a turn for half an hour.
	seedSlottedInPhase(t, pool, acmeProject, "ACME-5", "e", phase.Implementing, nil)
	approvedGate(t, pool, "ACME-5")
	turnStart := fixedNow.Add(-30 * time.Minute)
	records := claimStore(t, pool)
	if err := records.PutClaim(context.Background(), supervise.Claim{Token: acmeToken(t, "ACME-5", claim.RoleImplementer),
		Project: acmeProjectToken(t), Tree: "ACME-5", Issue: "ACME-5", Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1,
		State: supervise.StateWorking, Session: "ses_implementer_5", LastTurnStart: turnStart}); err != nil {
		t.Fatalf("record the implementer's claim of ACME-5: %v", err)
	}
	// A root waiting for its tree to be opened, ranked below ACME-1 and above the other three
	// holders, while the cap is full.
	seedWaitingForProject(t, pool, acmeProject, "ACME-4", "b")

	tick(t, pool, admission, "tick-slots")

	notice, payload := tickNoticeOf(t, pool, acmeProject)
	if notice.Slots == nil {
		t.Fatalf("the tick notice carries no slots reading: %s", payload)
	}
	want := record.SlotsReading{
		InUse: 4, Cap: slotCap,
		Working:           record.StateCount{Count: 1, Oldest: &turnStart},
		Stopped:           record.StateCount{Count: 1},
		Person:            record.StateCount{Count: 1, Oldest: &personSince},
		Idle:              record.StateCount{Count: 1, Oldest: &fixedNow},
		BelowFirstWaiting: record.BelowFirstWaiting{Count: 3, First: "ACME-4"},
	}
	want.Line = record.SlotsLine(want)
	if got, wanted := slotsReadingSaid(t, *notice.Slots), slotsReadingSaid(t, want); got != wanted {
		t.Fatalf("the tick notice's slots = %s, want %s", got, wanted)
	}
	if !strings.HasPrefix(notice.Slots.Line, "slots: 4 of 4 in use: 1 working") ||
		!strings.Contains(notice.Slots.Line, "ACME-4, which waits for a slot.") {
		t.Fatalf("the tick notice's line is %q, want the slots line naming 4 of 4 in use and ACME-4 waiting", notice.Slots.Line)
	}
	// The stored row is jsonb, which Postgres prints with its own spacing, so the payload is read as
	// JSON rather than matched as text: it carries a slots object, and that object carries the line.
	var stored struct {
		Slots *struct {
			Line *string `json:"line"`
		} `json:"slots"`
	}
	if err := json.Unmarshal([]byte(payload), &stored); err != nil || stored.Slots == nil || stored.Slots.Line == nil {
		t.Fatalf("the tick notice's payload carries no slots object with its line (%v): %s", err, payload)
	}

	// The state's own reading over the same records, read through internal/projection.
	var stated *record.SlotsReading
	inTx(t, pool, func(tx pgx.Tx) {
		claims, err := records.ClaimsTx(context.Background(), tx)
		if err != nil {
			t.Fatalf("read the claims: %v", err)
		}
		state, err := projection.Project(context.Background(), projection.Inputs{
			Tx: tx, Store: record.NewStore(), Project: acmeProject, Steps: steps, Claims: claims, Now: fixedNow, Cap: slotCap,
		})
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		stated = state.Admission.Slots
	})
	if stated == nil {
		t.Fatalf("the state over the same records carries no admission.slots, while the tick notice carries %s", payload)
	}
	if got, ticked := slotsReadingSaid(t, *stated), slotsReadingSaid(t, *notice.Slots); got != ticked {
		t.Fatalf("the state's admission.slots = %s and the tick notice's slots = %s: one reading of one project reads the same wherever it is read", got, ticked)
	}
}
