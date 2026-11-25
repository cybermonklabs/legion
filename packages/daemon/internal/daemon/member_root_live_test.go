//go:build e2e

// The live proof of k32's relaunch (decision 13 of dispatch://INFRA-4730, R22 of
// dispatch://INFRA-3320): a member whose own architect runs, launched under its gate root, becomes
// a root by a change of parent; the daemon relaunches that architect in its session, the
// relaunch's LEGION_TREE and its registration's answer name the member itself, and the legion
// tool of this checkout and the tool of the oldest plugin contract in the fleet each register the
// member's own gate, which neither would before the relaunch. scripts/e2e/member-becomes-root.sh
// builds this driver with `-tags e2e`, owns the nats-server it runs against (LEGION_E2E_NATS_URL),
// and makes the old plugin's root (scripts/e2e/lib/old-plugin-root.sh), which it names in
// LEGION_E2E_OLD_TOOL_ROOT; the PostgreSQL is LEGION_TEST_PG_DSN's.
//
// The rig is the person-handover proofs' (person_handover_scratch_dispatch_test.go): the real
// Dispatch server this checkout builds, on a database of its own, publishing its events on that
// broker, and the daemon in this process reading them. The tools are run under bun by
// packages/pi-legion/test/gate-holder-driver.ts, copied into the old root, each holding the claim
// as the plugin's own registration answered it. A change of parent made by unlink_children is not
// driven here: that route is the API's (dispatch://INFRA-4730's t9), and the relaunch it makes is
// the same pass's.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// memberRootDriver is the driver every tool root runs, relative to the root.
const memberRootDriver = "packages/pi-legion/test/gate-holder-driver.ts"

// memberRootTools reads what each live proof here needs, and fails naming what is missing rather
// than skipping: the broker for Dispatch's events and the daemon's intake, this checkout's root,
// bun, and each tool root with its node_modules and the driver. It answers the broker's URL, the
// checkout's root and the two tools.
func memberRootTools(t *testing.T) (string, string, []memberTool) {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Fatalf("%s is unset, so there is no broker for Dispatch's events and the daemon's intake; scripts/e2e/member-becomes-root.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	root := os.Getenv(testbuild.RootEnv)
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := os.Getenv(treeOldToolRootEnv)
	if oldRoot == "" {
		t.Fatalf("%s is unset; scripts/e2e/member-becomes-root.sh makes the oldest contract's plugin root and names it there", treeOldToolRootEnv)
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatalf("bun is not on PATH, and the legion tool runs under it: %v", err)
	}
	tools := []memberTool{{name: "this checkout's tool", root: root}, {name: "the oldest contract's tool", root: oldRoot}}
	for _, tool := range tools {
		for _, need := range []string{"node_modules", memberRootDriver} {
			if _, err := os.Stat(filepath.Join(tool.root, need)); err != nil {
				t.Fatalf("%s at %s has no %s: %v", tool.name, tool.root, need, err)
			}
		}
	}
	return natsURL, root, tools
}

// memberRootRig boots the person-handover rig the live proofs here share, over the broker and the
// checkout memberRootTools read, with the supervision sweep a relaunch rides each second, and the
// daemon's log written where the script names it.
func memberRootRig(t *testing.T, natsURL, root string) *personHandoverRig {
	t.Helper()
	r := newPersonHandoverRigOn(t, scratchDispatchOptions{natsURL: natsURL, root: root}, 2)
	// The pass that relaunches a claim under its issue's gate root rides the supervision sweep.
	r.tune = func(o *overrides) { o.orphanSweep = time.Second }
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		// Registered before the boot, this cleanup runs after the daemon has stopped, so the file
		// holds the whole log the rig kept in memory. Each proof of a run appends its own daemon's
		// log under its name.
		t.Cleanup(func() {
			file, err := os.OpenFile(daemonLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, err = fmt.Fprintf(file, "== %s\n%s", t.Name(), r.logs.String())
				err = errors.Join(err, file.Close())
			}
			if err != nil {
				t.Errorf("write the daemon's log: %v", err)
			}
		})
	}
	r.boot()
	return r
}

// TestAMemberThatBecomesARootIsRelaunchedAndRegisters drives the proof's steps against one booted
// daemon and one Dispatch. It needs bun and each tool root's node_modules, and fails naming what
// is missing rather than skipping.
func TestAMemberThatBecomesARootIsRelaunchedAndRegisters(t *testing.T) {
	natsURL, root, tools := memberRootTools(t)
	r := memberRootRig(t, natsURL, root)
	home := t.TempDir()

	// The world: P, a root at work whose design gate the person opened, and M, its child,
	// released by P's architect, whose first phase start launches M's own architect under P, M's
	// gate root.
	labelled := map[string]any{"labels": []string{"legion"}}
	parent := r.create(asSession, "Harbour crane retrofit", labelled)
	grant := r.admit(parent.Key)
	r.registerApprovedGate(parent.Key, parent.PrimaryArtifactID, r.approve(parent.PrimaryArtifactID).Version, grant)
	member := r.create(asSession, "Quay lighting survey", map[string]any{"parent": parent.Key})
	r.setStatus(member.Key, "backlog")
	r.quiet(member.Key)
	status, answer := r.d.request(http.MethodPost, "/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: grant(), Issues: []string{member.Key}}, false)
	var released api.WaveReleaseResponse
	if status != http.StatusOK || json.Unmarshal(answer, &released) != nil || !slices.Contains(released.Released, member.Key) {
		t.Fatalf("release_children of %s = %d %s, want it released", member.Key, status, answer)
	}
	// M takes the second slot and starts its first phase, and its own architect with it.
	architect := r.architectToken(member.Key)
	first := r.awaitLaunch(architect, 0)
	if first.Tree != parent.Key {
		t.Fatalf("the first launch of %s names LEGION_TREE=%q, want its gate root %s", architect, first.Tree, parent.Key)
	}
	before := memberRegister(t, r, first)
	if before.Tree != parent.Key {
		t.Fatalf("the registration of %s answered tree %q, want %s", architect, before.Tree, parent.Key)
	}
	call := map[string]any{"op": "register_gate", "issue": member.Key, "artifactId": member.PrimaryArtifactID, "version": 1,
		"step": string(record.DesignStepSpec)}
	for _, tool := range tools {
		told := memberToolCall(t, tool, home, r, before, member.Key, call)
		if !told.IsError || len(told.DaemonRequests) != 0 || !strings.Contains(told.Text, parent.Key) {
			t.Fatalf("%s's register_gate of %s under %s answered %+v, want it refused naming %s with nothing sent to the daemon", tool.name, member.Key, parent.Key, told, parent.Key)
		}
	}

	// The change of parent: the person takes M out from under P, and M is a root.
	launched := r.launches(string(architect))
	r.patch(member.Key, asPerson, map[string]any{"parent": nil})
	second := r.awaitLaunch(architect, launched)
	if second.Tree != member.Key || second.ResumeSessionFile == "" {
		t.Fatalf("the relaunch of %s names LEGION_TREE=%q resuming %q, want %s in its session", architect, second.Tree, second.ResumeSessionFile, member.Key)
	}
	after := memberRegister(t, r, second)
	if after.Tree != member.Key {
		t.Fatalf("the relaunch's registration of %s answered tree %q, want %s", architect, after.Tree, member.Key)
	}
	for _, tool := range tools {
		told := memberToolCall(t, tool, home, r, after, member.Key, call)
		if told.IsError || !slices.Contains(told.DaemonRequests, "POST /legion/v1/gates/register") {
			t.Fatalf("%s's register_gate of %s, a root now, answered %+v, want it taken", tool.name, member.Key, told)
		}
	}
	if gate := r.gates(member.Key)[record.DesignStepSpec]; gate.ArtifactID != member.PrimaryArtifactID || gate.LatestVersion != 1 {
		t.Fatalf("the daemon records %s's spec step as %+v, want version 1 of %s", member.Key, gate, member.PrimaryArtifactID)
	}
}

// TestAGateHolderUnderAParentRegistersItsOwnGate drives k32's gate holder: M, a root its own
// architect runs, which holds its own design gate, is set by a person under P, a root at work.
// M keeps its own gate (issues.holds_gate: an issue that ran as a root holds it, R15 of
// dispatch://INFRA-3320), so its gate root is itself and its pod and LEGION_TREE stay (R17).
// Relaunched - its process lost and launched again in its session - its registration names M as
// its tree; it edits its spec, and each plugin's legion tool, this checkout's and the oldest
// contract's in the fleet, registers M's own gate at the new version, which the daemon takes.
func TestAGateHolderUnderAParentRegistersItsOwnGate(t *testing.T) {
	natsURL, root, tools := memberRootTools(t)
	r := memberRootRig(t, natsURL, root)
	home := t.TempDir()

	labelled := map[string]any{"labels": []string{"legion"}}
	parent := r.create(asSession, "Lighthouse beacon overhaul", labelled)
	r.admit(parent.Key)
	member := r.create(asSession, "Breakwater tide logger", labelled)
	r.admit(member.Key)
	architect := r.architectToken(member.Key)
	first := r.awaitLaunch(architect, 0)
	if first.Tree != member.Key {
		t.Fatalf("the first launch of %s names LEGION_TREE=%q, want its own %s", architect, first.Tree, member.Key)
	}
	memberRegister(t, r, first)

	// The person sets M under P, a root at work: M holds its own gate still.
	r.patch(member.Key, asPerson, map[string]any{"parent": parent.Key})
	r.quiet(member.Key)

	// M's architect's process is lost, and the daemon launches it again in its session.
	launched := r.launches(string(architect))
	var lost *runtime.Locator
	for _, c := range r.d.claims() {
		if c.Token == architect {
			lost = c.Locator
		}
	}
	if lost == nil {
		t.Fatalf("the daemon holds no process of %s to lose", architect)
	}
	r.rt.Emit(runtime.Observation{Locator: *lost, Kind: runtime.Gone})
	second := r.awaitLaunch(architect, launched)
	if second.Tree != member.Key {
		t.Fatalf("the relaunch of %s names LEGION_TREE=%q, want its own gate root %s", architect, second.Tree, member.Key)
	}
	after := memberRegister(t, r, second)
	if after.Tree != member.Key {
		t.Fatalf("the relaunch's registration of %s answered tree %q, want %s", architect, after.Tree, member.Key)
	}

	version := r.editDocument(member.PrimaryArtifactID, insertParagraph("The logger samples every ten minutes."))
	call := map[string]any{"op": "register_gate", "issue": member.Key, "artifactId": member.PrimaryArtifactID, "version": version,
		"step": string(record.DesignStepSpec)}
	for _, tool := range tools {
		told := memberToolCall(t, tool, home, r, after, member.Key, call)
		if told.IsError || !slices.Contains(told.DaemonRequests, "POST /legion/v1/gates/register") {
			t.Fatalf("%s's register_gate of %s, its own gate under %s, answered %+v, want it taken", tool.name, member.Key, parent.Key, told)
		}
	}
	if gate := r.gates(member.Key)[record.DesignStepSpec]; gate.ArtifactID != member.PrimaryArtifactID || gate.LatestVersion != version {
		t.Fatalf("the daemon records %s's spec step as %+v, want version %d of %s", member.Key, gate, version, member.PrimaryArtifactID)
	}
}

// memberTool is one tool root the proof drives.
type memberTool struct{ name, root string }

// awaitLaunch waits for the launch of token after the first launched of its launches, and answers
// its spec.
func (r *personHandoverRig) awaitLaunch(token claim.Token, launched int) runtime.SpawnSpec {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("launch %d of %s", launched+1, token), func() bool {
		return r.launches(string(token)) > launched
	})
	return lastLaunch(r.t, r.rt, token)
}

// memberRegister registers launch's session as the plugin's boot does, and answers the daemon's
// registration, whose tree the plugin keeps as its claim's (packages/pi-shared's claim core). A
// relaunch in its session registers the agent session the claim's first launch registered, as the
// resumed agent does.
func memberRegister(t *testing.T, r *personHandoverRig, launch runtime.SpawnSpec) claim.RegisterResponse {
	t.Helper()
	session := "ses_" + string(launch.Claim)
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion(),
	}, false)
	var registered claim.RegisterResponse
	if status != http.StatusOK || json.Unmarshal(body, &registered) != nil {
		t.Fatalf("register the session of %s = %d; body %s", launch.Claim, status, body)
	}
	r.architects[registered.Issue] = rigArchitect{launch: launch, session: session, secret: registered.Secret}
	return registered
}

// memberToolCall runs call through tool's driver under bun as the architect registered holds the
// claim, and answers what the driver printed. Nothing of the caller's environment reaches bun but
// PATH: its home and temporary directory are the run's own.
func memberToolCall(t *testing.T, tool memberTool, home string, r *personHandoverRig, registered claim.RegisterResponse,
	issue string, call map[string]any) registerGateAnswer {
	t.Helper()
	architect := r.architects[issue]
	input, err := json.Marshal(map[string]any{
		"daemonUrl": r.d.base, "dispatchUrl": r.baseURL, "dispatchToken": scratchAgentToken,
		"claim": map[string]any{
			"sessionID": architect.session, "tree": registered.Tree, "issue": registered.Issue, "role": "architect",
			"secret": registered.Secret, "claimToken": string(registered.ClaimToken), "generation": registered.Generation,
		},
		"call": call,
	})
	if err != nil {
		t.Fatalf("encode the tool's input: %v", err)
	}
	stdout, stderr := runBunDriver(t, tool.name+"'s driver", tool.root, memberRootDriver, home, registerGateToolDeadline, input)
	var answer registerGateAnswer
	if err := json.Unmarshal(stdout, &answer); err != nil {
		t.Fatalf("decode %s's driver's answer %s: %v\nstderr %s", tool.name, stdout, err, stderr)
	}
	t.Logf("%s, holding the claim under tree %s, answered (isError %t): %s; daemon requests %q", tool.name, registered.Tree, answer.IsError, answer.Text, answer.DaemonRequests)
	return answer
}
