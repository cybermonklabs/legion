//go:build e2e

// The live proof's driver for dispatch://INFRA-3320's check k8 (R1): a root architect's own
// `legion` tool makes the registration R1 takes. scripts/e2e/register-gate-basis.sh builds this
// driver with `-tags e2e` and owns the nats-server it runs against, which it names in
// LEGION_E2E_NATS_URL; the PostgreSQL is LEGION_TEST_PG_DSN's.
//
// The rig is the person-handover proofs' (person_handover_scratch_dispatch_test.go): the real
// Dispatch server this checkout builds, on a database of its own, publishing its events on that
// broker, and the daemon in this process reading them, with a person's act a request with the
// trusted identity header. The tool is the one packages/pi-legion builds: a bun process
// (packages/pi-legion/test/register-gate-basis-driver.ts) builds createLegionTool with the plugin's
// own daemon client at the daemon's API and envoy-client's DispatchClient at the scratch Dispatch,
// and holds the claim the daemon registered for R's architect, as R's pane holds it, so the tool
// mints each grant from that claim's secret in its own process. Every assertion is a read of what
// the tool answered and the requests it sent, of GET /legion/v1/state, of the daemon's own store,
// or of Dispatch.
//
// What is stood in for lies outside the registration: GitHub (blockersGitHub), the Apps' tokens,
// the runtime (the package's fake runtime launches R's architect, and the rig registers its
// session as the plugin's boot does), and Envoy's listener, which nothing answers, so every notice
// stays in the daemon's outbox to be read.
package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
)

// registerGateToolRootEnv names the checkout whose packages/pi-legion tool the run drives, its
// driver file included; unset, the checkout the run builds from. The script sets it from
// REGISTER_GATE_BASIS_TOOL_ROOT, which k8's negative control points at the commit the branch starts
// from.
const registerGateToolRootEnv = "LEGION_E2E_TOOL_ROOT"

// registerGateReason is the architect's reason the tool's calls carry.
const registerGateReason = "R's one check serves P2 of P, whose spec the person approved at its latest version; R's spec holds no requirement of its own."

// registerGateBasisLive is one run of the proof: the rig, the checkout whose tool runs, the home
// bun runs in, and the world the steps build.
type registerGateBasisLive struct {
	*personHandoverRig
	toolRoot string
	home     string
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

// TestRegisterGateBasisToolLive drives the proof's steps against one booted daemon and one
// Dispatch.
func TestRegisterGateBasisToolLive(t *testing.T) {
	r := newRegisterGateBasisLive(t)
	r.step("p-is-approved-with-requirement-blocks-and-r-is-a-view-under-it", r.world)
	r.step("the-tool-registers-rs-spec-step-on-ps-approval-and-the-daemon-records-the-basis", r.registers)
	r.step("with-a-decision-block-open-in-rs-spec-the-tool-refuses-before-the-daemon-and-nothing-is-recorded", r.refusesOverAnOpenBlock)
}

// newRegisterGateBasisLive starts Dispatch on the script's broker and boots the daemon on the rig,
// with a cap of two: R takes one slot, and P is never admitted.
func newRegisterGateBasisLive(t *testing.T) *registerGateBasisLive {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker for Dispatch's events and the daemon's intake; scripts/e2e/register-gate-basis.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	// testbuild.RootEnv names the checkout the script runs from, whose packages/envoy the rig
	// builds Dispatch from: the driver binary runs from the caller's directory, not the package's.
	// Unset (a plain `go test -tags e2e`, which runs in the package directory), the checkout is four
	// directories up.
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
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatalf("bun is not on PATH, and the legion tool runs under it: %v", err)
	}
	board := scratchDispatchOptions{natsURL: natsURL, root: root}
	daemonLog := os.Getenv(liveDaemonLogEnv)
	if daemonLog != "" {
		file, err := os.OpenFile(filepath.Join(filepath.Dir(daemonLog), "dispatch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open Dispatch's log: %v", err)
		}
		t.Cleanup(func() { file.Close() })
		board.log = file
	}
	r := &registerGateBasisLive{personHandoverRig: newPersonHandoverRigOn(t, board, 2), toolRoot: toolRoot, home: t.TempDir()}
	if daemonLog != "" {
		// The rig keeps the daemon's log in memory, where its proofs read it. Registered before the
		// boot, this cleanup runs after the daemon has stopped, so the file holds the whole log.
		t.Cleanup(func() {
			if err := os.WriteFile(daemonLog, []byte(r.logs.String()), 0o600); err != nil {
				t.Errorf("write the daemon's log: %v", err)
			}
		})
	}
	r.boot()
	r.note("Dispatch %s (database of its own, events on %s), project %s; daemon %s; the tool of %s", r.baseURL, natsURL, r.cfg.Project, r.d.base, toolRoot)
	return r
}

// step runs one of the proof's steps between its `== name` and `ok name (<s>s)` lines, as the
// repository's live proofs print them.
func (r *registerGateBasisLive) step(name string, body func()) {
	r.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// note prints what a step drove or read, under its heading.
func (r *registerGateBasisLive) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// Step 1. R, labelled by a session, is set todo by the person and admitted, and its architect is
// launched and registered: R runs as a root of its own. P, labelled by a session and in progress,
// so the daemon records nothing of it, marks p1 to p3 in its primary document, and the person
// approves its latest version. The person sets P as R's parent, and R's one check serves P2.
func (r *registerGateBasisLive) world() {
	t := r.t
	t.Helper()
	labelled := map[string]any{"labels": []string{"legion"}}
	r.root = r.create(asSession, "Ledger export rewrite", labelled)
	r.admit(r.root.Key)
	r.parent = r.create(asSession, "Quarterly billing overhaul", map[string]any{"labels": []string{"legion"},
		"spec": requirementsMarkdown("Billing", "p1", "p2", "p3")})
	r.setStatus(r.parent.Key, "in_progress")
	r.approved = r.approve(r.parent.PrimaryArtifactID).Version
	r.linkUnder(r.root, r.parent, false)
	r.writeDocument(r.root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(r.parent.Key, "p2")}}))
	r.quiet(r.root.Key, r.parent.Key)
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
// on P's approval (its gate row's basis, the state's open_on_basis) and R plans.
func (r *registerGateBasisLive) registers() {
	t := r.t
	t.Helper()
	answer := r.tool(r.call(1))
	if answer.IsError || !slices.Equal(answer.DaemonRequests, []string{"POST /legion/v1/grants", "POST /legion/v1/gates/register"}) {
		t.Fatalf("the tool's registration of %s on %s answered %+v, want a success after one grant and one registration sent to the daemon", r.root.Key, r.parent.Key, answer)
	}
	parentRead := "GET /api/v1/issues/" + r.parent.Key
	if !slices.ContainsFunc(answer.DispatchRequests, func(request string) bool {
		return request == parentRead || strings.HasPrefix(request, parentRead+"/") || strings.HasPrefix(request, parentRead+"?")
	}) {
		t.Fatalf("the tool's Dispatch requests %q read nothing of %s, where its basis reference dispatch://%s/spec is looked up", answer.DispatchRequests, r.parent.Key, r.parent.Key)
	}
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
	version := r.editDocument(r.root.PrimaryArtifactID, map[string]string{"op": "insert", "after": "end", "markdown": personHandoverBlock})
	ask := r.blockAsk(r.root.Key, "wave")
	// The daemon's own writes to Dispatch for R run first, so nothing of them lands between the
	// two snapshots.
	r.dispatchRowsDone(r.root.Key)
	r.quiet(r.root.Key)
	before := r.snapshot(r.root.Key)
	r.note("version %d of %s's spec holds decision block wave, open as ask %s; the daemon records %s in %s with %d gate facts",
		version, r.root.Key, ask, r.root.Key, before.phase, before.gateFacts)
	answer := r.tool(r.call(version))
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

// call is the register_gate call of R's architect: R's primary document by its reference at
// version, the spec step, and the basis dispatch://<P>/spec at P's approved version with the
// reason.
func (r *registerGateBasisLive) call(version int) map[string]any {
	return map[string]any{
		"op": "register_gate", "issue": r.root.Key, "artifactId": "spec", "version": version, "step": string(record.DesignStepSpec),
		"basis": "dispatch://" + r.parent.Key + "/spec", "basisVersion": r.approved, "reason": registerGateReason,
	}
}

// tool runs one call of the legion tool as R's architect, through the driver under bun in the
// tool's checkout, and answers what the driver printed. The claim's session and secret go on
// stdin, never on a command line. Nothing of the caller's environment reaches bun but PATH: its
// home and temporary directory are the run's own.
func (r *registerGateBasisLive) tool(call map[string]any) registerGateAnswer {
	t := r.t
	t.Helper()
	architect, ok := r.architects[r.root.Key]
	if !ok {
		t.Fatalf("no architect of %s was launched and registered", r.root.Key)
	}
	input, err := json.Marshal(map[string]any{
		"daemonUrl": r.d.base, "dispatchUrl": r.baseURL, "dispatchToken": scratchAgentToken,
		"claim": map[string]any{
			"sessionID": architect.session, "tree": r.root.Key, "issue": r.root.Key, "role": "architect",
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
	command := exec.Command("bun", filepath.Join(pane, "test", "register-gate-basis-driver.ts"))
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
		t.Fatalf("the tool's driver in %s failed: %v\nstdout %s\nstderr %s", pane, err, stdout.Bytes(), stderr.Bytes())
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
