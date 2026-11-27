package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The work hold (`gates.hold: work`, steps 1 to 7 of its spec under `work`, and its plan's
// decisions 13 to 19) as the workflow writes it: at the close, the record of the hold,
// a message to every live architect and phase worker of the tree carrying each worker's stop, and
// the stop of a released worker that is not a merger; at the opening, today's open message to
// every live holder and one start for each claim the hold stopped; at the boot, the close a tree
// was owed and the opening a tree no longer held is owed. The rows are asserted, not the outbox's
// run of them, which internal/daemon's tests hold (t-6).

// holdNow is the clock of every engine of these tests, and so the moment a close records.
var holdNow = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

// workHoldWords is the sentence the close message adds under `work`, after today's first one
// (spec step 2), and which every opening task repeats.
const workHoldWords = "The deployment holds the tree's work until every gated step stands open again: no phase " +
	"starts, and the daemon stops each phase worker, with its session kept, when its turn ends or 2 minutes after this message."

// holdWords is every text the hold sends about one gated document: tree is the root whose gate it
// is, step its step and document its id. Each text is written out word for word here, never read
// from the code under test, so a change of a word fails a row (H12).
type holdWords struct {
	tree     string
	step     record.DesignStep
	document string
}

// legionHold is the words of LEGION-208's spec, the gate most of these tests move.
var legionHold = holdWords{tree: "LEGION-208", step: record.DesignStepSpec, document: "artifact-208"}

func (w holdWords) closed(version int) string {
	return fmt.Sprintf("The design gate of %s closed: version %d of its %s document (%s) is with its reviewer, "+
		"so the design you are working to may change.", w.tree, version, w.step, w.document)
}

// mergeClose is today's close message, which `merge` keeps word for word (H1).
func (w holdWords) mergeClose(version int) string {
	return gateMovedBodyOf(w.tree, w.step, w.document, version, false)
}

// opened is today's open message, which both modes send at the opening.
func (w holdWords) opened(version int) string {
	return gateMovedBodyOf(w.tree, w.step, w.document, version, true)
}

// workerClose is the close message under `work` to a phase worker (spec step 2).
func (w holdWords) workerClose(version int) string {
	return w.closed(version) + " " + workHoldWords + " End your turn now."
}

// architectClose is the close message under `work` to an architect (spec step 2, decision 14).
func (w holdWords) architectClose(version int) string {
	return w.closed(version) + " " + workHoldWords +
		" Your own work goes on, and a step that you report starts nothing until every gated step stands open again."
}

// opening is the sentences the one task of the opening starts with: the
// close message's two sentences, which name the version that closed the gate, that the hold
// stopped the turn, today's open message, and the check of what the turn already did.
func (w holdWords) opening(closedAt, openedAt int) string {
	return w.closed(closedAt) + " " + workHoldWords + " The work hold stopped your turn. " + w.opened(openedAt) +
		" Check what your turn already did: a tool call may not have finished."
}

// bootOpened is what the boot says in place of today's open message where a tree no longer holds
// work although its gate did not open: the deployment moved to `merge`, or turned the gate off.
func (w holdWords) bootOpened() string {
	return "The deployment no longer holds the work of " + w.tree + "'s tree."
}

// bootOpening is the opening sentences of a tree the boot ends the hold of (H26).
func (w holdWords) bootOpening(closedAt int) string {
	return w.closed(closedAt) + " " + workHoldWords + " The work hold stopped your turn. " + w.bootOpened() +
		" Check what your turn already did: a tool call may not have finished."
}

// unrecordedOpening is the opening sentences of a start of a claim the hold stopped where the
// daemon keeps no record of the hold it can use: the hold's own sentence, and that the hold has
// ended with no record of the version that closed the gate. It never says that the deployment no
// longer holds the tree's work.
func (w holdWords) unrecordedOpening() string {
	return workHoldWords + " The work hold stopped your turn. The work hold of " + w.tree + "'s tree has ended, " +
		"and the daemon keeps no record of the version that closed its gate. Check what your turn already did: a tool call may not have finished."
}

// stop is the row of the work hold's stop of role of issue at generation, closed at version at
// closedAt: the suspend the close writes at once for a released worker, which no message reaches,
// naming the version that closed the gate and the deadline HoldStopGrace after the close. It is
// the supervise row of messageStop (record.HoldStop.Suspend).
func (w holdWords) stop(role claim.Role, generation uint64, version int, closedAt time.Time) record.SuperviseRequest {
	return w.messageStop(role, generation, version).Suspend(closedAt.UTC().Add(HoldStopGrace), time.Time{})
}

// messageStop is the same stop as a close message carries it (record.RoleMessage.Stop), for the
// outbox to post once the message reached the claim, with the deadline it names then.
func (w holdWords) messageStop(role claim.Role, generation uint64, version int) *record.HoldStop {
	return &record.HoldStop{Tree: w.tree, Role: role, Generation: generation, Step: w.step, HoldVersion: version,
		Reason: fmt.Sprintf("the work hold: version %d of the %s document (%s) of %s closed the design gate", version, w.step, w.document, w.tree)}
}

// noPhaseTask is the work of an opening task to a claim that holds no phase of its issue.
const noPhaseTask = "The issue's other roles may ask you questions."

// holdEngine is the engine of a deployment whose `gates.hold` is hold and that gates steps, of
// project, logging to log.
func holdEngine(project string, hold config.Hold, steps []record.DesignStep, log *slog.Logger) *Engine {
	return New(record.NewStore(), Config{
		Project: project, Steps: steps, Hold: hold, ReviewRoundCap: 3, Linger: time.Hour, ReviewAppLogin: testReviewApp,
		Repositories: testRepositories, Clock: func() time.Time { return holdNow },
	}, log)
}

// holdRig is the tree most of these tests hold: LEGION-208, whose root's architect writes the
// design while its children work, gated on its spec (artifact-208), approved at version 3.
//
//   - LEGION-208, the root, at admitted, its architect at work;
//   - LEGION-209, implementing: its own architect idle, its planner idle in a phase it finished,
//     its implementer at work, its tester released with its idle pod, and its reviewer suspended
//     by an operator;
//   - LEGION-210, merging: its merger released while its pull request sits in the merge queue, its
//     implementer suspended by the daemon, and its tester failed;
//   - LEGION-211, at admitted, a member no phase of which has started, which holds no claim;
//   - LEGION-212, closed, whose implementer is at work on the production check's look.
type holdRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	logged *testlog.Buffer
}

func newHoldRig(t *testing.T) holdRig {
	t.Helper()
	pool := migratedPool(t)
	rig := holdRig{t: t, pool: pool, logged: &testlog.Buffer{}}
	member := func(key string, stands phase.Phase, status string) record.Issue {
		issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: "LEGION-208", Project: "LEGION", Title: key, Phase: stands, Generation: 4, Status: status, Rank: "V" + key}
		if key != "LEGION-208" {
			issue.Parent = new("LEGION-208")
		}
		return issue
	}
	seedIssue(t, pool, member("LEGION-208", phase.Admitted, "in_progress"))
	seedIssue(t, pool, member("LEGION-209", phase.Implementing, PhaseStatus(phase.Implementing)))
	seedIssue(t, pool, member("LEGION-210", phase.Merging, PhaseStatus(phase.Merging)))
	seedIssue(t, pool, member("LEGION-211", phase.Admitted, "todo"))
	seedIssue(t, pool, member("LEGION-212", phase.Done, "done"))
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3), ApprovedMark: record.EventMark{Seq: 3}, EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: holdNow})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutSlot(t.Context(), tx, record.Slot{Issue: "LEGION-209", Index: 1, AdmittedAt: holdNow})
	})
	rig.claim("LEGION-208", claim.RoleArchitect, supervise.StateWorking, "")
	rig.claim("LEGION-209", claim.RoleArchitect, supervise.StateIdle, "")
	rig.claim("LEGION-209", claim.RolePlanner, supervise.StateIdle, "")
	rig.claim("LEGION-209", claim.RoleImplementer, supervise.StateWorking, "")
	rig.claim("LEGION-209", claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)
	rig.claim("LEGION-209", claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByOperator)
	rig.claim("LEGION-210", claim.RoleMerger, supervise.StateSuspended, supervise.SuspendedByReleased)
	rig.claim("LEGION-210", claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
	rig.claim("LEGION-210", claim.RoleTester, supervise.StateFailed, "")
	rig.claim("LEGION-212", claim.RoleImplementer, supervise.StateWorking, "")
	// The production check's look the closed member's implementer is at work on (check.go).
	seedRecord(t, pool, func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow("LEGION-212", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer,
			Generation: 4, Task: "Look at production.", CheckLook: 1}, holdNow)
		if err != nil {
			return err
		}
		return record.NewStore().Enqueue(t.Context(), tx, row)
	})
	return rig
}

// engine is the rig's engine under hold, gating the spec step, logging to the rig's buffer.
func (r holdRig) engine(hold config.Hold) *Engine {
	return holdEngine("LEGION", hold, config.DesignGateRootIssues.Steps(), slog.New(slog.NewTextHandler(r.logged, nil)))
}

// claim records issue's role claim in state, suspended by by when it is suspended.
func (r holdRig) claim(issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) {
	r.t.Helper()
	seedHoldClaim(r.t, r.pool, "legion", "LEGION-208", issue, role, state, by)
}

// seedHoldClaim records the claim of issue's role in tree, in state and suspended by by.
func seedHoldClaim(t *testing.T, pool *pgxpool.Pool, project, tree, issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatalf("the %s token of %s: %v", role, issue, err)
	}
	session := "ses-" + issue + "-" + string(role)
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $4, $5, 4, $6, $7, $8, 0, 0, 0, 0, $9)`,
		string(token), project, tree, issue, string(role), session, "/sessions/"+session+".jsonl", string(state), string(by)); err != nil {
		t.Fatalf("seed the %s claim of %s: %v", role, issue, err)
	}
}

// heldClaim is one claim of a rig, named by its issue and its role.
type heldClaim struct {
	issue string
	role  claim.Role
}

// stopsMade is the outbox having made the hold's stops the close asked for: each claim it
// stopped stands suspended by the hold.
func (r holdRig) stopsMade(claims ...heldClaim) {
	r.t.Helper()
	for _, c := range claims {
		stopByTheHold(r.t, r.pool, treeRoleToken(r.t, c.issue, c.role))
	}
}

// stopByTheHold is the outbox having made the hold's stop of the claim token: it stands
// suspended by the hold.
func stopByTheHold(t *testing.T, pool *pgxpool.Pool, token string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `update claims set state = 'suspended', suspended_by = 'hold' where token = $1`, token); err != nil {
		t.Fatalf("stop %s by the hold: %v", token, err)
	}
}

// legionStops is every claim of the rig the hold stops: the live phase workers and the two
// released ones.
var legionStops = []heldClaim{
	{"LEGION-209", claim.RolePlanner}, {"LEGION-209", claim.RoleImplementer}, {"LEGION-209", claim.RoleTester}, {"LEGION-210", claim.RoleMerger},
}

// close writes version of LEGION-208's spec, which closes its standing gate.
func (r holdRig) close(engine *Engine, version int) {
	r.t.Helper()
	applyMirrored(r.t, r.pool, engine, fmt.Sprintf("version-%d", version), intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: version})
}

// open is a person's approval of version of LEGION-208's spec, which opens its standing gate.
func (r holdRig) open(engine *Engine, version int) {
	r.t.Helper()
	applyMirrored(r.t, r.pool, engine, fmt.Sprintf("approved-%d", version), stepApproval("LEGION-208", "artifact-208", version, record.EventMark{Seq: 12}))
}

// boot is a boot of the daemon under engine (intake.DaemonBooted).
func (r holdRig) boot(engine *Engine) {
	r.t.Helper()
	applyMirrored(r.t, r.pool, engine, "boot-"+randomSuffix(r.t), intake.DaemonBooted{})
}

// roleMessagesOf is every role message the outbox holds for issue, oldest first, whole.
func roleMessagesOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.RoleMessage {
	t.Helper()
	var messages []record.RoleMessage
	for _, row := range issueOutboxOf(t, pool, issue, "role_message") {
		var message record.RoleMessage
		if err := json.Unmarshal(row.payload, &message); err != nil {
			t.Fatalf("decode role message %d: %v", row.id, err)
		}
		messages = append(messages, message)
	}
	return messages
}

// wantMessages holds issue's role messages, whole, to want.
func wantMessages(t *testing.T, pool *pgxpool.Pool, issue string, want []record.RoleMessage) {
	t.Helper()
	got := roleMessagesOf(t, pool, issue)
	if len(got) != len(want) {
		t.Fatalf("%s holds %d role messages, want %d:\n got %s\nwant %s", issue, len(got), len(want), asJSON(got), asJSON(want))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("role message %d of %s is\n %s\nwant\n %s", i+1, issue, asJSON(got[i]), asJSON(want[i]))
		}
	}
}

// wantSupervise holds issue's supervise rows of op, whole, to want.
func wantSupervise(t *testing.T, pool *pgxpool.Pool, issue, op string, want []record.SuperviseRequest) {
	t.Helper()
	got := superviseRowsOf(t, pool, issue, op)
	if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("the %s rows of %s are\n %s\nwant\n %s", op, issue, asJSON(got), asJSON(want))
	}
}

// asJSON is v as a failure prints it.
func asJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(encoded)
}

// workHoldOf is the hold the record keeps of tree, nil for none, read with its row locked for the
// read's own transaction.
func workHoldOf(t *testing.T, pool *pgxpool.Pool, tree string) *record.WorkHold {
	t.Helper()
	var hold *record.WorkHold
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		hold, err = record.NewStore().LockWorkHold(t.Context(), tx, tree)
		return err
	})
	return hold
}

// legionRecord is the record of LEGION-208's hold, closed at version.
func legionRecord(version int) record.WorkHold {
	return record.WorkHold{Project: "LEGION", Tree: "LEGION-208", RootGeneration: 4, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: version, ClosedAt: holdNow}
}

// phaseOpening is the opening start of the claim of issue's role that holds its phase: the one
// task, the opening's sentences and then the phase work as the boot builds it (ResumeTask, for an
// issue with no plan rows and the rigs' working copy of no directory), with the opening sentences
// carried apart for supervise's EndHold.
func phaseOpening(issue record.Issue, role claim.Role, opening string) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: opening + "\n\n" + resumeText(issue, WorkingCopy(issue, nil)),
		Generation: issue.Generation, Phase: issue.Phase, ResumeTask: true, NeedsSlot: record.PhaseKeepsSlot(issue.Phase), Opening: opening}
}

// otherOpening is the opening start of a claim that holds no phase of its issue.
func otherOpening(issue record.Issue, role claim.Role, opening string) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: opening + "\n\n" + noPhaseTask,
		Generation: issue.Generation, Opening: opening}
}

// legionCloseMessages is every message of the rig's close under `work`, by the issue whose row
// carries it: every live architect and phase worker of an in-workflow member, and the released
// merger, woken for it; each marked as a close message, and each worker's carrying its stop.
func legionCloseMessages(t *testing.T, version int) map[string][]record.RoleMessage {
	stop := func(role claim.Role) *record.HoldStop {
		return legionHold.messageStop(role, 4, version)
	}
	return map[string][]record.RoleMessage{
		"LEGION-208": {{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: legionHold.architectClose(version), Close: true}},
		"LEGION-209": {
			{Role: treeRoleToken(t, "LEGION-209", claim.RoleArchitect), Body: legionHold.architectClose(version), Close: true},
			{Role: treeRoleToken(t, "LEGION-209", claim.RolePlanner), Body: legionHold.workerClose(version), Close: true, Stop: stop(claim.RolePlanner)},
			{Role: treeRoleToken(t, "LEGION-209", claim.RoleImplementer), Body: legionHold.workerClose(version), Close: true, Stop: stop(claim.RoleImplementer)},
		},
		"LEGION-210": {{Role: treeRoleToken(t, "LEGION-210", claim.RoleMerger), Body: legionHold.workerClose(version), Close: true, Wake: true,
			Stop: stop(claim.RoleMerger)}},
		"LEGION-211": nil,
		"LEGION-212": nil,
	}
}

// wantLegionClose holds the rig's outbox to the close under `work` at version: its messages, the
// one stop it writes at once (the released tester's), and its record.
func wantLegionClose(t *testing.T, pool *pgxpool.Pool, version int) {
	t.Helper()
	for issue, want := range legionCloseMessages(t, version) {
		wantMessages(t, pool, issue, want)
	}
	wantSupervise(t, pool, "LEGION-209", "suspend", []record.SuperviseRequest{legionHold.stop(claim.RoleTester, 4, version, holdNow)})
	for _, issue := range []string{"LEGION-208", "LEGION-210", "LEGION-211", "LEGION-212"} {
		wantSupervise(t, pool, issue, "suspend", nil)
	}
	if hold, want := workHoldOf(t, pool, "LEGION-208"), legionRecord(version); hold == nil || !reflect.DeepEqual(*hold, want) {
		t.Fatalf("the record holds %+v, want %+v", hold, want)
	}
}

// holdStartsOf is issue's supervise starts, the production check's look left out.
func holdStartsOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, issue, "start") {
		if start.CheckLook == 0 {
			starts = append(starts, start)
		}
	}
	return starts
}

// I4: the hold holds for every step a deployment can gate. For each step of record.DesignSteps,
// gated alone, a new version of the step's approved document closes the standing gate: under
// `work` the hold applies, under `merge` today's hold does. Then the gate opens, once by a
// person's approval and once by Dispatch's carried approval after an exact undo, in F3's shape
// (testdata/dispatch-carried-approval.json): under `work` the claim the hold stopped is started
// with one task, under `merge` today's open message goes out. A step added to DesignSteps later is
// in the loop.
func TestWorkHoldHoldsForEveryGatedStep(t *testing.T) {
	for _, step := range record.DesignSteps {
		for _, hold := range []config.Hold{config.HoldWork, config.HoldMerge} {
			for _, opening := range []string{"a person's approval", "Dispatch's carried approval after an exact undo"} {
				t.Run(fmt.Sprintf("%s/%s/%s", step, hold, opening), func(t *testing.T) {
					rig := newCarriedRig(t, step, hold)
					rig.close()
					if hold == config.HoldWork {
						rig.wantWorkClose()
						rig.stopped()
					} else {
						rig.wantMergeClose()
					}
					if opening == "a person's approval" {
						rig.apply("approved-46", stepApproval(carriedRoot, carriedDocument, 46, record.EventMark{Seq: 1457}))
					} else {
						rig.apply("carried-46", CarriedApprovalCapture(t))
					}
					if hold == config.HoldWork {
						rig.wantWorkOpening()
					} else {
						rig.wantMergeOpening()
					}
				})
			}
		}
	}
}

// carriedRoot and carriedDocument are F3's issue and document: Dispatch carried the approval of
// version 24 of that spec onto version 46, an exact undo of the edits since.
const (
	carriedRoot     = "ACME-2694"
	carriedDocument = "aec8fb48-fd46-458a-99a3-a1b199a46b38"
)

// CarriedApprovalCapture is F3, Dispatch's carried approval of version 46 after an exact undo, as
// its events route served it (testdata/dispatch-carried-approval.json), decoded as the daemon's
// live consumer decodes it. It is the one decoder of the capture in this package's tests, exported
// for the rows of the external test package (approval_route_paths_test.go).
func CarriedApprovalCapture(t *testing.T) intake.Fact {
	t.Helper()
	raw, err := os.ReadFile("testdata/dispatch-carried-approval.json")
	if err != nil {
		t.Fatalf("read the captured carried approval: %v", err)
	}
	var captured struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatalf("decode the captured carried approval: %v", err)
	}
	fact, unread, err := intake.DecodeDispatchEvent("ACME", carriedRoot, captured.Event)
	if err != nil || len(unread) > 0 {
		t.Fatalf("decode the captured carried approval's event = %v, unread %v", err, unread)
	}
	artifact, ok := fact.(intake.DispatchArtifact)
	if !ok || !artifact.CarriedApproval() || artifact.Version != 46 || artifact.ArtifactID != carriedDocument || artifact.Mark.Seq != 1457 {
		t.Fatalf("the captured event decodes to %#v, want the carried approval of version 46 of %s at sequence 1457", fact, carriedDocument)
	}
	return fact
}

// carriedRig is F3's root at work: ACME-2694 implementing, its implementer and its architect at
// work, and step gated alone on F3's document, approved at version 24.
type carriedRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
	words  holdWords
	issue  record.Issue
}

func newCarriedRig(t *testing.T, step record.DesignStep, hold config.Hold) carriedRig {
	t.Helper()
	pool := migratedPool(t)
	issue := record.Issue{Key: carriedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: carriedRoot, Project: "ACME", Title: "the carried root", Phase: phase.Implementing, Generation: 1,
		Status: PhaseStatus(phase.Implementing), Rank: "U"}
	seedIssue(t, pool, issue)
	seedGate(t, pool, record.DesignGate{Issue: carriedRoot, Step: step, ArtifactID: carriedDocument, LatestVersion: 24,
		ApprovedVersion: new(24), ApprovedMark: record.EventMark{Seq: 1000}, EverOpened: true})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if err := record.NewStore().PutSlot(t.Context(), tx, record.Slot{Issue: carriedRoot, Index: 0, AdmittedAt: holdNow}); err != nil {
			return err
		}
		_, err := record.NewStore().OpenTreeLifecycle(t.Context(), tx, "acme", carriedRoot, treelifecycle.AuthorityWorkflow)
		return err
	})
	seedHoldClaim(t, pool, "acme", carriedRoot, carriedRoot, claim.RoleArchitect, supervise.StateWorking, "")
	seedHoldClaim(t, pool, "acme", carriedRoot, carriedRoot, claim.RoleImplementer, supervise.StateWorking, "")
	return carriedRig{t: t, pool: pool, engine: holdEngine("ACME", hold, []record.DesignStep{step}, nil),
		words: holdWords{tree: carriedRoot, step: step, document: carriedDocument}, issue: issue}
}

func (r carriedRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	if _, err := intake.ApplyFact(context.Background(), r.pool, "dispatch", id, fact, r.engine, admissionStub{}); err != nil {
		r.t.Fatalf("ApplyFact %s: %v", id, err)
	}
}

// close writes version 45, an edit, which closes the standing gate, and version 46, the exact
// undo of it, which leaves it closed.
func (r carriedRig) close() {
	r.t.Helper()
	for _, version := range []int{45, 46} {
		r.apply(fmt.Sprintf("version-%d", version), intake.DispatchArtifact{Key: carriedRoot, ArtifactID: carriedDocument,
			Kind: intake.DispatchArtifactVersion, Version: version})
	}
}

func (r carriedRig) token(role claim.Role) string {
	r.t.Helper()
	token, err := claim.NewToken("acme", carriedRoot, role)
	if err != nil {
		r.t.Fatalf("the %s token of %s: %v", role, carriedRoot, err)
	}
	return string(token)
}

func (r carriedRig) wantWorkClose() {
	r.t.Helper()
	wantMessages(r.t, r.pool, carriedRoot, []record.RoleMessage{
		{Role: r.token(claim.RoleArchitect), Body: r.words.architectClose(45), Close: true},
		{Role: r.token(claim.RoleImplementer), Body: r.words.workerClose(45), Close: true, Stop: r.words.messageStop(claim.RoleImplementer, 1, 45)},
	})
	want := record.WorkHold{Project: "ACME", Tree: carriedRoot, RootGeneration: 1, Step: r.words.step, ArtifactID: carriedDocument,
		Version: 45, ClosedAt: holdNow}
	if hold := workHoldOf(r.t, r.pool, carriedRoot); hold == nil || !reflect.DeepEqual(*hold, want) {
		r.t.Fatalf("the record holds %+v, want %+v", hold, want)
	}
}

func (r carriedRig) wantMergeClose() {
	r.t.Helper()
	wantMessages(r.t, r.pool, carriedRoot, []record.RoleMessage{
		{Role: r.token(claim.RoleImplementer), Body: r.words.mergeClose(45), Close: true},
		{Role: r.token(claim.RoleArchitect), Body: r.words.mergeClose(45), Close: true},
	})
	wantSupervise(r.t, r.pool, carriedRoot, "suspend", nil)
	if hold := workHoldOf(r.t, r.pool, carriedRoot); hold != nil {
		r.t.Fatalf("under merge the record holds %+v, want no hold of the work", hold)
	}
}

// stopped is the outbox having made the implementer's stop.
func (r carriedRig) stopped() {
	r.t.Helper()
	stopByTheHold(r.t, r.pool, r.token(claim.RoleImplementer))
}

// wantWorkOpening holds the rig to its opening under `work`: the architect, a live holder owed news
// (the rig records no told row), is sent no open message and its told row owes the opening, which
// the task carrying its news leads with (openingDelivery); the implementer the hold stopped is
// started with the one task, which carries the opening, so its told row owes none.
func (r carriedRig) wantWorkOpening() {
	r.t.Helper()
	wantMessages(r.t, r.pool, carriedRoot, []record.RoleMessage{
		{Role: r.token(claim.RoleArchitect), Body: r.words.architectClose(45), Close: true},
		{Role: r.token(claim.RoleImplementer), Body: r.words.workerClose(45), Close: true, Stop: r.words.messageStop(claim.RoleImplementer, 1, 45)},
	})
	assertOpeningsOwed(r.t, r.pool, carriedRoot, carriedRoot+"/architect")
	wantSupervise(r.t, r.pool, carriedRoot, "start", []record.SuperviseRequest{
		phaseOpening(r.issue, claim.RoleImplementer, r.words.opening(45, 46))})
	if hold := workHoldOf(r.t, r.pool, carriedRoot); hold != nil {
		r.t.Fatalf("after the opening the record holds %+v, want none", hold)
	}
}

// wantMergeOpening holds the rig to its opening under `merge`: each holder is owed news (the rig
// records no told row), so it is sent no open message and its told row owes the opening, which the
// task carrying its news leads with (openingDelivery).
func (r carriedRig) wantMergeOpening() {
	r.t.Helper()
	wantMessages(r.t, r.pool, carriedRoot, []record.RoleMessage{
		{Role: r.token(claim.RoleImplementer), Body: r.words.mergeClose(45), Close: true},
		{Role: r.token(claim.RoleArchitect), Body: r.words.mergeClose(45), Close: true},
	})
	assertOpeningsOwed(r.t, r.pool, carriedRoot, carriedRoot+"/architect", carriedRoot+"/holder")
	wantSupervise(r.t, r.pool, carriedRoot, "start", nil)
}

// H1, H16: under `merge` the close is today's, word for word, to each phase holder and the root's
// architect, and stops nothing; the one change is the released merger's message, marked to wake
// it, so it can take its pull request out of the merge queue (decision 13). The opening marks no
// wake: each holder, owed news since the rig records no told row, is told it by the task that
// carries its news (openingDelivery), so it is sent no open message and its told row owes it.
func TestMergeHoldKeepsTodaysTextsAndStopsNothing(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldMerge)
	rig.close(engine, 4)
	closed := map[string][]record.RoleMessage{
		"LEGION-208": {{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: legionHold.mergeClose(4), Close: true}},
		"LEGION-209": {{Role: treeRoleToken(t, "LEGION-209", claim.RoleImplementer), Body: legionHold.mergeClose(4), Close: true}},
		"LEGION-210": {{Role: treeRoleToken(t, "LEGION-210", claim.RoleMerger), Body: legionHold.mergeClose(4), Close: true, Wake: true}},
	}
	for issue, want := range closed {
		wantMessages(t, rig.pool, issue, want)
	}
	for _, issue := range []string{"LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-212"} {
		wantSupervise(t, rig.pool, issue, "suspend", nil)
	}
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
		t.Fatalf("under merge the record holds %+v, want no hold of the work", hold)
	}

	rig.open(engine, 4)
	for issue, want := range closed {
		wantMessages(t, rig.pool, issue, want)
	}
	assertOpeningsOwed(t, rig.pool, "LEGION-208", "LEGION-208/architect", "LEGION-209/holder", "LEGION-210/holder")
	for _, issue := range []string{"LEGION-208", "LEGION-209", "LEGION-210"} {
		wantSupervise(t, rig.pool, issue, "start", nil)
	}
}

// H8, H13, H15, decision 15: the close stops every phase-worker claim of every in-workflow member
// of the tree, idle claims of finished phases too: a live worker's stop rides on its message for
// the outbox to post once the message reached it; a released merger's too, woken for it; a
// released worker that is not a merger is stopped at once, with no message. A claim an operator or
// the daemon suspended, or a failed one, is stopped by nothing (H17, H27), and a closed member's
// look is left alone (H29).
func TestWorkHoldStopsEveryPhaseWorkerOfTheTree(t *testing.T) {
	rig := newHoldRig(t)
	rig.close(rig.engine(config.HoldWork), 4)
	stopped := map[string]bool{}
	for issue := range legionCloseMessages(t, 4) {
		for _, message := range roleMessagesOf(t, rig.pool, issue) {
			if message.Stop != nil {
				stopped[issue+"/"+string(message.Stop.Role)] = true
			}
		}
		for _, stop := range superviseRowsOf(t, rig.pool, issue, "suspend") {
			stopped[issue+"/"+string(stop.Role)] = true
		}
	}
	want := map[string]bool{}
	for _, c := range legionStops {
		want[c.issue+"/"+string(c.role)] = true
	}
	if !reflect.DeepEqual(stopped, want) {
		t.Fatalf("the close stops %v, want every live or released phase worker of an in-workflow member, %v", stopped, want)
	}
}

// H9: no architect is stopped. The root's architect and a child's own are told the close with the
// architect's ending, "Your own work goes on, and a step that you report starts nothing until every
// gated step stands open again.", and carry no stop; no suspend row names an architect.
func TestWorkHoldStopsNoArchitect(t *testing.T) {
	rig := newHoldRig(t)
	rig.close(rig.engine(config.HoldWork), 4)
	told := 0
	for _, issue := range []string{"LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-212"} {
		for _, message := range roleMessagesOf(t, rig.pool, issue) {
			if message.Role != treeRoleToken(t, issue, claim.RoleArchitect) {
				continue
			}
			told++
			if message.Stop != nil || message.Wake || message.Body != legionHold.architectClose(4) {
				t.Fatalf("the architect of %s is told %s, want the architect's close and no stop", issue, asJSON(message))
			}
		}
		for _, stop := range superviseRowsOf(t, rig.pool, issue, "suspend") {
			if stop.Role == claim.RoleArchitect {
				t.Fatalf("the close stops the architect of %s: %s", issue, asJSON(stop))
			}
		}
	}
	if told != 2 {
		t.Fatalf("the close told %d architects, want the root's and LEGION-209's own", told)
	}
}

// H12, decision 14: every live architect and phase worker of the tree is told the close, and so
// is the released merger, woken for it; each text is the one its recipient is owed, word for word.
func TestWorkHoldTellsEveryArchitectAndEveryWorker(t *testing.T) {
	rig := newHoldRig(t)
	rig.close(rig.engine(config.HoldWork), 4)
	wantLegionClose(t, rig.pool, 4)
}

// H14: a released worker that is not a merger is not woken at the close: the hold is recorded on
// it over its `released` source at once, by a stop row with no message. Taken over, it is no
// claim a wake finds: a CI settlement's task and the release backstop's wake each start nothing.
// The opening starts it with one task that begins with the close message's sentences.
func TestWorkHoldHoldsAReleasedWorkerUntilTheOpening(t *testing.T) {
	pool := migratedPool(t)
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Hold: config.HoldWork,
		Pods: wakePods(t, pool), Linger: time.Hour, Repositories: testRepositories, Clock: func() time.Time { return wakeAt }}, nil)
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: wakeIssueGeneration,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, issue)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1", Failing: []string{}})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3), ApprovedMark: record.EventMark{Seq: 3}, EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: wakeAt})
	seedReleasedClaim(t, pool, claim.RoleImplementer, "session-implementer")
	apply := func(id string, fact intake.Fact) { applyMirrored(t, pool, engine, id, fact) }

	apply("version-4", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})
	wantMessages(t, pool, "LEGION-208", nil)
	stop := legionHold.stop(claim.RoleImplementer, wakeIssueGeneration, 4, wakeAt)
	wantSupervise(t, pool, "LEGION-208", "suspend", []record.SuperviseRequest{stop})
	if starts := wakeStarts(t, pool); len(starts) != 0 {
		t.Fatalf("the close woke %+v, want the released worker left in its pod's bundle", starts)
	}

	// The outbox posts the stop, and the hold takes the claim over from the release.
	claimSuspendedBy(t, pool, claim.RoleImplementer, string(supervise.SuspendedByHold))
	apply("settled", settlement("head1", "build"))
	id := armBackstop(t, pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
	apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer})
	if starts, resumes := wakeStarts(t, pool), wakeResumes(t, pool); len(starts) != 0 || len(resumes) != 0 {
		t.Fatalf("a settlement and the backstop started %+v and took the wakes %v, want the held worker left stopped", starts, resumes)
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if _, released, err := wakePods(t, pool).ReleasedClaimOfRole(t.Context(), tx, "LEGION-208", claim.RoleImplementer); err != nil || released {
			t.Fatalf("the held implementer reads released = %v, %v; want no claim a hook prompt's wake finds", released, err)
		}
		return nil
	})

	apply("approved-4", stepApproval("LEGION-208", "artifact-208", 4, record.EventMark{Seq: 12}))
	wantSupervise(t, pool, "LEGION-208", "start", []record.SuperviseRequest{
		phaseOpening(storedIssue(t, pool, "LEGION-208"), claim.RoleImplementer, legionHold.opening(4, 4))})
}

// H4: a gated Verification document holds the work as the spec does. A deployment gating the
// criteria before the spec closes at the criteria's new version, and the record names that
// document; a registration of a new version closes it too (H5), and the opening is the criteria's
// approval.
func TestWorkHoldOverAGatedVerificationDocument(t *testing.T) {
	criteria := holdWords{tree: "LEGION-208", step: record.DesignStepCriteria, document: criteriaDoc}
	for _, tc := range []struct {
		name  string
		close intake.Fact
	}{
		{name: "a new version of it", close: intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: criteriaDoc,
			Kind: intake.DispatchArtifactVersion, Version: 2}},
		{name: "a registration at a new version", close: intake.GateRegistered{Issue: "LEGION-208", Step: record.DesignStepCriteria,
			ArtifactID: criteriaDoc, Version: 2, DispatchVersion: 2, Since: record.EventMark{Seq: 11}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t)
			seedGate(t, rig.pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
				LatestVersion: 1, ApprovedVersion: new(1), ApprovedMark: record.EventMark{Seq: 2}, EverOpened: true})
			engine := holdEngine("LEGION", config.HoldWork, twoSteps(), nil)
			applyMirrored(t, rig.pool, engine, "criteria-2", tc.close)
			wantMessages(t, rig.pool, "LEGION-208", []record.RoleMessage{
				{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: criteria.architectClose(2), Close: true}})
			if got := roleMessagesOf(t, rig.pool, "LEGION-209"); len(got) != 3 || !reflect.DeepEqual(got[2],
				record.RoleMessage{Role: treeRoleToken(t, "LEGION-209", claim.RoleImplementer), Body: criteria.workerClose(2), Close: true,
					Stop: criteria.messageStop(claim.RoleImplementer, 4, 2)}) {
				t.Fatalf("LEGION-209's messages are %s, want the criteria's close with the implementer's stop last", asJSON(got))
			}
			want := record.WorkHold{Project: "LEGION", Tree: "LEGION-208", RootGeneration: 4, Step: record.DesignStepCriteria,
				ArtifactID: criteriaDoc, Version: 2, ClosedAt: holdNow}
			if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold == nil || !reflect.DeepEqual(*hold, want) {
				t.Fatalf("the record holds %+v, want %+v", hold, want)
			}

			rig.stopsMade(legionStops...)
			applyMirrored(t, rig.pool, engine, "criteria-approved-2", stepApproval("LEGION-208", criteriaDoc, 2, record.EventMark{Seq: 12}))
			implementing := storedIssue(t, rig.pool, "LEGION-209")
			wantSupervise(t, rig.pool, "LEGION-209", "start", legionOpeningStarts(implementing, criteria.opening(2, 2)))
		})
	}
}

// A Verification document the root's architect registered as `criteria` beside a deployment that
// gates `spec` and not `criteria` joins the steps the tree is gated on (record.TreeSteps), so its
// row holds the work as a gated document's does: its new version closes the standing gate
// (record.TreeGate.Standing) and applies the work hold, whose record names that document, and its
// approval is the opening.
func TestWorkHoldOverAVerificationDocumentRegisteredBesideTheSpec(t *testing.T) {
	criteria := holdWords{tree: "LEGION-208", step: record.DesignStepCriteria, document: criteriaDoc}
	rig := newHoldRig(t)
	seedGate(t, rig.pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1), ApprovedMark: record.EventMark{Seq: 2}, EverOpened: true})
	engine := rig.engine(config.HoldWork)
	applyMirrored(t, rig.pool, engine, "criteria-2", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: criteriaDoc,
		Kind: intake.DispatchArtifactVersion, Version: 2})
	want := record.WorkHold{Project: "LEGION", Tree: "LEGION-208", RootGeneration: 4, Step: record.DesignStepCriteria,
		ArtifactID: criteriaDoc, Version: 2, ClosedAt: holdNow}
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold == nil || !reflect.DeepEqual(*hold, want) {
		t.Fatalf("the record holds %+v, want %+v: the registered criteria row holds the tree under a deployment of spec alone", hold, want)
	}
	wantMessages(t, rig.pool, "LEGION-208", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: criteria.architectClose(2), Close: true}})

	rig.stopsMade(legionStops...)
	applyMirrored(t, rig.pool, engine, "criteria-approved-2", stepApproval("LEGION-208", criteriaDoc, 2, record.EventMark{Seq: 12}))
	implementing := storedIssue(t, rig.pool, "LEGION-209")
	wantSupervise(t, rig.pool, "LEGION-209", "start", legionOpeningStarts(implementing, criteria.opening(2, 2)))
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
		t.Fatalf("after the criteria's approval the record holds %+v, want none", hold)
	}
}

// H10, H17, decision 18: a person's approval opens the gate, and the daemon starts each claim the
// hold stopped, and no other, with one task: the phase's holder its phase work, any other claim
// that the issue's other roles may ask it questions. The live claims are told today's open
// message, or by the news they are owed where they hold a told row (openingDelivery), an
// operator's suspension and the daemon's are left as they are, and the record goes.
func TestWorkHoldStartsItsClaimsWhenAPersonApproves(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.stopsMade(legionStops...)
	rig.open(engine, 4)
	wantLegionOpening(t, rig, legionHold.opening(4, 4), true)
}

// legionOpeningStarts is LEGION-209's starts at the opening with opening, in the order the
// opening writes them, claim.Roles's: the planner, whose phase is over and which so holds none;
// the implementer, the phase's holder; and the tester, a released worker the hold took over.
func legionOpeningStarts(implementing record.Issue, opening string) []record.SuperviseRequest {
	return []record.SuperviseRequest{
		otherOpening(implementing, claim.RolePlanner, opening),
		phaseOpening(implementing, claim.RoleImplementer, opening),
		otherOpening(implementing, claim.RoleTester, opening),
	}
}

// wantLegionOpening holds the rig to its opening with opening, after every stop of the close was
// made: the start of each held claim, the opening told to the live claims where told is set, and
// no record. The root's architect, a live holder owed news (the rig records no told row), is sent
// no open message, and its told row owes the opening, which the task carrying its news leads with
// (openingDelivery). LEGION-209's own architect holds no told row and is sent the open message.
// The holders the hold stopped are told the opening by the one task that starts each, so their
// told rows owe none.
func wantLegionOpening(t *testing.T, rig holdRig, opening string, told bool) {
	t.Helper()
	implementing, merging := storedIssue(t, rig.pool, "LEGION-209"), storedIssue(t, rig.pool, "LEGION-210")
	wantSupervise(t, rig.pool, "LEGION-209", "start", legionOpeningStarts(implementing, opening))
	wantSupervise(t, rig.pool, "LEGION-210", "start", []record.SuperviseRequest{phaseOpening(merging, claim.RoleMerger, opening)})
	if starts := holdStartsOf(t, rig.pool, "LEGION-208"); len(starts) != 0 {
		t.Fatalf("the opening starts %s of the root, want nothing: no claim of it was held", asJSON(starts))
	}
	if starts := holdStartsOf(t, rig.pool, "LEGION-212"); len(starts) != 0 {
		t.Fatalf("the opening starts %s of the closed member, want nothing", asJSON(starts))
	}
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
		t.Fatalf("after the opening the record holds %+v, want none", hold)
	}
	if !told {
		return
	}
	closeMessages := legionCloseMessages(t, 4)
	wantMessages(t, rig.pool, "LEGION-208", closeMessages["LEGION-208"])
	wantMessages(t, rig.pool, "LEGION-209", append(closeMessages["LEGION-209"],
		record.RoleMessage{Role: treeRoleToken(t, "LEGION-209", claim.RoleArchitect), Body: legionHold.opened(4)}))
	wantMessages(t, rig.pool, "LEGION-210", closeMessages["LEGION-210"])
	assertOpeningsOwed(t, rig.pool, "LEGION-208", "LEGION-208/architect")
}

// H11, F3: Dispatch's carried approval after an exact undo opens the gate as a person's approval
// does, and the claim the hold stopped is started with one task naming the version that closed the
// gate and the version the approval was carried to.
func TestWorkHoldStartsItsClaimsWhenTheChangeIsUndone(t *testing.T) {
	rig := newCarriedRig(t, record.DesignStepSpec, config.HoldWork)
	rig.close()
	rig.wantWorkClose()
	rig.stopped()
	rig.apply("carried-46", CarriedApprovalCapture(t))
	rig.wantWorkOpening()
}

// H32: a gate that opens before the outbox made a stop makes none: the opening writes no stop and
// starts no claim the hold never stopped, every live claim is told the gate opened, a holder owed
// news by the task that carries that news (openingDelivery), the released merger whose stop was
// not made among them, and with the record gone the stops still queued read a tree that holds no
// work.
func TestWorkHoldMakesNoStopAfterTheGateOpens(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.open(engine, 4)
	for _, issue := range []string{"LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-212"} {
		if starts := holdStartsOf(t, rig.pool, issue); len(starts) != 0 {
			t.Fatalf("the opening starts %s of %s, want nothing: the hold stopped no claim", asJSON(starts), issue)
		}
	}
	wantSupervise(t, rig.pool, "LEGION-209", "suspend", []record.SuperviseRequest{legionHold.stop(claim.RoleTester, 4, 4, holdNow)})
	closeMessages := legionCloseMessages(t, 4)
	opened := func(issue string, role claim.Role) record.RoleMessage {
		return record.RoleMessage{Role: treeRoleToken(t, issue, role), Body: legionHold.opened(4)}
	}
	wantMessages(t, rig.pool, "LEGION-208", closeMessages["LEGION-208"])
	wantMessages(t, rig.pool, "LEGION-209", append(closeMessages["LEGION-209"], opened("LEGION-209", claim.RoleArchitect),
		opened("LEGION-209", claim.RolePlanner)))
	assertOpeningsOwed(t, rig.pool, "LEGION-208", "LEGION-208/architect", "LEGION-209/holder", "LEGION-210/holder")
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
		t.Fatalf("after the opening the record holds %+v, want none", hold)
	}
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		gates, err := record.NewStore().GatesOf(t.Context(), tx, "LEGION-208")
		if _, holds := (record.TreeGate{Steps: config.DesignGateRootIssues.Steps(), Rows: gates}).Standing(); err == nil && holds {
			t.Fatal("the gate the stops read still holds the tree's work after the opening")
		}
		return err
	})
}

// The work hold's opening tells each holder the opening once, as tellGateMoved does under `merge`
// for every holder it addresses, live or not: a holder that the hold did not stop and that is
// owed news is sent no open message, and its told row owes the opening, so the task that carries
// its news leads with the opened sentence. Here the root's architect is owed news, since the rig
// records no told row: its idle pod was released after the close, or it holds no claim. A
// restart cannot change the outcome, since the engine keeps nothing between facts, and neither can
// a failed attempt, since the opening's settle is in the transaction that opens the gate.
func TestWorkHoldOpeningOwesTheOpeningToAHolderOwedNewsThatIsNotLive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave string
	}{
		{name: "a released architect", leave: `update claims set state = 'suspended', suspended_by = 'released' where token = $1`},
		{name: "an architect with no claim", leave: `delete from claims where token = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t)
			engine := rig.engine(config.HoldWork)
			rig.close(engine, 4)
			rig.stopsMade(legionStops...)
			if _, err := rig.pool.Exec(t.Context(), tc.leave, treeRoleToken(t, "LEGION-208", claim.RoleArchitect)); err != nil {
				t.Fatalf("leave the root's architect not live: %v", err)
			}
			rig.open(engine, 4)
			wantMessages(t, rig.pool, "LEGION-208", legionCloseMessages(t, 4)["LEGION-208"])
			assertOpeningsOwed(t, rig.pool, "LEGION-208", "LEGION-208/architect")
		})
	}
}

// H30, decision 18: a start written for a held role while the gate was closed — here the
// tester's move back to implementing, which raised the implementer's entry — and the opening give
// that role one task: the newest entry's, behind the opening's sentences, in the one row. A second
// reading of the opening, the boot's, writes no second start.
func TestWorkHoldSendsEachStoppedClaimOneTask(t *testing.T) {
	pool := migratedPool(t)
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 4, Status: PhaseStatus(phase.Testing), Rank: "U"}
	seedIssue(t, pool, issue)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3), ApprovedMark: record.EventMark{Seq: 3}, EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: holdNow})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester"})
	seedHoldClaim(t, pool, "legion", "LEGION-208", "LEGION-208", claim.RoleTester, supervise.StateWorking, "")
	seedHoldClaim(t, pool, "legion", "LEGION-208", "LEGION-208", claim.RoleImplementer, supervise.StateIdle, "")
	engine := holdEngine("LEGION", config.HoldWork, config.DesignGateRootIssues.Steps(), nil)
	rig := holdRig{t: t, pool: pool}

	rig.close(engine, 4)
	rig.stopsMade(heldClaim{"LEGION-208", claim.RoleTester}, heldClaim{"LEGION-208", claim.RoleImplementer})
	result, err := intake.ApplyFact(context.Background(), pool, "api", "back", intake.BackwardMove{Issue: "LEGION-208",
		Requester: claim.RoleTester, To: phase.Implementing, Reason: "the fix does not cover the empty list"}, engine, admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("the move back = %+v, %v", result.Refusal, err)
	}
	written := holdStartsOf(t, pool, "LEGION-208")
	if len(written) != 1 || written[0].Role != claim.RoleImplementer || written[0].Opening != "" {
		t.Fatalf("the move back wrote the starts %s, want the implementer's, which the outbox holds", asJSON(written))
	}

	rig.open(engine, 4)
	implementing := storedIssue(t, pool, "LEGION-208")
	row := phaseRowOf(t, pool, claim.RoleImplementer)
	opening := legionHold.opening(4, 4)
	want := []record.SuperviseRequest{
		{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Task: opening + "\n\n" + row.EntryTask, Generation: 4,
			Phase: phase.Implementing, Entry: row.Entry, NeedsSlot: true, Opening: opening},
		otherOpening(implementing, claim.RoleTester, opening),
	}
	if starts := holdStartsOf(t, pool, "LEGION-208"); !reflect.DeepEqual(starts, want) {
		t.Fatalf("after the opening the starts are\n %s\nwant one task for each held role, the implementer's the newest entry's\n %s", asJSON(starts), asJSON(want))
	}
	rig.boot(engine)
	if starts := holdStartsOf(t, pool, "LEGION-208"); !reflect.DeepEqual(starts, want) {
		t.Fatalf("after a boot the starts are\n %s\nwant still\n %s", asJSON(starts), asJSON(want))
	}
}

// H25: a deployment that moved from `merge` to `work` while a tree's gate stood closed has the
// hold applied at the boot: the record, the messages and the stops, as at a close.
func TestWorkHoldAppliesAtTheBoot(t *testing.T) {
	rig := newHoldRig(t)
	seedGate(t, rig.pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 4, ApprovedVersion: new(3), ApprovedMark: record.EventMark{Seq: 3}, EverOpened: true})
	rig.boot(rig.engine(config.HoldWork))
	wantLegionClose(t, rig.pool, 4)
}

// H24: a held tree whose close was applied starts nothing at a boot, during the drain and after
// it: no message, no stop and no start is written again, and the record stands as it was.
func TestWorkHoldStartsNothingAtTheBoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stops []heldClaim
	}{
		{name: "during the drain", stops: legionStops[1:2]},
		{name: "after it", stops: legionStops},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t)
			engine := rig.engine(config.HoldWork)
			rig.close(engine, 4)
			rig.stopsMade(tc.stops...)
			rig.boot(engine)
			wantLegionClose(t, rig.pool, 4)
			for _, issue := range []string{"LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-212"} {
				if starts := holdStartsOf(t, rig.pool, issue); len(starts) != 0 {
					t.Fatalf("the boot starts %s of %s, want nothing while the tree holds work", asJSON(starts), issue)
				}
			}
		})
	}
}

// H26: a tree whose hold the deployment no longer applies — it moved to `merge`, or turned the gate
// off — has its hold ended at the boot: each claim the hold stopped starts with its task, which
// names the version that closed the gate and says the deployment no longer holds the tree's work,
// each live holder is told so, and the record goes.
func TestWorkHoldEndsUnderMergeAtTheBoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hold  config.Hold
		steps []record.DesignStep
	}{
		{name: "moved to merge", hold: config.HoldMerge, steps: config.DesignGateRootIssues.Steps()},
		{name: "turned the gate off", hold: config.HoldWork, steps: config.DesignGateOff.Steps()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t)
			rig.close(rig.engine(config.HoldWork), 4)
			rig.stopsMade(legionStops...)
			rig.boot(holdEngine("LEGION", tc.hold, tc.steps, nil))
			wantLegionOpening(t, rig, legionHold.bootOpening(4), false)
			told := func(issue string, role claim.Role) record.RoleMessage {
				return record.RoleMessage{Role: treeRoleToken(t, issue, role), Body: legionHold.bootOpened()}
			}
			closeMessages := legionCloseMessages(t, 4)
			wantMessages(t, rig.pool, "LEGION-208", append(closeMessages["LEGION-208"], told("LEGION-208", claim.RoleArchitect)))
			wantMessages(t, rig.pool, "LEGION-209", append(closeMessages["LEGION-209"], told("LEGION-209", claim.RoleArchitect)))
		})
	}
}

// A lingering tree is left alone: its linger suspended every claim of it, so a gate that moves
// then closes nothing, stops nothing and records no hold, and a boot does nothing to it.
func TestWorkHoldLeavesALingeringTreeAlone(t *testing.T) {
	rig := newHoldRig(t)
	root := storedIssue(t, rig.pool, "LEGION-208")
	lingerUntil := holdNow.Add(time.Hour)
	root.LingerUntil = &lingerUntil
	seedIssue(t, rig.pool, root)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.boot(engine)
	for _, issue := range []string{"LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-212"} {
		wantMessages(t, rig.pool, issue, nil)
		wantSupervise(t, rig.pool, issue, "suspend", nil)
	}
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
		t.Fatalf("a lingering tree's record holds %+v, want none", hold)
	}
}

// H29: a closed member's production-check look is not held: its implementer at work on the look is
// told nothing and stopped by nothing at the close, the look's start stands as it was, and the
// opening starts nothing of it.
func TestWorkHoldLeavesAClosedMembersCheckLookAlone(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	look := superviseRowsOf(t, rig.pool, "LEGION-212", "start")
	rig.close(engine, 4)
	wantMessages(t, rig.pool, "LEGION-212", nil)
	wantSupervise(t, rig.pool, "LEGION-212", "suspend", nil)
	rig.stopsMade(legionStops...)
	rig.open(engine, 4)
	wantSupervise(t, rig.pool, "LEGION-212", "start", look)
	if len(look) != 1 || look[0].CheckLook != 1 {
		t.Fatalf("the look's start is %s, want the one look", asJSON(look))
	}
}

// H28: a held tree that is admitted again is not held by its last generation's hold. The
// re-admission's clear of the tree's generation takes the record with the gate rows, and a boot
// that finds a record of an older generation of a root whose own gate stands open drops it and
// holds nothing.
func TestWorkHoldGivesWayToANewerGeneration(t *testing.T) {
	t.Run("the re-admission's clear", func(t *testing.T) {
		rig := newHoldRig(t)
		rig.close(rig.engine(config.HoldWork), 4)
		seedRecord(t, rig.pool, func(tx pgx.Tx) error {
			return rig.engine(config.HoldWork).ClearTreeGeneration(t.Context(), tx, "LEGION-208", holdNow)
		})
		if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
			t.Fatalf("after the tree's new generation the record holds %+v, want none", hold)
		}
	})
	t.Run("a record of an older generation at the boot", func(t *testing.T) {
		rig := newHoldRig(t)
		engine := rig.engine(config.HoldWork)
		rig.close(engine, 4)
		root := storedIssue(t, rig.pool, "LEGION-208")
		root.Generation = 5
		seedIssue(t, rig.pool, root)
		seedGate(t, rig.pool, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208",
			LatestVersion: 1, ApprovedVersion: new(1), ApprovedMark: record.EventMark{Seq: 20}, EverOpened: true})
		before := roleMessagesOf(t, rig.pool, "LEGION-209")
		rig.boot(engine)
		if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
			t.Fatalf("the boot left the older generation's record %+v, want it dropped", hold)
		}
		if after := roleMessagesOf(t, rig.pool, "LEGION-209"); len(after) != len(before) {
			t.Fatalf("the boot told LEGION-209 %s, want nothing new: the newer generation holds nothing", asJSON(after[len(before):]))
		}
	})
}

// Decision 17: a held issue keeps its admission slot. The hold moves no
// phase, so admission's pass over the held tree takes no slot back.
func TestWorkHoldKeepsTheIssuesSlot(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.stopsMade(legionStops...)
	admissionPass(t, rig.pool, engine)
	var slots []record.Slot
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		slots, err = record.NewStore().Slots(t.Context(), tx)
		return err
	})
	if !slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == "LEGION-209" }) {
		t.Fatalf("after the pass the slots are %+v, want LEGION-209's kept while it is held", slots)
	}
}

// H34, decision 17: the pod of a held claim is kept, as an operator-suspended claim's is: the rule
// for a pod no live claim owns holds it under a hold of its own, so the worker's unpushed work stays.
func TestWorkHoldKeepsTheHeldClaimsPod(t *testing.T) {
	view := unownedIssue()
	view.Claims[2].SuspendedBy = supervise.SuspendedByHold
	if !view.Claims[2].WorkHeld() {
		t.Fatalf("the claim %+v does not read held by the work hold", view.Claims[2])
	}
	decision := UnownedPodRelease(view, idleNow, idlePeriod, true)
	if decision.Release || decision.Hold != IdleHoldUnownedWorkHeld {
		t.Fatalf("the rule decided %+v, want the pod kept under %s", decision, IdleHoldUnownedWorkHeld)
	}
	if name := IdleHoldUnownedWorkHeld.String(); name != "unowned_work_held" {
		t.Fatalf("the hold is recorded as %q, want unowned_work_held", name)
	}
	view.Claims[2].SuspendedBy = supervise.SuspendedByDaemon
	if decision := UnownedPodRelease(view, idleNow, idlePeriod, true); !decision.Release {
		t.Fatalf("the rule kept the pod of a daemon-suspended claim under %s, want it released: only the hold's is kept", decision.Hold)
	}
	if (pods.IdleClaim{State: supervise.StateIdle, SuspendedBy: supervise.SuspendedByHold}).WorkHeld() {
		t.Fatal("a live claim reads held by the work hold")
	}
}

// H35: the stranded-claim report passes over a held claim, since the hold says why it waits. The
// phase holder the hold stopped has no start queued, which would report any other suspended phase
// holder an operator suspended.
func TestWorkHoldIsPassedOverByTheStrandedReport(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.stopsMade(legionStops...)
	claims := []supervise.Claim{
		{Token: claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleImplementer)), Project: "legion", Tree: "LEGION-208",
			Issue: "LEGION-209", Role: claim.RoleImplementer, Generation: 4, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByHold},
		{Token: claim.Token(treeRoleToken(t, "LEGION-210", claim.RoleMerger)), Project: "legion", Tree: "LEGION-208",
			Issue: "LEGION-210", Role: claim.RoleMerger, Generation: 4, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator},
	}
	var stranded []supervise.Claim
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		stranded, err = Stranded(t.Context(), tx, record.NewStore(), "LEGION", config.DesignGateRootIssues.Steps(), claims, nil, holdNow)
		return err
	})
	if len(stranded) != 1 || stranded[0].Issue != "LEGION-210" {
		t.Fatalf("the report names %s, want the operator's merger alone: the held implementer is passed over", asJSON(stranded))
	}
}

// H33: each stop and each start of the hold is one daemon log line naming the issue, the role and
// the version that closed the gate.
func TestWorkHoldLogsEachStopAndStart(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	wantLines(t, rig.logged.String(), "workflow: the work hold stops a phase worker of the tree", legionStops)
	rig.logged.Reset()
	rig.stopsMade(legionStops...)
	rig.open(engine, 4)
	wantLines(t, rig.logged.String(), "workflow: the work hold's opening starts a claim it stopped", legionStops)
}

// wantLines holds the log to one line of msg for each claim, naming its issue, its role and the
// version 4 that closed the gate, and no other line of msg.
func wantLines(t *testing.T, logged, msg string, claims []heldClaim) {
	t.Helper()
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			lines = append(lines, line)
		}
	}
	if len(lines) != len(claims) {
		t.Fatalf("the log holds %d lines of %q, want %d, one per claim:\n%s", len(lines), msg, len(claims), logged)
	}
	for _, c := range claims {
		if !slices.ContainsFunc(lines, func(line string) bool {
			return strings.Contains(line, " issue="+c.issue+" ") && strings.Contains(line, " role="+string(c.role)+" ") && strings.Contains(line, " version=4")
		}) {
			t.Fatalf("no line of %q names %s's %s and version 4:\n%s", msg, c.issue, c.role, strings.Join(lines, "\n"))
		}
	}
}

// H33 and spec step 7, for the start the fact of the hold's end writes (intake.WorkHoldOpened).
// The opening read the implementer live and started nothing of it, and the implementer's stop was
// written after the opening. The daemon applies the fact once the read after that stop finds the
// hold ended, and the outbox applies it from a stop row that finds its claim stopped after the
// opening. The opening deleted the tree's work_holds row and kept the hold as the record of the
// tree's last opening. So the one start the fact writes for the claim the hold stopped has the
// opening's task: the close message's sentences naming version 4, that the hold stopped the turn,
// today's open message naming the person's approval of version 4, and the check of the turn. The
// fact here names no version (a stop that named none), so the record alone supplies the version 4
// that the start's log line names.
func TestWorkHoldStartAfterTheOpeningNamesTheVersionOfTheOpening(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.open(engine, 4)
	// The opening read the implementer live and owed news, so its told row owes the opening, as it
	// does the root architect's and the released merger's, whose stop was not made.
	assertOpeningsOwed(t, rig.pool, "LEGION-208", "LEGION-208/architect", "LEGION-209/holder", "LEGION-210/holder")
	if starts := holdStartsOf(t, rig.pool, "LEGION-209"); len(starts) != 0 {
		t.Fatalf("the opening started %s, want nothing: it read the implementer live", asJSON(starts))
	}
	if hold := workHoldOf(t, rig.pool, "LEGION-208"); hold != nil {
		t.Fatalf("the record holds %+v, want no hold that stands: the opening ended it", hold)
	}
	rig.stopsMade(heldClaim{"LEGION-209", claim.RoleImplementer})
	rig.logged.Reset()

	applyMirrored(t, rig.pool, engine, "work-hold-opened", intake.WorkHoldOpened{Tree: "LEGION-208", Version: 0})

	opening := legionHold.opening(4, 4)
	want := phaseOpening(storedIssue(t, rig.pool, "LEGION-209"), claim.RoleImplementer, opening)
	starts := holdStartsOf(t, rig.pool, "LEGION-209")
	if len(starts) != 1 || !strings.HasPrefix(starts[0].Task, opening+"\n\n") || !reflect.DeepEqual(starts[0], want) {
		t.Fatalf("the fact wrote the starts\n %s\nwant the implementer's one, its task behind the opening's sentences\n %s", asJSON(starts), asJSON(want))
	}
	for _, issue := range []string{"LEGION-208", "LEGION-210", "LEGION-211", "LEGION-212"} {
		if starts := holdStartsOf(t, rig.pool, issue); len(starts) != 0 {
			t.Fatalf("the fact starts %s of %s, want nothing: the hold stopped no claim of it", asJSON(starts), issue)
		}
	}
	wantLines(t, rig.logged.String(), "workflow: the work hold's opening starts a claim it stopped",
		[]heldClaim{{"LEGION-209", claim.RoleImplementer}})
	// The one start tells the opening, so the implementer's told row owes it no more (openHeldClaims).
	assertOpeningsOwed(t, rig.pool, "LEGION-208", "LEGION-208/architect", "LEGION-210/holder")
}

// H33 and the architect's ruling of 2026-11-22, for the fact of the hold's end
// (intake.WorkHoldOpened) where the daemon keeps no record of the tree's hold: no close or opening
// was recorded, and the implementer stands stopped by the hold. The fact names version 4, as the
// hold's stop named it. The one start it writes says that the hold has ended and that the daemon
// keeps no record of the version that closed the gate, and the start's log line names version 4,
// the fact's.
func TestWorkHoldStartWithNoRecordNamesTheVersionOfItsStop(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.stopsMade(heldClaim{"LEGION-209", claim.RoleImplementer})

	applyMirrored(t, rig.pool, engine, "work-hold-opened", intake.WorkHoldOpened{Tree: "LEGION-208", Version: 4})

	want := phaseOpening(storedIssue(t, rig.pool, "LEGION-209"), claim.RoleImplementer, legionHold.unrecordedOpening())
	if starts := holdStartsOf(t, rig.pool, "LEGION-209"); len(starts) != 1 || !reflect.DeepEqual(starts[0], want) {
		t.Fatalf("the fact wrote the starts\n %s\nwant the implementer's one, its task behind the sentences of a hold no record keeps\n %s",
			asJSON(starts), asJSON(want))
	}
	wantLines(t, rig.logged.String(), "workflow: the work hold's opening starts a claim it stopped",
		[]heldClaim{{"LEGION-209", claim.RoleImplementer}})
}

// The architect's ruling of 2026-11-22 ("A path that truly cannot know a version ... says so in
// its own words, never that the deployment holds the work"): a boot that finds a claim the hold
// stopped in a tree that holds no work, where the daemon keeps no record of the tree's hold,
// starts the claim with a task that says the hold has ended and that the daemon keeps no record
// of the version that closed the gate. It never says that the deployment no longer holds the
// tree's work: the deployment may still hold it under `work`. The start's log line names no
// version.
func TestWorkHoldStartWithNoRecordSaysTheDaemonKeepsNone(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.stopsMade(heldClaim{"LEGION-209", claim.RoleImplementer})

	rig.boot(engine)

	want := phaseOpening(storedIssue(t, rig.pool, "LEGION-209"), claim.RoleImplementer, legionHold.unrecordedOpening())
	starts := holdStartsOf(t, rig.pool, "LEGION-209")
	if len(starts) != 1 || !reflect.DeepEqual(starts[0], want) {
		t.Fatalf("the boot wrote the starts\n %s\nwant the implementer's one, its task behind the sentences of a hold no record keeps\n %s",
			asJSON(starts), asJSON(want))
	}
	if !strings.Contains(rig.logged.String(), `msg="workflow: the work hold's opening starts a claim it stopped" issue=LEGION-209 role=implementer version=0 `) {
		t.Fatalf("the log holds no start of the implementer naming version 0:\n%s", rig.logged.String())
	}
}

// H28 for the record of an opening: a record of the tree's last opening that names an older
// generation of the root is the hold of a run that ended, and no start uses it. Here the record
// keeps the opening of a hold of generation 3, closed at version 2, while the root stands at
// generation 4, and the fact of the hold's end names version 4. The one start it writes says that
// the daemon keeps no record of the version that closed the gate, never version 2 or that run's
// open message, and the start's log line names version 4, the fact's.
func TestWorkHoldStartUsesNoRecordOfAnOlderGeneration(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	older := record.WorkHold{Project: "LEGION", Tree: "LEGION-208", RootGeneration: 3, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 2, ClosedAt: holdNow}
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		if err := record.NewStore().PutWorkHold(t.Context(), tx, older); err != nil {
			return err
		}
		return record.NewStore().OpenWorkHold(t.Context(), tx, "LEGION-208", holdNow, legionHold.opened(2))
	})
	rig.stopsMade(heldClaim{"LEGION-209", claim.RoleImplementer})

	applyMirrored(t, rig.pool, engine, "work-hold-opened", intake.WorkHoldOpened{Tree: "LEGION-208", Version: 4})

	want := phaseOpening(storedIssue(t, rig.pool, "LEGION-209"), claim.RoleImplementer, legionHold.unrecordedOpening())
	if starts := holdStartsOf(t, rig.pool, "LEGION-209"); len(starts) != 1 || !reflect.DeepEqual(starts[0], want) {
		t.Fatalf("the fact wrote the starts\n %s\nwant the implementer's one, its task behind the sentences of a hold no record keeps, "+
			"not the opening of generation 3\n %s", asJSON(starts), asJSON(want))
	}
	wantLines(t, rig.logged.String(), "workflow: the work hold's opening starts a claim it stopped",
		[]heldClaim{{"LEGION-209", claim.RoleImplementer}})
}

// R2 and spec step 7: a close after an opening makes the hold stand again. The opening ends the
// hold, so the boot, the state and a claim's read of the row read no hold. The next close, of
// version 5, records the hold again and drops the record of the opening of version 4: the boot and
// the state read the hold of version 5 (Store.WorkHolds), the read a claim's decision makes
// answers that the hold stands (supervise.Store.WorkHoldStands), and the tree's record is that
// hold.
func TestWorkHoldStandsAgainAtACloseAfterAnOpening(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	st, err := store.Open(t.Context(), rig.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	stands := func() bool {
		t.Helper()
		held, err := st.WorkHoldStands(t.Context(), "LEGION-208")
		if err != nil {
			t.Fatalf("read the work hold of LEGION-208: %v", err)
		}
		return held
	}
	holds := func() []record.WorkHold {
		t.Helper()
		var holds []record.WorkHold
		seedRecord(t, rig.pool, func(tx pgx.Tx) error {
			var err error
			holds, err = record.NewStore().WorkHolds(t.Context(), tx, "LEGION")
			return err
		})
		return holds
	}
	rig.close(engine, 4)
	rig.open(engine, 4)
	if got := holds(); len(got) != 0 || stands() {
		t.Fatalf("after the opening the project's holds are %+v and the read of the row stands %t, want no hold", got, stands())
	}

	rig.close(engine, 5)

	if got, want := holds(), []record.WorkHold{legionRecord(5)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the second close the project's holds are %+v, want %+v", got, want)
	}
	if !stands() {
		t.Fatal("after the second close the read of the row answers that the hold does not stand, want it to stand")
	}
	var tree *record.WorkHold
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		tree, err = record.NewStore().TreeWorkHold(t.Context(), tx, "LEGION-208")
		return err
	})
	if want := legionRecord(5); tree == nil || !tree.Standing() || !reflect.DeepEqual(*tree, want) {
		t.Fatalf("after the second close the tree's record is %+v, want the hold that stands %+v", tree, want)
	}
	var openings int
	if err := rig.pool.QueryRow(t.Context(), `select count(*) from work_hold_openings where tree = 'LEGION-208'`).Scan(&openings); err != nil {
		t.Fatalf("read the records of the openings: %v", err)
	}
	if openings != 0 {
		t.Fatalf("after the second close the tree keeps %d records of an opening, want none: the close drops the opening of version 4", openings)
	}
}

// heldWhy is the answer the state gives an issue the work hold holds (H36).
func heldWhy(stands phase.Phase) *api.WhyView {
	return &api.WhyView{State: api.WhyHeld, Reason: fmt.Sprintf("Its phase is %s, and the work hold holds the work of its tree LEGION-208 "+
		"until every gated step stands open again: version 4 of its spec document (artifact-208) closed the design gate.", stands)}
}

// projectHeld is the state of the rig's project, projected over the record as it stands.
func projectHeld(t *testing.T, pool *pgxpool.Pool) api.State {
	t.Helper()
	var state api.State
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(t.Context(), projection.Inputs{Tx: tx, Store: record.NewStore(), Project: "LEGION",
			Steps: config.DesignGateRootIssues.Steps(), Now: holdNow})
		return err
	})
	return state
}

// H36, decision 17: the state shows why each held issue waits, `why: held` with the version that
// closed the gate, for each member whose work is a phase worker's; the root, whose architect works
// on, carries no such answer, and none does once the gate opens.
func TestWorkHoldShowsWhyInTheState(t *testing.T) {
	rig := newHoldRig(t)
	engine := rig.engine(config.HoldWork)
	rig.close(engine, 4)
	rig.stopsMade(legionStops...)
	state := projectHeld(t, rig.pool)
	for key, want := range map[string]*api.WhyView{"LEGION-209": heldWhy(phase.Implementing), "LEGION-210": heldWhy(phase.Merging)} {
		if got := state.Issues[key].Why; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s's why is %s, want %s", key, asJSON(got), asJSON(want))
		}
	}
	if why := state.Issues["LEGION-208"].Why; why != nil && why.State == api.WhyHeld {
		t.Fatalf("the root's why is %s, want no held answer: its architect works on", asJSON(why))
	}
	rig.open(engine, 4)
	for key, view := range projectHeld(t, rig.pool).Issues {
		if view.Why != nil && view.Why.State == api.WhyHeld {
			t.Fatalf("after the opening %s's why is %s, want no held answer", key, asJSON(view.Why))
		}
	}
}

// H36: a caller whose plugin is below the contract that added `why: held` reads the state without
// it, through the state route's own narrowing, while a caller at the contract is served it. The
// state is the one the record projects, served by the route the daemon serves it from. A plugin
// of either contract states it in its `Legion-Plugin-Contract` header, as every plugin from the
// contract of the shaped answers (193) does, and the route places it by that; a caller in no pod
// that states nothing, the operator's `legion state`, is served the daemon's own shape.
func TestWorkHoldStateKeepsOlderCallersReading(t *testing.T) {
	entries := api.NumberedEntries()
	at := slices.IndexFunc(entries, func(entry api.NumberedEntry) bool { return entry.Slug == "approval-route" })
	if at < 0 {
		t.Fatal("the approval-route entry is not registered")
	}
	workHoldNumber := entries[at].Number
	rig := newHoldRig(t)
	rig.close(rig.engine(config.HoldWork), 4)
	rig.stopsMade(legionStops...)
	const podAddr, podUID = "10.1.24.6", "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11"
	for _, tc := range []struct {
		name     string
		contract int
		stated   bool
		remote   string
		held     bool
	}{
		{name: "a pod whose plugin is the contract before", contract: workHoldNumber - 1, stated: true, remote: podAddr + ":41000"},
		{name: "a pod at the contract", contract: workHoldNumber, stated: true, remote: podAddr + ":41000", held: true},
		{name: "a caller in no pod", contract: workHoldNumber - 1, remote: "192.0.2.1:41000", held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
			claims := []supervise.Claim{{Token: token, State: supervise.StateIdle, PluginContract: tc.contract, Locator: &runtime.Locator{
				Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: podUID + "/1",
				Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-209", PodUID: podUID, Container: "roles", Generation: 1}}}}
			server := api.NewServer("127.0.0.1", 8437, api.Options{
				Steps: config.DesignGateRootIssues.Steps(), State: projectedSource{pool: rig.pool, claims: claims}, StateTransactions: rig.pool,
				CallerPod: func(addr netip.Addr) (string, bool) { return podUID, addr == netip.MustParseAddr(podAddr) },
				Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
			request.RemoteAddr = tc.remote
			if tc.stated {
				request.Header.Set("Legion-Plugin-Contract", fmt.Sprint(tc.contract))
			}
			recorder := httptest.NewRecorder()
			server.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("state = %d, want 200: %s", recorder.Code, recorder.Body)
			}
			var served struct {
				Issues map[string]struct {
					Why *api.WhyView `json:"why"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &served); err != nil {
				t.Fatalf("decode the state: %v", err)
			}
			for key, issue := range served.Issues {
				if issue.Why != nil && issue.Why.State == api.WhyHeld && !tc.held {
					t.Fatalf("the caller below the contract is served %s's why %s, a word its reader refuses", key, asJSON(issue.Why))
				}
			}
			if got, want := served.Issues["LEGION-209"].Why, heldWhy(phase.Implementing); tc.held != reflect.DeepEqual(got, want) {
				t.Fatalf("LEGION-209's why is %s, want served %v", asJSON(got), tc.held)
			}
		})
	}
}

// projectedSource is the daemon's state source over pool: the projection of the record, with
// claims as the snapshot's own claims, which the route reads the caller's contract from.
type projectedSource struct {
	pool   *pgxpool.Pool
	claims []supervise.Claim
}

func (s projectedSource) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: record.NewStore(), Project: "LEGION",
		Steps: config.DesignGateRootIssues.Steps(), Claims: s.claims, Now: holdNow})
	return state, s.claims, err
}

func (s projectedSource) ObservedState(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	observed, err := projection.Observe(ctx, projection.ObservedInputs{Tx: tx, Store: record.NewStore(), Project: "LEGION",
		Steps: config.DesignGateRootIssues.Steps(), Now: holdNow})
	return observed, s.claims, err
}

// The rule "a queued opening start stands" (OpeningStartStands), the one body the opening and the
// outbox read it with: a queued start of the opening stands over a start for current where it is
// newer than any stop that still acts and than the start in hand's own row, and it does not stand
// where it carries no phase work and the start in hand carries the phase's.
func TestWorkHoldOpeningStartStandsByOneRule(t *testing.T) {
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 4}
	opening := func(id int64, at phase.Phase) record.QueuedSupervise {
		return record.QueuedSupervise{ID: id, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer,
			Generation: 4, Phase: at, Task: legionHold.opening(4, 4) + "\n\n" + noPhaseTask, Opening: legionHold.opening(4, 4)}}
	}
	stop := record.QueuedSupervise{ID: 7, Request: legionHold.stop(claim.RoleImplementer, 4, 5, holdNow)}
	plain := record.QueuedSupervise{ID: 9, Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer,
		Generation: 4, Phase: phase.Implementing, Task: "Implement LEGION-208."}}
	for _, tc := range []struct {
		name    string
		queued  []record.QueuedSupervise
		current phase.Phase
		after   int64
		stands  bool
	}{
		{name: "no queued start", current: phase.Implementing},
		{name: "a queued start that carries no opening", queued: []record.QueuedSupervise{plain}, current: phase.Implementing},
		{name: "an opening start with phase work before a phase start", queued: []record.QueuedSupervise{opening(5, phase.Implementing)},
			current: phase.Implementing, stands: true},
		{name: "an opening start with phase work before a start of no phase", queued: []record.QueuedSupervise{opening(5, phase.Implementing)},
			stands: true},
		{name: "an opening start of no phase before a start of no phase", queued: []record.QueuedSupervise{opening(5, "")}, stands: true},
		{name: "an opening start of no phase before a phase start", queued: []record.QueuedSupervise{opening(5, "")},
			current: phase.Implementing},
		{name: "an opening start older than a stop that still acts", queued: []record.QueuedSupervise{opening(5, phase.Implementing), stop},
			current: phase.Implementing},
		{name: "an opening start newer than a stop that still acts", queued: []record.QueuedSupervise{stop, opening(8, phase.Implementing)},
			current: phase.Implementing, stands: true},
		{name: "the opening start in hand alone", queued: []record.QueuedSupervise{opening(5, "")}, after: 5},
		{name: "an older opening start before the opening start in hand", queued: []record.QueuedSupervise{opening(5, ""), opening(8, "")},
			after: 8},
		{name: "a newer opening start before the opening start in hand", queued: []record.QueuedSupervise{opening(5, ""), opening(8, "")},
			after: 5, stands: true},
		{name: "a newer opening start of no phase before the phase opening start in hand",
			queued: []record.QueuedSupervise{opening(5, phase.Implementing), opening(8, "")}, current: phase.Implementing, after: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := record.RoleRun{Queued: tc.queued}
			if got := OpeningStartStands(run, root, tc.current, tc.after); got != tc.stands {
				t.Fatalf("OpeningStartStands = %v, want %v", got, tc.stands)
			}
		})
	}
}
