package legion

// export_test.go hands this package's external tests (package legion_test, admission_test.go) the
// parts of the intake, and of this package's own tests, that they drive. Those tests run the
// admission eval's operations that dispatch/evals builds (evals.AdmissionWriter), and dispatch/evals
// imports this package, so a test of package legion that imported it would be an import cycle.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rig's stores and run (rigStateID, otherStateID, rigRun), and the schema version of every
// state the tests' daemons serve (stateSchema).
const (
	RigStateID   = rigStateID
	OtherStateID = otherStateID
	RigRun       = rigRun
	StateSchema  = stateSchema
)

// IntakeRig is an intake rig (intakeRig) of an external test.
type IntakeRig struct {
	rig *intakeRig
}

// NewIntakeRig opens a test's database and NATS account (newIntakeRig). Its intakes run
// noAdmissions until the test gives it others (Admit).
func NewIntakeRig(t *testing.T) *IntakeRig {
	t.Helper()
	return &IntakeRig{rig: newIntakeRig(t)}
}

// Database is the rig's database.
func (r *IntakeRig) Database() *store.Store { return r.rig.database }

// Admit has every intake the rig starts from here on run admissions.
func (r *IntakeRig) Admit(admissions Admissions) { r.rig.admissions = admissions }

// Retry has every intake the rig starts from here on try a failed database step again after first,
// and after at most last (Intake.firstRetry, Intake.lastRetry).
func (r *IntakeRig) Retry(first, last time.Duration) { r.rig.firstRetry, r.rig.lastRetry = first, last }

// Start starts an intake and waits for its take-over (intakeRig.start).
func (r *IntakeRig) Start() {
	r.rig.t.Helper()
	r.rig.start()
}

// AwaitStartingRead waits for the good starting read of project, and moves the intake's clock past
// its minute (intakeRig.awaitStartingRead).
func (r *IntakeRig) AwaitStartingRead(project string) {
	r.rig.t.Helper()
	r.rig.awaitStartingRead(project)
}

// AwaitWholeReads waits for want whole reads of any project to be recorded.
func (r *IntakeRig) AwaitWholeReads(want int) {
	r.rig.t.Helper()
	r.rig.awaitWholeReads(want)
}

// Publish publishes events as one message of project's state lane under the rig's run and store,
// at the wall clock's moment, and answers that moment (intakeRig.publish).
func (r *IntakeRig) Publish(project string, events ...map[string]any) time.Time {
	r.rig.t.Helper()
	return r.rig.publish(project, events...)
}

// PublishIn publishes events as one message of project's state lane under run and the store
// stateID, at at (intakeRig.publishIn).
func (r *IntakeRig) PublishIn(project, run, stateID string, at time.Time, events ...map[string]any) {
	r.rig.t.Helper()
	r.rig.publishIn(project, run, stateID, at, false, events...)
}

// Settled waits for the intake to have acknowledged every message the stream holds.
func (r *IntakeRig) Settled() {
	r.rig.t.Helper()
	r.rig.settled()
}

// LogRecords is every record the rig's intakes logged with msg, in order.
func (r *IntakeRig) LogRecords(msg string) []map[string]any { return r.rig.logRecords(msg) }

// WaitFor polls done until it holds, and fails the test with failure's message when within passes.
func (r *IntakeRig) WaitFor(within time.Duration, done func() bool, failure func() string) {
	r.rig.t.Helper()
	r.rig.waitFor(within, done, failure)
}

// LastSeq is the sequence project's cursor stands at, nil for none.
func (r *IntakeRig) LastSeq(project string) *int64 {
	r.rig.t.Helper()
	return cursorOf(r.rig.t, r.rig.database, project).lastSeq
}

// Current is project's legion_current, "<issue>/<field>" to the value's text.
func (r *IntakeRig) Current(project string) map[string]string {
	r.rig.t.Helper()
	return currentValues(r.rig.t, r.rig.database, project)
}

// FakeDaemon is a Legion daemon stand-in (fakeDaemon), at its URL.
type FakeDaemon struct {
	*fakeDaemon
}

// NewFakeDaemon starts a daemon stand-in that serves body.
func NewFakeDaemon(t *testing.T, body string) *FakeDaemon {
	t.Helper()
	return &FakeDaemon{newFakeDaemon(t, body)}
}

// Serves has the daemon answer body from its next read on.
func (d *FakeDaemon) Serves(body string) { d.serves(body) }

// RefusesUnavailable has the daemon refuse every read from its next on with 503.
func (d *FakeDaemon) RefusesUnavailable() { d.refuses(http.StatusServiceUnavailable) }

// SeedLegionProject writes project and its Legion setting, which names daemonURL.
func SeedLegionProject(t *testing.T, database *store.Store, project, daemonURL string) {
	t.Helper()
	seedLegionProject(t, database, project, daemonURL)
}

// AdmissionEvent is an admission event at seq carrying block whole (admissionMoved).
func AdmissionEvent(seq int, block map[string]any) map[string]any { return admissionMoved(seq, block) }

// PhaseEvent is a change event at seq moving issue's phase to phase.
func PhaseEvent(seq int, issue, phase string) map[string]any {
	return change(seq, issue, moved(FieldPhase, phase))
}

// ApplyMessage makes one attempt at the state message payload, a message's payload as the daemon's
// state lane carries it, on an intake of pool that runs admissions, outside any durable: its batch
// (applyBatch), in the one transaction a delivery on the project's runner makes.
func ApplyMessage(ctx context.Context, pool *store.Pool, admissions Admissions, payload []byte) error {
	message, err := decodeStateMessage(&contracts.Envelope{Payload: string(payload)}, nil)
	if err != nil {
		return err
	}
	_, _, err = newIntake(pool, admissions, nil).applyBatch(ctx, &messageAttempt{message: message, bytes: len(payload)})
	return err
}

// JudgeCrowded is ApplyMessage for a message past stateMaxWaiting, judged beside the project's
// runner (judgeCrowded).
func JudgeCrowded(ctx context.Context, pool *store.Pool, admissions Admissions, payload []byte) error {
	message, err := decodeStateMessage(&contracts.Envelope{Payload: string(payload)}, nil)
	if err != nil {
		return err
	}
	_, err = newIntake(pool, admissions, nil).judgeCrowded(ctx, &messageAttempt{message: message, bytes: len(payload)})
	return err
}
