//go:build e2e

// The live proof's driver for dispatch://INFRA-3320's check k8 (R1): a root architect's own
// `legion` tool makes the registration R1 takes. scripts/e2e/register-gate-basis.sh builds this
// driver with `-tags e2e` and owns the nats-server it runs against, which it names in
// LEGION_E2E_NATS_URL; the PostgreSQL is LEGION_TEST_PG_DSN's.
//
// The rig is the person-handover proofs' (person_handover_scratch_dispatch_test.go): the real
// Dispatch server built from the checkout LEGION_E2E_DISPATCH_ROOT names (this one by default), on
// a database of its own, publishing its events on that broker, and the daemon in this process
// reading them, with a person's act a request with the trusted identity header. The tool is the
// one packages/pi-legion builds: a bun process (packages/pi-legion/test/register-gate-basis-driver.ts)
// builds createLegionTool with the plugin's own daemon client at the daemon's API and
// envoy-client's DispatchClient at the scratch Dispatch, and holds the claim the daemon registered
// for the root's architect, as the root's pane holds it, so the tool mints each grant from that
// claim's secret in its own process. Every assertion is a read of what the tool answered and the
// requests it sent, of GET /legion/v1/state, of the daemon's own store, or of Dispatch.
//
// Across projects (LEGION_E2E_ACROSS_PROJECTS=1) the roots are issues of project LEGION, whose
// daemon this is, and each issue they rest on is of a second project of the same Dispatch, OTHR,
// which no daemon runs. R is set under P through Dispatch's own parent route, which a Dispatch
// that takes no parent of another project refuses, so the run then fails at its first step. Two
// steps follow the refusal: pending, where R2's step waits on the open approval request of P2 and
// opens at the first controller tick after the person approves P2, and accepted, where R3's
// register_gate on P3 is refused once P3 stands in retro as the sign_off of P3's daemon writes it.
// The daemon then applies no controller tick but the one the pending step hands it
// (overrides.controllerTicks).
//
// Once the daemon has stopped, the driver writes the scratch Dispatch's whole event log, one event
// to a line as GET /api/v1/events/log serves it, to dispatch-events.jsonl beside the daemon's log
// in the evidence directory: scripts/dispatch/cross-project-basis-census.ts reads it.
//
// What is stood in for lies outside the registration: GitHub (blockersGitHub), the Apps' tokens,
// the runtime (the package's fake runtime launches each root's architect, and the rig registers
// its session as the plugin's boot does), and Envoy's listener, which nothing answers, so every
// notice stays in the daemon's outbox to be read.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
)

// registerGateToolRootEnv names the checkout whose packages/pi-legion tool the run drives, its
// driver file included; unset, the checkout the run builds from. The script sets it from
// REGISTER_GATE_BASIS_TOOL_ROOT, which k8's negative control points at the commit the branch starts
// from.
const registerGateToolRootEnv = "LEGION_E2E_TOOL_ROOT"

// registerGateDispatchRootEnv names the checkout whose packages/envoy the scratch Dispatch is built
// from and runs in; unset, the checkout the run builds from. The script sets it from
// REGISTER_GATE_BASIS_DISPATCH_ROOT, which the control of the run across projects points at a
// Dispatch that takes no parent of another project.
const registerGateDispatchRootEnv = "LEGION_E2E_DISPATCH_ROOT"

// registerGateAcrossProjectsEnv, at 1, runs the proof across two projects. The script sets it from
// REGISTER_GATE_BASIS_ACROSS_PROJECTS.
const registerGateAcrossProjectsEnv = "LEGION_E2E_ACROSS_PROJECTS"

// The two projects of the run across projects: the roots' own, and that of the issues they rest
// on.
const (
	registerGateRootProject  = "LEGION"
	registerGateOtherProject = "OTHR"
)

// registerGateEventLog is the file of the evidence directory the scratch Dispatch's event log is
// written to, and registerGateEventLogPage the page it is read in: the route's own maximum.
const (
	registerGateEventLog     = "dispatch-events.jsonl"
	registerGateEventLogPage = 1000
)

// The architect's reason each root's calls carry: R's, R2's and R3's.
const (
	registerGateReason         = "R's one check serves P2 of P, whose spec the person approved at its latest version; R's spec holds no requirement of its own."
	registerGatePendingReason  = "R2's one check serves Q1 of P2, whose spec waits on the person's approval of its latest version; R2's spec holds no requirement of its own."
	registerGateAcceptedReason = "R3's one check serves U1 of P3, whose spec the person approved at its latest version; R3's spec holds no requirement of its own."
)

// registerGateToolDeadline bounds one run of the tool's driver: bun's start and the few requests
// one call makes take seconds, so a driver past it is hung, and the run fails there rather than at
// the go test timeout.
const registerGateToolDeadline = 2 * time.Minute

// registerGateBasisLive is one run of the proof: the rig, the checkout whose tool runs, the home
// bun runs in, and the world the steps build.
type registerGateBasisLive struct {
	*personHandoverRig
	toolRoot string
	home     string
	// across is a run across projects, and ticks then the daemon's one source of controller ticks.
	across bool
	ticks  chan time.Time
	// root is R, a root the daemon admitted, and parent is P, whose primary document the person
	// approved at version approved.
	root, parent scratchIssue
	approved     int
}

// registerGateAnswer is what the tool's driver printed: whether the tool's answer is an error, its
// text, and every request it sent the daemon and Dispatch, as `<METHOD> <path>`.
type registerGateAnswer struct {
	IsError          bool     `json:"isError"`
	Text             string   `json:"text"`
	DaemonRequests   []string `json:"daemonRequests"`
	DispatchRequests []string `json:"dispatchRequests"`
}

// registerGateSent is what a call that reached the daemon sent it: one grant and one registration.
var registerGateSent = []string{"POST /legion/v1/grants", "POST /legion/v1/gates/register"}

// TestRegisterGateBasisToolLive drives the proof's steps against one booted daemon and one
// Dispatch; across projects, two more.
func TestRegisterGateBasisToolLive(t *testing.T) {
	r := newRegisterGateBasisLive(t)
	r.step("world", "P is approved with requirement blocks, and R is a view under it", r.world)
	r.step("registration", "the tool registers R's spec step on P's approval, and the daemon records the basis", r.registers)
	r.step("refusal", "with a decision block open in R's spec, the tool refuses before the daemon, and nothing is recorded", r.refusesOverAnOpenBlock)
	if !r.across {
		return
	}
	r.step("pending", "R2's step waits on P2's open approval request, and opens at the first tick after the person approves P2", r.opensAtTheTick)
	r.step("accepted", "once P3 is accepted, R3's registration on it is refused", r.refusedOnAnAcceptedBasis)
}

// newRegisterGateBasisLive starts Dispatch on the script's broker and boots the daemon on the rig,
// with a cap of two: R takes one slot, and P is never admitted. Across projects the cap is three,
// one for each of R, R2 and R3, and the daemon is LEGION's, which admits no issue of OTHR.
func newRegisterGateBasisLive(t *testing.T) *registerGateBasisLive {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker for Dispatch's events and the daemon's intake; scripts/e2e/register-gate-basis.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	// testbuild.RootEnv names the checkout the script runs from: the driver binary runs from the
	// caller's directory, not the package's. Unset (a plain `go test -tags e2e`, which runs in the
	// package directory), the checkout is four directories up.
	root := os.Getenv(testbuild.RootEnv)
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	toolRoot := os.Getenv(registerGateToolRootEnv)
	if toolRoot == "" {
		toolRoot = root
	}
	dispatchRoot := os.Getenv(registerGateDispatchRootEnv)
	if dispatchRoot == "" {
		dispatchRoot = root
	}
	if dispatchRoot, err = filepath.Abs(dispatchRoot); err != nil {
		t.Fatal(err)
	}
	// testbuild builds the packages/envoy of the checkout testbuild.RootEnv names, once for this
	// process: named here, before the rig starts Dispatch, it builds dispatchRoot's.
	t.Setenv(testbuild.RootEnv, dispatchRoot)
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatalf("bun is not on PATH, and the legion tool runs under it: %v", err)
	}
	board := scratchDispatchOptions{natsURL: natsURL, root: dispatchRoot}
	daemonLog := os.Getenv(liveDaemonLogEnv)
	if daemonLog != "" {
		file, err := os.OpenFile(filepath.Join(filepath.Dir(daemonLog), "dispatch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open Dispatch's log: %v", err)
		}
		t.Cleanup(func() { file.Close() })
		board.log = file
	}
	across := os.Getenv(registerGateAcrossProjectsEnv) == "1"
	var rig *personHandoverRig
	if across {
		rig = newProjectRigOn(t, startScratchDispatchOn(t, board), natsURL, registerGateRootProject, "acme/widgets", 3)
		scratchProject(t, rig.baseURL, registerGateOtherProject, "acme/gadgets")
	} else {
		rig = newPersonHandoverRigOn(t, board, 2)
	}
	r := &registerGateBasisLive{personHandoverRig: rig, toolRoot: toolRoot, home: t.TempDir(), across: across}
	if across {
		r.ticks = make(chan time.Time)
		r.tune = func(o *overrides) { o.controllerTicks = r.ticks }
	}
	if daemonLog != "" {
		// The rig keeps the daemon's log in memory, where its proofs read it. Registered before the
		// boot, these cleanups run after the daemon has stopped and before Dispatch stops, whose
		// cleanup the rig registered first: the file holds the whole log, and the event log holds
		// every event the run made.
		t.Cleanup(func() {
			if err := os.WriteFile(daemonLog, []byte(r.logs.String()), 0o600); err != nil {
				t.Errorf("write the daemon's log: %v", err)
			}
		})
		t.Cleanup(func() { r.saveEventLog(filepath.Join(filepath.Dir(daemonLog), registerGateEventLog)) })
	}
	r.boot()
	projects := "project " + r.cfg.Project
	if across {
		projects = fmt.Sprintf("roots in project %s and the issues they rest on in project %s", registerGateRootProject, registerGateOtherProject)
	}
	r.note("Dispatch %s built from %s (database of its own, events on %s), %s; daemon %s of project %s; the tool of %s",
		r.baseURL, dispatchRoot, natsURL, projects, r.d.base, r.cfg.Project, toolRoot)
	return r
}

// step runs one of the proof's steps between its `== name` and `ok name (<s>s)` lines, as the
// repository's live proofs print them, with what it proves as its first line.
func (r *registerGateBasisLive) step(name, proves string, body func()) {
	r.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	r.note("%s", proves)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// note prints what a step drove or read, under its heading.
func (r *registerGateBasisLive) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// resting is fields with the project of the issues the roots rest on: OTHR across projects, and
// the rig's own project otherwise, which create names when fields name none.
func (r *registerGateBasisLive) resting(fields map[string]any) map[string]any {
	if r.across {
		fields["project"] = registerGateOtherProject
	}
	return fields
}

// setUnder sets parent as root's Dispatch parent as the person does at the dashboard, through
// Dispatch's own route, and waits for the daemon to apply the link. root and parent are of two
// projects, so a Dispatch that takes no parent of another project refuses the write, and the run
// ends here with Dispatch's answer.
func (r *registerGateBasisLive) setUnder(root, parent scratchIssue) {
	t := r.t
	t.Helper()
	status, answer := dispatchAnswerAs(t, r.baseURL, http.MethodPatch, "/api/v1/issues/"+root.Key, map[string]any{"parent": parent.Key}, asPerson)
	if status != http.StatusOK {
		t.Fatalf("Dispatch refused to set %s, of project %s, under %s, of project %s: PATCH /api/v1/issues/%s {\"parent\": %q} as the person = %d %s",
			root.Key, registerGateRootProject, parent.Key, registerGateOtherProject, root.Key, parent.Key, status, answer)
	}
	r.quiet(root.Key)
	if shown := r.issue(root.Key); shown.Parent == nil || *shown.Parent != parent.Key {
		t.Fatalf("Dispatch shows %s under %v after the link, want %s", root.Key, shown.Parent, parent.Key)
	}
}

// Step 1. R, labelled by a session, is set todo by the person and admitted, and its architect is
// launched and registered: R runs as a root of its own. P, labelled by a session and in progress,
// so the daemon records nothing of it, marks p1 to p3 in its primary document, and the person
// approves its latest version. The person sets P as R's parent, and R's one check serves P2.
// Across projects P is of OTHR, which this daemon never records, and R is set under it through
// Dispatch's parent route.
func (r *registerGateBasisLive) world() {
	t := r.t
	t.Helper()
	labelled := map[string]any{"labels": []string{"legion"}}
	r.root = r.create(asSession, "Ledger export rewrite", labelled)
	r.admit(r.root.Key)
	r.parent = r.create(asSession, "Quarterly billing overhaul", r.resting(map[string]any{"labels": []string{"legion"},
		"spec": requirementsMarkdown("Billing", "p1", "p2", "p3")}))
	r.setStatus(r.parent.Key, "in_progress")
	r.approved = r.approve(r.parent.PrimaryArtifactID).Version
	if r.across {
		r.setUnder(r.root, r.parent)
	} else {
		r.linkUnder(r.root, r.parent, false)
	}
	r.writeDocument(r.root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(r.parent.Key, "p2")}}))
	if r.across {
		r.quiet(r.root.Key)
	} else {
		r.quiet(r.root.Key, r.parent.Key)
	}
	state, body := settled(t, r.d, "answer its state before the registration", func(api.State) bool { return true })
	if step := stepOf(state, r.root.Key); step.Status != api.GatedStepUnregistered || r.gateFacts(r.root.Key) != 0 {
		t.Fatalf("before the registration %s's spec step = %+v with %d gate facts, want unregistered and none\nstate %s", r.root.Key, step, r.gateFacts(r.root.Key), body)
	}
	architect := r.architects[r.root.Key]
	r.note("R = %s, recorded in %s with a slot, its architect's claim %s registered as session %s", r.root.Key, state.Issues[r.root.Key].Phase, architect.launch.Claim, architect.session)
	r.note("P = %s, labelled and in progress, unrecorded; the person approved version %d of its primary document %s, which marks p1, p2 and p3",
		r.parent.Key, r.approved, r.parent.PrimaryArtifactID)
	r.note("%s is under %s, and the one check of its Verification document serves %s; its spec step is unregistered", r.root.Key, r.parent.Key, specBlock(r.parent.Key, "p2"))
}

// Step 2. R's architect's tool calls register_gate with step spec, R's primary document by its
// reference and version, the basis dispatch://<P>/spec at P's approved version, and a reason. The
// tool looks the basis up on P, mints a grant and registers; the daemon records R's spec step open
// on P's approval (its gate row's basis, the state's open_on_basis) and R plans. Across projects,
// the message that records the opening names P as the ancestor, as the census reads it.
func (r *registerGateBasisLive) registers() {
	t := r.t
	t.Helper()
	answer := r.tool(r.root, r.call(r.root, r.parent, 1, r.approved, registerGateReason))
	if answer.IsError || !slices.Equal(answer.DaemonRequests, registerGateSent) {
		t.Fatalf("the tool's registration of %s on %s answered %+v, want a success after one grant and one registration sent to the daemon", r.root.Key, r.parent.Key, answer)
	}
	r.readsTheBasis(answer, r.parent)
	state, message := r.openedOnBasis(r.root, r.parent, 1, r.approved)
	step := stepOf(state, r.root.Key)
	gate := r.gates(r.root.Key)[record.DesignStepSpec]
	session := r.architects[r.root.Key].session
	if basis := gate.Basis; basis == nil || basis.Issue != r.parent.Key || basis.ArtifactID != r.parent.PrimaryArtifactID || basis.Version != r.approved ||
		basis.Approver != "user:smoke" || basis.Reason != registerGateReason || basis.Session != session || basis.Pending != nil ||
		gate.ArtifactID != r.root.PrimaryArtifactID || gate.LatestVersion != 1 {
		t.Fatalf("the daemon recorded %s's spec step as %+v (basis %+v), want version 1 of %s open on version %d of %s's primary document %s, approved by user:smoke, with the tool's reason and session %s",
			r.root.Key, gate, gate.Basis, r.root.PrimaryArtifactID, r.approved, r.parent.Key, r.parent.PrimaryArtifactID, session)
	}
	if facts := r.gateFacts(r.root.Key); facts != 1 {
		t.Fatalf("the daemon applied %d gate facts for %s, want the one registration", facts, r.root.Key)
	}
	if r.across {
		r.namesTheAncestor(r.root, r.parent, message)
	}
	r.note("the daemon's gate row of %s: spec step at version %d of %s, basis %s version %d (%s), approver %s, reason %q, session %s",
		r.root.Key, gate.LatestVersion, gate.ArtifactID, gate.Basis.Issue, gate.Basis.Version, gate.Basis.ArtifactID, gate.Basis.Approver, gate.Basis.Reason, gate.Basis.Session)
	r.note("GET /legion/v1/state: %s in %s, its spec step %s on %s version %d", r.root.Key, state.Issues[r.root.Key].Phase, step.Status, step.Basis.Issue, step.Basis.Version)
	r.note("the message the daemon posted on %s:\n%s", r.root.Key, message)
}

// Step 3. A session writes a decision block into R's spec, and Dispatch opens its ask. The same
// call, at the version that holds the block, is refused by the tool, naming the block, with no
// request sent to the daemon; the daemon's record of R, its gate rows, its gate facts and R's
// Dispatch issue are as they were.
func (r *registerGateBasisLive) refusesOverAnOpenBlock() {
	t := r.t
	t.Helper()
	version := r.editDocument(r.root.PrimaryArtifactID, insertAtEnd(personHandoverBlock))
	ask := r.blockAsk(r.root.Key, "wave")
	// The daemon's own writes to Dispatch for R run first, so nothing of them lands between the
	// two snapshots.
	r.dispatchRowsDone(r.root.Key)
	r.quiet(r.root.Key)
	before := r.snapshot(r.root.Key)
	r.note("version %d of %s's spec holds decision block wave, open as ask %s; the daemon records %s in %s with %d gate facts",
		version, r.root.Key, ask, r.root.Key, before.phase, before.gateFacts)
	answer := r.tool(r.root, r.call(r.root, r.parent, version, r.approved, registerGateReason))
	if !answer.IsError || len(answer.DaemonRequests) != 0 {
		t.Fatalf("the tool's call over the open block answered %+v, want a refusal that sent the daemon nothing", answer)
	}
	for _, want := range []string{"legion register_gate was not called", fmt.Sprintf("(version %d) has 1 open decision block", version), "block wave, ask " + ask} {
		if !strings.Contains(answer.Text, want) {
			t.Errorf("the tool's refusal does not say %q:\n%s", want, answer.Text)
		}
	}
	if after := r.snapshot(r.root.Key); !reflect.DeepEqual(before, after) {
		t.Fatalf("the refused call changed %s: before %+v, after %+v", r.root.Key, before, after)
	}
	r.note("the daemon's record of %s, its gate rows, its %d gate facts and its Dispatch issue are as they were", r.root.Key, before.gateFacts)
}

// Step 4, across projects. P2, of OTHR and labelled, marks q1 and q2, and its approval request of
// version 1 is open on the person. R2, labelled and admitted, is set under P2, and its one check
// serves Q1. R2's architect's tool registers on P2's requested version, and R2's step waits on that
// request. The person approves P2; this daemon reads no event of OTHR, so a later event of R2,
// applied, finds the step still waiting and no tick run. One controller tick reads P2 again, and
// R2's step opens on the approval: R2 plans, its architect is told design-approved naming P2's
// version, and the message on R2 names P2 as the ancestor.
func (r *registerGateBasisLive) opensAtTheTick() {
	t := r.t
	t.Helper()
	ancestor := r.create(asSession, "Warehouse inventory audit", r.resting(map[string]any{"labels": []string{"legion"},
		"spec": requirementsMarkdown("Inventory", "q1", "q2")}))
	ask := r.requestApproval(ancestor.PrimaryArtifactID, "The inventory audit is ready for review.")
	root := r.create(asSession, "Tidal gauge calibration", map[string]any{"labels": []string{"legion"}})
	r.admit(root.Key)
	r.setUnder(root, ancestor)
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(ancestor.Key, "q1")}}))
	r.quiet(root.Key)
	r.note("R2 = %s under P2 = %s, whose approval request %s of version 1 of its primary document %s is open on the person; R2's one check serves %s",
		root.Key, ancestor.Key, ask, ancestor.PrimaryArtifactID, specBlock(ancestor.Key, "q1"))

	answer := r.tool(root, r.call(root, ancestor, 1, 1, registerGatePendingReason))
	if answer.IsError || !slices.Equal(answer.DaemonRequests, registerGateSent) {
		t.Fatalf("the tool's registration of %s on the open request of %s answered %+v, want a success after one grant and one registration sent to the daemon", root.Key, ancestor.Key, answer)
	}
	r.readsTheBasis(answer, ancestor)
	waiting, body := settled(t, r.d, root.Key+" to wait on "+ask, func(s api.State) bool {
		step := stepOf(s, root.Key)
		return step.Status == api.GatedStepPendingOnBasis && step.Basis != nil && step.Basis.Issue == ancestor.Key && step.Basis.AskID == ask
	})
	gate := r.gates(root.Key)[record.DesignStepSpec]
	if gate.Basis == nil || gate.Basis.Issue != ancestor.Key || gate.Basis.Pending == nil || gate.Basis.Reason != registerGatePendingReason || r.gateFacts(root.Key) != 1 {
		t.Fatalf("the daemon recorded %s's spec step as %+v (basis %+v) with %d gate facts, want it pending on %s's request with the tool's reason, after one registration\nstate %s",
			root.Key, gate, gate.Basis, r.gateFacts(root.Key), ancestor.Key, body)
	}
	r.note("GET /legion/v1/state: %s in %s, its spec step %s on %s's request %s", root.Key, waiting.Issues[root.Key].Phase, stepOf(waiting, root.Key).Status, ancestor.Key, ask)

	ticked := controllerTicks(t, r.pool)
	approval := r.approve(ancestor.PrimaryArtifactID)
	r.patch(root.Key, asSession, map[string]any{"title": "Tidal gauge calibration, phase one"})
	r.quiet(root.Key)
	if step := stepOf(r.d.state(), root.Key); step.Status != api.GatedStepPendingOnBasis || controllerTicks(t, r.pool) != ticked {
		t.Fatalf("after the person's approval of %s and before a tick, the spec step of %s = %+v (ticks %d, were %d), want it pending",
			ancestor.Key, root.Key, step, controllerTicks(t, r.pool), ticked)
	}
	r.note("the person approved version %d of %s; a later event of %s is applied, no tick has run, and its step still waits", approval.Version, ancestor.Key, root.Key)

	r.controllerTick(r.ticks)
	state, message := r.openedOnBasis(root, ancestor, 1, approval.Version)
	r.namesTheAncestor(root, ancestor, message)
	step := stepOf(state, root.Key)
	r.note("one controller tick (%d applied, were %d); GET /legion/v1/state: %s in %s, its spec step %s on %s version %d, approved by %s",
		controllerTicks(t, r.pool), ticked, root.Key, state.Issues[root.Key].Phase, step.Status, step.Basis.Issue, step.Basis.Version, step.Basis.Approver)
	r.note("the message the daemon posted on %s:\n%s", root.Key, message)
}

// Step 5, across projects. P3, of OTHR, labelled and in progress, marks u1 and u2, and the person
// approves its latest version. P3 is then accepted: its status is written retro as the sign_off of
// P3's own daemon writes it (dispatch.HTTPClient.SetAcceptanceStatus, with
// refuse_open_foreign_children), while no child stands under it. R3, labelled and admitted, is
// then set under P3, and its one check serves U1. R3's architect's tool registers on P3's approved
// version: the daemon answers 409 BASIS_ACCEPTED, naming P3 retro, and records nothing of R3's
// gate.
func (r *registerGateBasisLive) refusedOnAnAcceptedBasis() {
	t := r.t
	t.Helper()
	ancestor := r.create(asSession, "Satellite uplink migration", r.resting(map[string]any{"labels": []string{"legion"},
		"spec": requirementsMarkdown("Uplink", "u1", "u2")}))
	r.setStatus(ancestor.Key, "in_progress")
	approved := r.approve(ancestor.PrimaryArtifactID).Version
	client := dispatch.New(r.baseURL, r.scratch.token)
	read, err := client.GetIssue(t.Context(), ancestor.Key)
	if err != nil {
		t.Fatalf("read %s before its acceptance: %v", ancestor.Key, err)
	}
	accepted, err := client.SetAcceptanceStatus(t.Context(), read, "retro")
	if err != nil || accepted.Status != "retro" {
		t.Fatalf("the acceptance of %s, written as its daemon's sign_off writes it, = %+v, %v; want retro", ancestor.Key, accepted, err)
	}
	root := r.create(asSession, "Buoy maintenance roster", map[string]any{"labels": []string{"legion"}})
	r.admit(root.Key)
	r.setUnder(root, ancestor)
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(ancestor.Key, "u1")}}))
	r.quiet(root.Key)
	r.note("P3 = %s: the person approved version %d of its primary document %s, and Dispatch shows it %s; R3 = %s under it, whose one check serves %s",
		ancestor.Key, approved, ancestor.PrimaryArtifactID, r.issue(ancestor.Key).Status, root.Key, specBlock(ancestor.Key, "u1"))

	answer := r.tool(root, r.call(root, ancestor, 1, approved, registerGateAcceptedReason))
	if !answer.IsError || !slices.Equal(answer.DaemonRequests, registerGateSent) {
		t.Fatalf("the tool's registration of %s on %s answered %+v, want the daemon's refusal of its one registration", root.Key, ancestor.Key, answer)
	}
	for _, want := range []string{"409 BASIS_ACCEPTED", "Dispatch shows " + ancestor.Key + " retro"} {
		if !strings.Contains(answer.Text, want) {
			t.Fatalf("the tool's answer does not say %q:\n%s", want, answer.Text)
		}
	}
	if facts, gates := r.gateFacts(root.Key), r.gates(root.Key); facts != 0 || len(gates) != 0 {
		t.Fatalf("the refused registration left %d gate facts and the gate rows %+v on %s, want none", facts, gates, root.Key)
	}
	if step := stepOf(r.d.state(), root.Key); step.Status != api.GatedStepUnregistered {
		t.Fatalf("after the refused registration %s's spec step = %+v, want unregistered", root.Key, step)
	}
	r.note("the daemon recorded no gate row and no gate fact of %s, and its spec step is unregistered", root.Key)
}

// readsTheBasis fails unless the tool's Dispatch requests read basis, where its reference
// dispatch://<basis>/spec is looked up.
func (r *registerGateBasisLive) readsTheBasis(answer registerGateAnswer, basis scratchIssue) {
	r.t.Helper()
	read := "GET /api/v1/issues/" + basis.Key
	if !slices.ContainsFunc(answer.DispatchRequests, func(request string) bool {
		return request == read || strings.HasPrefix(request, read+"/") || strings.HasPrefix(request, read+"?")
	}) {
		r.t.Fatalf("the tool's Dispatch requests %q read nothing of %s, where its basis reference dispatch://%s/spec is looked up", answer.DispatchRequests, basis.Key, basis.Key)
	}
}

// namesTheAncestor fails unless message, the one the daemon posted on root when its step opened,
// names ancestor on the line scripts/dispatch/cross-project-basis-census.ts reads it from.
func (r *registerGateBasisLive) namesTheAncestor(root, ancestor scratchIssue, message string) {
	r.t.Helper()
	if !strings.Contains(message, "\n- The ancestor is "+ancestor.Key+".") {
		r.t.Fatalf("the gate-opened message on %s does not name %s as the ancestor:\n%s", root.Key, ancestor.Key, message)
	}
}

// call is the register_gate call of root's architect: root's primary document by its reference at
// version, the spec step, and the basis dispatch://<basis>/spec at basisVersion with reason.
func (r *registerGateBasisLive) call(root, basis scratchIssue, version, basisVersion int, reason string) map[string]any {
	return map[string]any{
		"op": "register_gate", "issue": root.Key, "artifactId": "spec", "version": version, "step": string(record.DesignStepSpec),
		"basis": "dispatch://" + basis.Key + "/spec", "basisVersion": basisVersion, "reason": reason,
	}
}

// tool runs one call of the legion tool as root's architect, through the driver under bun in the
// tool's checkout, and answers what the driver printed. The claim's session and secret go on
// stdin, never on a command line. Nothing of the caller's environment reaches bun but PATH: its
// home and temporary directory are the run's own.
func (r *registerGateBasisLive) tool(root scratchIssue, call map[string]any) registerGateAnswer {
	t := r.t
	t.Helper()
	architect, ok := r.architects[root.Key]
	if !ok {
		t.Fatalf("no architect of %s was launched and registered", root.Key)
	}
	input, err := json.Marshal(map[string]any{
		"daemonUrl": r.d.base, "dispatchUrl": r.baseURL, "dispatchToken": scratchAgentToken,
		"claim": map[string]any{
			"sessionID": architect.session, "tree": root.Key, "issue": root.Key, "role": "architect",
			"secret": architect.secret, "claimToken": string(architect.launch.Claim), "generation": architect.launch.Generation,
		},
		"call": call,
	})
	if err != nil {
		t.Fatalf("encode the tool's input: %v", err)
	}
	shown, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("encode the call: %v", err)
	}
	r.note("legion %s", shown)
	pane := filepath.Join(r.toolRoot, "packages", "pi-legion")
	ctx, cancel := context.WithTimeout(t.Context(), registerGateToolDeadline)
	defer cancel()
	command := exec.CommandContext(ctx, "bun", filepath.Join(pane, "test", "register-gate-basis-driver.ts"))
	// A child bun leaves holding the output pipes would hold Run past the kill; this bounds that.
	command.WaitDelay = time.Second
	command.Dir = pane
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.home,
		"TMPDIR=" + r.home,
		"XDG_CONFIG_HOME=" + filepath.Join(r.home, "config"),
		"XDG_STATE_HOME=" + filepath.Join(r.home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(r.home, "cache"),
	}
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("the tool's driver in %s failed: %v (deadline %s: %v)\nstdout %s\nstderr %s", pane, err, registerGateToolDeadline, ctx.Err(),
			stdout.Bytes(), stderr.Bytes())
	}
	var answer registerGateAnswer
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		t.Fatalf("decode the tool's driver's answer %s: %v\nstderr %s", stdout.Bytes(), err, stderr.Bytes())
	}
	r.note("the tool answered (isError %t):\n%s", answer.IsError, answer.Text)
	r.note("its requests to the daemon: %q", answer.DaemonRequests)
	r.note("its requests to Dispatch: %q", answer.DispatchRequests)
	return answer
}

// saveEventLog writes the scratch Dispatch's whole event log to path: every page of GET
// /api/v1/events/log from the first event, each event on a line of its own exactly as the route
// served it. It runs as a cleanup, so a failure is an error of the test and leaves no file. An
// empty log, or one whose ids do not rise, is a failure: the run made events, and the route pages
// by id.
func (r *registerGateBasisLive) saveEventLog(path string) {
	t := r.t
	var log bytes.Buffer
	var after int64
	events := 0
	for {
		page := fmt.Sprintf("/api/v1/events/log?after=%d&limit=%d", after, registerGateEventLogPage)
		status, answer := dispatchAnswerAs(t, r.baseURL, http.MethodGet, page, nil, asSession)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d: %s", page, status, answer)
			return
		}
		var served []json.RawMessage
		if err := json.Unmarshal(answer, &served); err != nil {
			t.Errorf("decode the page GET %s: %v", page, err)
			return
		}
		if len(served) == 0 {
			break
		}
		for _, event := range served {
			var read struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(event, &read); err != nil || read.ID <= after {
				t.Errorf("GET %s served the event %s after the id %d (%v), want ids that rise", page, event, after, err)
				return
			}
			if bytes.ContainsAny(event, "\r\n") {
				t.Errorf("GET %s served the event %d on more than one line: %s", page, read.ID, event)
				return
			}
			log.Write(event)
			log.WriteByte('\n')
			after = read.ID
			events++
		}
	}
	if events == 0 {
		t.Errorf("GET /api/v1/events/log served no event, though the run made some")
		return
	}
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		t.Errorf("write the scratch Dispatch's event log: %v", err)
		return
	}
	r.note("the scratch Dispatch's event log: %d events, ids up to %d, written to %s", events, after, path)
}
