//go:build e2e

// The live proof that an experiment arm's folder of texts replaces Legion's own texts for the roles
// its experiment reaches, and that a folder that fails a check is run as no arm at all
// (dispatch://INFRA-5590, its checks e0e84423, A1, and e0b617c0, A2).
// scripts/e2e/texts-folder.sh builds this driver with `-tags e2e` and owns the nats-server and the
// PostgreSQL it runs against. TEXTS_FOLDER_CASES names the cases a run makes, `accepted` when it
// names none, and each case is a subtest of TestTextsFolderLive with a daemon, a database, a
// Dispatch project and panes of its own.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonWithin), with dispatch_url at the real Dispatch this checkout
// builds (startHandoffDispatch, on a database of its own and its token in every pane, as
// late_completion_live_test.go runs it). Each pane is a real `omp --mode rpc` under the real
// `legion worker-shim`, started by localruntime_live_test.go's runtime, which prepares a launch of a
// folder through the tmux runtime's own helper, with this checkout's two plugins installed in an Oh
// My Pi profile of the run's own (installProofPlugins). What stands in is each pane's model, a
// stand-in of this driver's own per claim (folderProofStandin), GitHub (blockersGitHub), the two
// Apps' tokens and Envoy's listener.
//
// The project declares one experiment, `texts`, at mode on, reaching the architect and the
// reviewer and leaving the planner out on purpose, whose arm `folder` names the folder and whose
// arm `control` names nothing. Each tree is admitted through the workflow, so its draw records the
// arm against the tree's root: its root is created on Dispatch with the legion label at todo before
// the daemon boots, and the boot's listing admits it and launches its architect (an operator's
// spawn of a tree draws nothing). The root's child is then recorded in the store, as
// role_prompt_rule_live_test.go records its child: a child of the root in that tree, in planning on
// a slot of its own. The child's own architect, the child's reviewer and the root's planner are the
// operator's spawns of claims in that drawn tree, each with a task, and each launch reads the arm
// the tree's root was drawn into (experiment.Launch.Arms over the tree's recorded assignment), as
// the workflow's own starts do. A release by the root architect's `legion` tool would need GitHub
// Apps, a webhook App and a repository answer this proof has none of (the daemon reads the
// repository as one whose answer was bad), and a workflow start of reviewing needs an implementer's
// pushed round on an open pull request (plan.Validate refuses a reviewer step with no implementer
// step before it); every launch is built by the same spec builder whichever started it
// (specs.armTexts).
//
// What a pane read is read from the first request its own Oh My Pi sends its model stand-in,
// through omprequest.Read, the reader the real Oh My Pi's captured requests hold: the system
// prompt, the skill list, the `task` tool's agent list and the `<dispatch-first-skill>` blocks.
// The arms a pane's model calls name are read from its Oh My Pi's own OTEL_RESOURCE_ATTRIBUTES, in
// /proc/<pid>/environ, the pid as ompChildOf finds it. A notice is read from the real Dispatch
// (dispatch.HTTPClient.MessageBodiesSince) and the daemon's decisions from its own log
// (recordingLog).
//
// Everything that names the new key, the texts package or any other symbol this change added is in
// texts_folder_arm_live_test.go: the folder, its manifest, the boot's plugin gate and its line of
// shipped names. The negative control builds this file over a tree of main with
// texts_folder_arm_control_live_test.go in its place, which gives the folder's roles/reviewer.md as
// the arm's instructions file instead: there the reviewer's request holds the folder's marker
// beside the sentence the folder takes out, and the proof fails at reviewer-request, the first
// check of the accepted case. The two files define the four functions this one calls,
// folderProofArm, folderProofOverrides, folderProofBootRead and folderProofBreak, with one
// signature each.
//
// Each step prints `== <name>`, what it drove and read, and `ok <name>`. A check that does not
// hold prints `FAIL <name>: <why>` and ends its case.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/texts/omprequest"
)

const (
	// folderProofScript is the script that builds and runs this driver.
	folderProofScript = "scripts/e2e/texts-folder.sh"
	// folderProofCasesEnv names the cases a run makes, by their names, joined by commas.
	folderProofCasesEnv = "TEXTS_FOLDER_CASES"
	// folderProofExperiment and its two arms, as the project declares them.
	folderProofExperiment  = "texts"
	folderProofArmName     = "folder"
	folderProofControlName = "control"
	// folderProofRemoved is the sentence of Legion's roles/reviewer.md the folder's copy takes out.
	folderProofRemoved = "because the change is reviewed against the plan as it stands then"
	// The daemon's own lines for a launch of an arm's folder of texts (internal/daemon/texts.go and
	// daemon.go), each a literal here so a reworded line fails this proof.
	folderProofLaunched    = "legion daemon launches with an arm's folder of texts"
	folderProofFellBack    = "legion daemon launched Legion's own texts in place of an arm's folder of texts"
	folderProofNoticeAgain = "legion daemon applied no second notice of a launch that ran Legion's own texts in place of an arm's folder of texts: its claim and cause were noticed already"
	// folderProofNoticed is what every notice of a fallback says on the issue (workflow's
	// textsFellBack).
	folderProofNoticed = "ran Legion's own texts in place of the folder of texts"
	// folderProofWaiting is what a stand-in answers a turn it makes no call in: a WAITING line,
	// which the phase-stall reminder takes as a worker that said what it waits on.
	folderProofWaiting = "WAITING on the daemon's next task."
	// folderProofTurns bounds the wait for a pane's turns once it is ready.
	folderProofTurns = 3 * time.Minute
	// folderProofBootBound bounds a boot, the plugin gate's discovery among it.
	folderProofBootBound = 5 * time.Minute
)

// The stand-in providers, one per claim this proof reads, and the spare every other claim talks to.
const (
	folderProofRootArchitect    = "rootarchitect"
	folderProofRootPlanner      = "rootplanner"
	folderProofChildArchitect   = "childarchitect"
	folderProofChildReviewer    = "childreviewer"
	folderProofControlArchitect = "controlarchitect"
	folderProofControlPlanner   = "controlplanner"
	folderProofSpare            = "sparepane"
)

// folderProofCase is one case of the proof: its name, and for a case whose folder fails a check
// what the reviewer's launch falls back for: the cause the daemon's log and the issue's notice
// name, and the file, "" for the folder as a whole. afterStart says the folder is changed on disk
// only once a launch accepted it, since the daemon's boot refuses the change (R3).
type folderProofCase struct {
	name        string
	cause, file string
	afterStart  bool
}

// folderProofCases are every case, in the order a run of all of them makes them: A1's, then A2's
// seven.
var folderProofCases = []folderProofCase{
	{name: "accepted"},
	{name: "digest", cause: "digest", file: "roles/reviewer.md"},
	{name: "no-legion-text", cause: "no-legion-text", file: "roles/no-such-part.md"},
	{name: "unknown-agent", cause: "unknown-agent", file: "agents/goal-red-team.md"},
	{name: "unknown-skill", cause: "unknown-skill", file: "go/reviewer.md"},
	{name: "manifest-deleted", cause: "no-manifest", file: "manifest.json", afterStart: true},
	{name: "unreadable", cause: "unreadable", afterStart: true},
	{name: "unreached-part", cause: "unread", file: "roles/planner.md", afterStart: true},
}

// folderProofMarkers are the markers the folder's texts carry, one per file, each with the run's
// own suffix, so no text of Legion's can hold one.
type folderProofMarkers struct {
	architectRoot, architect, architectCommon, reviewerRole, reviewerGo, agent, reference, dispatchFirst string
}

func newFolderProofMarkers(t *testing.T) folderProofMarkers {
	t.Helper()
	suffix := randomSuffix(t)[:8]
	mark := func(what string) string { return "TFMARK-" + what + "-" + suffix }
	return folderProofMarkers{
		architectRoot: mark("roles-architect-root"), architect: mark("roles-architect"),
		architectCommon: mark("go-architect-common"), reviewerRole: mark("roles-reviewer"), reviewerGo: mark("go-reviewer"),
		agent: mark("agents-goal-red-team"), reference: mark("skills-legion-worker-plan-tests"),
		dispatchFirst: mark("skills-dispatch-first"),
	}
}

// all is every marker.
func (m folderProofMarkers) all() []string {
	return []string{m.architectRoot, m.architect, m.architectCommon, m.reviewerRole, m.reviewerGo, m.agent, m.reference, m.dispatchFirst}
}

// folderProofShared is what every case of a run shares: the checkout, the `legion` it built, the Oh
// My Pi profile with both plugins installed, the run's Dispatch, and a line of Legion's untouched
// skills/legion-worker/references/tools.md, which the reviewer reads through skill://.
type folderProofShared struct {
	root, work, legion, omp, home, profile string
	dispatch                               *handoffDispatch
	toolsLine                              string
}

// TestTextsFolderLive runs the cases TEXTS_FOLDER_CASES names, each a subtest of its own.
func TestTextsFolderLive(t *testing.T) {
	fixtureenv.Live(t)
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; %s requires it", liveOmpPathEnv, folderProofScript)
	}
	if os.Getenv(liveNATSURLEnv) == "" {
		t.Skipf("%s is unset, so there is no broker to drive the daemon's intake through; %s starts a nats-server and sets it", liveNATSURLEnv, folderProofScript)
	}
	cases := folderProofSelected(t)
	shared := newFolderProofShared(t, omp)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runFolderProofCase(t, shared, c) })
	}
}

// folderProofSelected is the cases TEXTS_FOLDER_CASES names, in its order, `accepted` alone when
// it names none; a name of no case, or one named twice, fails the run.
func folderProofSelected(t *testing.T) []folderProofCase {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(folderProofCasesEnv))
	if named == "" {
		named = folderProofCases[0].name
	}
	var picked []folderProofCase
	for _, name := range strings.Split(named, ",") {
		name = strings.TrimSpace(name)
		at := slices.IndexFunc(folderProofCases, func(c folderProofCase) bool { return c.name == name })
		if at < 0 {
			var known []string
			for _, c := range folderProofCases {
				known = append(known, c.name)
			}
			t.Fatalf("%s names the case %q, which is none of %s", folderProofCasesEnv, name, strings.Join(known, ", "))
		}
		if slices.ContainsFunc(picked, func(c folderProofCase) bool { return c.name == name }) {
			t.Fatalf("%s names the case %q twice", folderProofCasesEnv, name)
		}
		picked = append(picked, folderProofCases[at])
	}
	return picked
}

// newFolderProofShared builds what every case shares: `legion` from this checkout, both plugins
// packed and installed in a profile of the run's own, and the run's Dispatch.
func newFolderProofShared(t *testing.T, omp string) *folderProofShared {
	t.Helper()
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	fmt.Printf("   Oh My Pi %s at %s\n", strings.TrimSpace(string(version)), omp)
	root := proofCheckoutRoot(t)
	s := &folderProofShared{root: root, work: t.TempDir(), omp: omp, profile: "legion-texts-folder"}
	s.legion = buildProofLegion(t, root, s.work)
	s.home = filepath.Join(s.work, "omp-home")
	installProofPlugins(t, root, s.work, s.home, s.profile, omp)
	s.dispatch = startHandoffDispatch(t, root)
	tools := folderProofLegionText(t, root, "skills/legion-worker/references/tools.md")
	for _, line := range strings.Split(tools, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			s.toolsLine = line
			break
		}
	}
	if len(s.toolsLine) > 60 {
		s.toolsLine = s.toolsLine[:60]
	}
	if s.toolsLine == "" {
		t.Fatalf("skills/legion-worker/references/tools.md holds no line of text below its heading for the reviewer to read back")
	}
	fmt.Printf("   Dispatch %s (database of its own, NATS off); the reviewer reads back %q of Legion's tools.md\n", s.dispatch.base, s.toolsLine)
	return s
}

// folderProofLegionText is the checkout's text at path, below root.
func folderProofLegionText(t *testing.T, root, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read the checkout's %s: %v", path, err)
	}
	return string(body)
}

// folderProofPrompt is the checkout's role part at path (`roles/<p>` or `go/<p>`), the text the
// daemon this driver built embeds.
func folderProofPrompt(t *testing.T, root, path string) string {
	t.Helper()
	return folderProofLegionText(t, root, "packages/daemon/internal/prompts/"+path)
}

// folderProofAppend is text with marker after it, as a paragraph of its own.
func folderProofAppend(text, marker string) string {
	return strings.TrimRight(text, "\n") + "\n\n" + marker + "\n"
}

// folderProofReviewer is Legion's roles/reviewer.md, legion, with folderProofRemoved taken out and
// marker after it: the folder's roles/reviewer.md, and the control's instructions file. A text that
// lacks the sentence fails the run first, since the proof reads that sentence's absence.
func folderProofReviewer(t *testing.T, legion, marker string) string {
	t.Helper()
	cut := strings.Replace(legion, ", "+folderProofRemoved, "", 1)
	if cut == legion {
		cut = strings.Replace(legion, folderProofRemoved, "", 1)
	}
	if cut == legion {
		t.Fatalf("the checkout's packages/daemon/internal/prompts/roles/reviewer.md holds no sentence %q, which the folder's "+
			"roles/reviewer.md takes out: the reviewer's check reads that sentence's absence, so this proof cannot run on this text", folderProofRemoved)
	}
	return folderProofAppend(cut, marker)
}

// folderProofLive is one case: its daemon over its own database and Dispatch project, the one tree
// it admits (root and child) and, for the accepted case, the control tree it admits after a
// restart, the stand-in of each claim it reads, and the arm the experiment declares.
type folderProofLive struct {
	liveAgentProof
	shared   *folderProofShared
	c        folderProofCase
	markers  folderProofMarkers
	dispatch *handoffDispatch
	github   string
	// root is the tree's root, child its child, and control the root of the tree the accepted case
	// admits into the control arm after a restart.
	root, child, control string
	panes                map[claim.Token]*folderProofStandin
	spare                *folderProofStandin
	overlays             map[claim.Token][]string
	spareOverlays        []string
	// arm is the experiment's arm folder: a folder of texts (texts_folder_arm_live_test.go), or the
	// control's instructions file (texts_folder_arm_control_live_test.go). folder is the folder's
	// path, "" for the control's arm.
	arm    config.Variant
	folder string
	// started is when the case began, before which no notice of its project was posted.
	started time.Time
	// check is the step running, which a FAIL line names.
	check string

	mu sync.Mutex
	// runtimeFallbacks are the launches the local runtime ran their fallback for, as its hook told
	// them (folderProofOverrides).
	runtimeFallbacks []string
}

// runFolderProofCase makes one case: the tree admitted, its child recorded with its own architect
// started, and the child's reviewer started, then the case's checks.
func runFolderProofCase(t *testing.T, shared *folderProofShared, c folderProofCase) {
	r := newFolderProofLive(t, shared, c)
	r.step("boot", func() {
		r.boot()
		folderProofBootRead(r)
	})
	r.step("root-admitted", r.rootAdmitted)
	if c.afterStart {
		r.step("folder-accepted-then-changed", r.acceptedThenChanged)
	}
	r.step("child-started", r.childStarted)
	r.step("reviewer-started", r.reviewerStarted)
	if c.cause == "" {
		r.step("reviewer-request", r.reviewerRequest)
		r.step("reviewer-reads", r.reviewerReads)
		r.step("reviewer-arms", func() { r.reviewerArms(true) })
		r.step("root-architect-request", r.rootArchitectRequest)
		r.step("child-architect-request", r.childArchitectRequest)
		r.step("planner-request", r.plannerRequest)
		r.step("runtime-checked", r.runtimeChecked)
		r.step("control-tree", r.controlTree)
		return
	}
	r.step("reviewer-reads-legions-texts", r.reviewerReadsLegions)
	r.step("reviewer-arms", func() { r.reviewerArms(false) })
	r.step("fallback-logged", r.fallbackLogged)
	r.step("one-notice", r.oneNotice)
	if c.name == "digest" {
		r.step("child-architect-notice", r.childArchitectNotice)
		r.step("relaunch-posts-no-second-notice", r.relaunchPostsNoSecondNotice)
	}
}

// newFolderProofLive readies a case before its boot: its project on Dispatch with the tree's root
// at todo under the legion label, the root's child in triage, which the case records once the root
// is admitted (childStarted), and, for the accepted case's restart, the control tree's root in
// triage and unlabelled; a stand-in for each claim it reads; the profile's providers and each
// claim's overlay; the arm (folderProofArm); and the local runtime.
func newFolderProofLive(t *testing.T, shared *folderProofShared, c folderProofCase) *folderProofLive {
	t.Helper()
	proof := newLiveProof(t, folderProofScript)
	// Every case runs on the script's one broker, and its daemon leaves its consumers on the stream
	// newLiveProof creates, which the next case's create must find empty. This cleanup runs after
	// the case's daemon has stopped, since that stop is registered later.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := proof.js.DeleteStream(ctx, "ENVOY_NOTIFICATIONS"); err != nil && !errors.Is(err, jetstream.ErrStreamNotFound) {
			t.Errorf("delete the stream ENVOY_NOTIFICATIONS for the next case: %v", err)
		}
	})
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project.
	proof.cfg.Project = "TF" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	// No design gate holds the tree, so nothing waits on an approval before its roles work.
	proof.cfg.Gates.Design = config.DesignGateOff.Steps()
	// The scratch Dispatch is shared by every case; this case's view of it fails this case's test.
	scratch := *shared.dispatch.scratchDispatch
	scratch.t = t
	r := &folderProofLive{
		liveAgentProof: liveAgentProof{liveProof: proof, legion: shared.legion, omp: shared.omp},
		shared:         shared, c: c, markers: newFolderProofMarkers(t),
		dispatch: &handoffDispatch{scratchDispatch: &scratch, client: shared.dispatch.client},
		github:   blockersGitHub(t), panes: map[claim.Token]*folderProofStandin{},
		started: time.Now().Add(-time.Minute),
	}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.setUpDispatch()

	for _, pane := range r.claimPanes() {
		r.panes[pane.token] = newFolderProofStandin(t, pane.provider)
	}
	r.spare = newFolderProofStandin(t, folderProofSpare)
	t.Cleanup(r.keepRequests)
	r.panes[r.token(r.child, claim.RoleReviewer)].script = func(n int, _ []byte) []folderProofCall {
		if n > 0 {
			return nil
		}
		return []folderProofCall{
			{tool: "read", input: map[string]any{"path": "skill://legion-worker/references/plan-tests.md"}},
			{tool: "read", input: map[string]any{"path": "skill://legion-worker/references/tools.md"}},
		}
	}
	r.writeProfile()
	r.writeOverlays()

	r.rt = newLocalRuntime(t, r.cfg.StateDir, shared.legion, shared.omp, shared.home, shared.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.dispatchURL, r.rt.dispatchTokenFile = r.cfg.DispatchURL, r.cfg.DispatchTokenFile
	r.rt.overlays = func(token claim.Token) []string {
		if overlays, driven := r.overlays[token]; driven {
			return overlays
		}
		return r.spareOverlays
	}
	r.arm = folderProofArm(r)
	r.declare(folderProofArmName)
	r.daemon = newRecordingLog(r.log())
	r.note("case %s: project %s on Dispatch, tree %s with its child %s; the experiment %s at mode on, arm %s, reaching the architect and the reviewer",
		c.name, r.cfg.Project, r.root, r.child, folderProofExperiment, folderProofArmName)
	return r
}

// folderProofPane is one claim this proof reads, with the provider of its stand-in.
type folderProofPane struct {
	token    claim.Token
	provider string
}

// claimPanes are the claims this proof reads: the tree's root architect and planner, the child's
// own architect and reviewer, and the control tree's architect and planner.
func (r *folderProofLive) claimPanes() []folderProofPane {
	return []folderProofPane{
		{r.token(r.root, claim.RoleArchitect), folderProofRootArchitect},
		{r.token(r.root, claim.RolePlanner), folderProofRootPlanner},
		{r.token(r.child, claim.RoleArchitect), folderProofChildArchitect},
		{r.token(r.child, claim.RoleReviewer), folderProofChildReviewer},
		{r.token(r.control, claim.RoleArchitect), folderProofControlArchitect},
		{r.token(r.control, claim.RolePlanner), folderProofControlPlanner},
	}
}

// setUpDispatch makes the case's project on Dispatch with liveRepo its default repository, the
// tree's root at todo under the legion label, as a person hands an issue to Legion, its child in
// triage, and the control tree's root in triage with no label, which no daemon works until the
// accepted case's restart hands it over.
func (r *folderProofLive) setUpDispatch() {
	r.t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Texts folder live proof"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	r.root = r.createIssue("Ledger reconciles nightly", "")
	r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.root, map[string]any{"status": "todo", "labels": []string{"legion"}}, http.StatusOK, nil)
	r.child = r.createIssue("Billing exports quarterly totals", r.root)
	r.control = r.createIssue("Search ranks fresher pages", "")
}

// createIssue creates an issue of the case's project on Dispatch, under parent when it names one.
func (r *folderProofLive) createIssue(title, parent string) string {
	r.t.Helper()
	body := map[string]any{"project": r.cfg.Project, "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	var created struct {
		Key string `json:"key"`
	}
	r.dispatch.user(http.MethodPost, "/api/v1/issues", body, http.StatusCreated, &created)
	if created.Key == "" {
		r.t.Fatalf("Dispatch created %q with no key", title)
	}
	return created.Key
}

// declare sets the project's experiment, the arm folderProofArm made and the control, at mode on
// with on at onVariant; the daemon reads it at its next boot.
func (r *folderProofLive) declare(onVariant string) {
	r.cfg.Projects[r.cfg.Project] = config.Project{Experiments: []config.Experiment{{
		Name: folderProofExperiment, Mode: config.ExperimentOn, Unit: config.ExperimentTree,
		Roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, OnVariant: onVariant,
		Variants: []config.Variant{r.arm, {Name: folderProofControlName, Weight: 1}},
	}}}
}

// boot boots the whole daemon over the local runtime, with GitHub's stand-in, the Apps' tokens
// stood in for, and what the arm's file adds (folderProofOverrides).
func (r *folderProofLive) boot() {
	r.t.Helper()
	o := r.rt.daemonOverrides(r.rt)
	o.workflowTokens, o.githubAPI = &workflowTokenRecorder{}, r.github
	folderProofOverrides(r, &o)
	r.d = startDaemonWithin(r.t, r.cfg, o, slog.New(r.daemon), folderProofBootBound)
	r.note("the daemon answers at %s", r.cfg.DaemonURL)
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *folderProofLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// step runs one step between its `== name` and `ok name` lines, and names it in a FAIL line.
func (r *folderProofLive) step(name string, body func()) {
	r.t.Helper()
	r.check = name
	r.liveProof.step(name, body)
}

// fail prints the running step's FAIL line and ends the case with it.
func (r *folderProofLive) fail(format string, args ...any) {
	r.t.Helper()
	said := fmt.Sprintf(format, args...)
	fmt.Printf("FAIL %s: %s\n", r.check, said)
	r.t.Fatalf("%s: %s", r.check, said)
}

// runtimeFellBack records a launch the local runtime ran its fallback for (folderProofOverrides).
func (r *folderProofLive) runtimeFellBack(token, cause, file, detail string) {
	r.mu.Lock()
	r.runtimeFallbacks = append(r.runtimeFallbacks, fmt.Sprintf("%s: cause %s, file %q (%s)", token, cause, file, detail))
	r.mu.Unlock()
	fmt.Printf("   the local runtime ran the fallback of %s: cause %s, file %q (%s)\n", token, cause, file, detail)
}

// ---- the setup steps ---------------------------------------------------------------------------

// rootAdmitted waits for the boot's listing to admit the tree's root and launch its architect,
// and for that pane to report ready.
func (r *folderProofLive) rootAdmitted() {
	architect := r.token(r.root, claim.RoleArchitect)
	r.awaitPane(architect)
	r.note("the root %s was admitted through the workflow and its architect %s launched", r.root, architect)
}

// acceptedThenChanged waits for the daemon's line that the root architect's launch read the folder
// and accepted it, then changes the folder on disk as the case says (folderProofBreak), before any
// later launch reads it.
func (r *folderProofLive) acceptedThenChanged() {
	architect := r.token(r.root, claim.RoleArchitect)
	read := r.awaitLogFrom("the daemon's line that "+string(architect)+"'s launch read the folder", 0, time.Minute, func(record loggedRecord) bool {
		return record.Message == folderProofLaunched && record.Attrs["claim"] == string(architect)
	})
	r.note("%s's launch accepted the folder %v at fingerprint %v", architect, read.Attrs["folder"], read.Attrs["fingerprint"])
	folderProofBreak(r)
}

// childStarted records the root's child in the store as role_prompt_rule_live_test.go records its
// child: a child of the root in the root's tree, in planning on a slot of its own, with Dispatch
// showing it in progress under the legion label as a started child shows. Then it starts the
// child's own architect, the operator's spawn of a claim in that drawn tree with a task, and waits
// for its first turn. The child is not its tree's root, so its architect's launch composes the
// sub-architect's part (roles/architect.md) and not the root's.
func (r *folderProofLive) childStarted() {
	t := r.t
	ctx := context.Background()
	r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.child, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, nil)
	listed := r.dispatch.issue(r.child)
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.child, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.root, Parent: &r.root,
			Project: r.cfg.Project, Title: listed.Title, Phase: phase.Planning, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.child)
	}); err != nil {
		t.Fatalf("record %s, the child of %s: %v", r.child, r.root, err)
	}
	architect := r.token(r.child, claim.RoleArchitect)
	r.spawn(r.root, r.child, claim.RoleArchitect, "Reply with the word ANSWERED.")
	r.firstTurn(architect)
	r.note("recorded %s, the child of %s in its tree, in planning on a slot of its own; its own architect %s launched and answered a turn",
		r.child, r.root, architect)
}

// reviewerStarted spawns the child's reviewer as the operator spawns a claim, with a task, and
// waits for its first two turns: the first, which its stand-in answers with two `read` calls, and
// the second, which carries their results.
func (r *folderProofLive) reviewerStarted() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	r.spawn(r.root, r.child, claim.RoleReviewer, "Phase: reviewing. Read the plan-tests and tools references of the legion-worker skill, then answer.")
	r.awaitPane(reviewer)
	stand := r.panes[reviewer]
	r.await(string(reviewer)+"'s first two turns", folderProofTurns, func() bool { return stand.count() >= 2 })
	r.note("%s answered its first turn with two reads and sent their results in its second", reviewer)
}

// spawn starts role's claim on issue, in the tree rooted at tree, as the operator spawns one,
// with task.
func (r *folderProofLive) spawn(tree, issue string, role claim.Role, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{Tree: tree, Issue: issue, Role: role, Task: task}, true)
	if status != http.StatusOK && status != http.StatusCreated {
		r.fail("the operator's spawn of the %s of %s = %d %s", role, issue, status, body)
	}
}

// ---- the accepted case's checks ----------------------------------------------------------------

// reviewerRequest holds the reviewer's first request to the folder: both of the reviewer's
// markers and not the sentence the folder takes out, in that order, so a launch that read Legion's
// roles/reviewer.md beside an instructions file of the folder's text fails at the sentence; the
// arm's goal-red-team offered once, with its marker; legion-worker offered once, as the root's copy
// with no `<namespace>/legion-worker` alias, and no `<namespace>/dispatch-first` entry; and the
// arm's dispatch-first carried once.
func (r *folderProofLive) reviewerRequest() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	request := r.request(reviewer, 0)
	m := r.markers
	if !strings.Contains(request.System, m.reviewerRole) {
		r.fail("the reviewer's first request holds no marker %q of its folder's roles/reviewer.md in its system prompt", m.reviewerRole)
	}
	if strings.Contains(request.System, folderProofRemoved) {
		r.fail("the reviewer's first request holds the sentence its folder's roles/reviewer.md takes out (%q) beside that file's marker %q: "+
			"its system prompt holds Legion's roles/reviewer.md, not the folder's", folderProofRemoved, m.reviewerRole)
	}
	if !strings.Contains(request.System, m.reviewerGo) {
		r.fail("the reviewer's first request holds no marker %q of its folder's go/reviewer.md in its system prompt", m.reviewerGo)
	}
	r.note("its system prompt holds %s and %s, and not %q", m.reviewerRole, m.reviewerGo, folderProofRemoved)
	agents := folderProofAgentsNamed(request, "goal-red-team")
	if len(agents) != 1 || !strings.Contains(agents[0].Description, m.agent) {
		var listed []string
		for _, agent := range request.Agents {
			listed = append(listed, agent.Name)
		}
		r.fail("the reviewer's task tool offers goal-red-team %d times, as %+v, among the agents %v; want once, the folder's, holding %q",
			len(agents), agents, listed, m.agent)
	}
	r.note("the task tool offers goal-red-team once, its description holding %s", m.agent)
	for _, skill := range []string{"legion-worker", "dispatch-first"} {
		if aliases := folderProofAliases(request, skill); len(aliases) > 0 {
			r.fail("the reviewer's skill list offers %v, an alias of the skill %s whose files the folder holds", aliases, skill)
		}
	}
	if plain := folderProofSkillsNamed(request, "legion-worker"); len(plain) != 1 {
		r.fail("the reviewer's skill list offers legion-worker %d times; want once", len(plain))
	}
	r.note("the skill list offers legion-worker once and no <namespace>/legion-worker or <namespace>/dispatch-first entry")
	if len(request.DispatchFirst) != 1 || !strings.Contains(request.DispatchFirst[0], m.dispatchFirst) {
		r.fail("the reviewer's first request carries %d dispatch-first blocks; want one, the folder's, holding %q", len(request.DispatchFirst), m.dispatchFirst)
	}
	r.note("it carries one dispatch-first, the folder's, holding %s", m.dispatchFirst)
	for _, other := range []string{m.architectRoot, m.architect, m.architectCommon} {
		if strings.Contains(request.System, other) {
			r.fail("the reviewer's system prompt holds the architects' marker %q", other)
		}
	}
}

// reviewerReads holds the reviewer's second request to the results of its two reads: the folder's
// plan-tests.md, with its marker, and Legion's untouched tools.md of the same skill.
func (r *folderProofLive) reviewerReads() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	stand := r.panes[reviewer]
	results := folderProofResults(stand.turn(1))
	if unoffered := stand.unofferedCalls(); len(unoffered) > 0 {
		r.fail("the reviewer's requests offered no tool %v", unoffered)
	}
	var planTests, tools *folderProofResult
	for i, result := range results {
		switch stand.issuedPath(result.id) {
		case "skill://legion-worker/references/plan-tests.md":
			planTests = &results[i]
		case "skill://legion-worker/references/tools.md":
			tools = &results[i]
		}
	}
	switch {
	case planTests == nil || tools == nil:
		r.fail("the reviewer's second request carries the results %+v; want one for each read", results)
	case planTests.failed || !strings.Contains(planTests.text, r.markers.reference):
		r.fail("the read of skill://legion-worker/references/plan-tests.md answered %q (error %v); want the folder's text with %q",
			folderProofCut(planTests.text), planTests.failed, r.markers.reference)
	case tools.failed || !strings.Contains(tools.text, r.shared.toolsLine) || folderProofHoldsAny(tools.text, r.markers.all()) != "":
		r.fail("the read of skill://legion-worker/references/tools.md answered %q (error %v); want Legion's text, holding %q and no marker",
			folderProofCut(tools.text), tools.failed, r.shared.toolsLine)
	}
	r.note("the second request carries both results: plan-tests.md holding %s, and Legion's tools.md holding %q", r.markers.reference, r.shared.toolsLine)
}

// reviewerArms holds the arms the reviewer's model calls name, its Oh My Pi's own legion.arms: the
// folder's arm where the launch read the folder (folded), and no arm of the experiment where it
// fell back.
func (r *folderProofLive) reviewerArms(folded bool) {
	reviewer := r.token(r.child, claim.RoleReviewer)
	arms, pid := r.armsOf(reviewer)
	want := folderProofExperiment + ":" + folderProofArmName
	names := slices.Contains(strings.Split(arms, ","), want)
	switch {
	case folded && !names:
		r.fail("the reviewer's Oh My Pi (pid %d) names legion.arms %q; want %q among them", pid, arms, want)
	case !folded && strings.Contains(arms, folderProofExperiment+":"):
		r.fail("the reviewer's Oh My Pi (pid %d) names legion.arms %q; a launch that fell back names no arm of %s", pid, arms, folderProofExperiment)
	}
	r.note("the reviewer's Oh My Pi (pid %d) names legion.arms %q", pid, arms)
}

// rootArchitectRequest holds the root architect's first request to its folder texts: the markers
// of roles/architect-root.md and go/architect-common.md, and not roles/architect.md's.
func (r *folderProofLive) rootArchitectRequest() {
	architect := r.token(r.root, claim.RoleArchitect)
	request := r.request(architect, 0)
	m := r.markers
	if !strings.Contains(request.System, m.architectRoot) || !strings.Contains(request.System, m.architectCommon) || strings.Contains(request.System, m.architect) {
		r.fail("the root architect's first request holds %s %v, %s %v and %s %v; want the first two and not the third",
			m.architectRoot, strings.Contains(request.System, m.architectRoot), m.architectCommon, strings.Contains(request.System, m.architectCommon),
			m.architect, strings.Contains(request.System, m.architect))
	}
	r.note("the root architect's system prompt holds %s and %s", m.architectRoot, m.architectCommon)
}

// childArchitectRequest holds the child's own architect's first request to its folder texts: the
// markers of roles/architect.md and go/architect-common.md, and not roles/architect-root.md's.
func (r *folderProofLive) childArchitectRequest() {
	architect := r.token(r.child, claim.RoleArchitect)
	request := r.request(architect, 0)
	m := r.markers
	if !strings.Contains(request.System, m.architect) || !strings.Contains(request.System, m.architectCommon) || strings.Contains(request.System, m.architectRoot) {
		r.fail("the child architect's first request holds %s %v, %s %v and %s %v; want the first two and not the third",
			m.architect, strings.Contains(request.System, m.architect), m.architectCommon, strings.Contains(request.System, m.architectCommon),
			m.architectRoot, strings.Contains(request.System, m.architectRoot))
	}
	r.note("the child architect's system prompt holds %s and %s", m.architect, m.architectCommon)
}

// plannerRequest holds the root's planner, a role the experiment leaves out, of the issue whose
// architect read the folder, to Legion's texts: no marker in any request it sent, Legion's
// roles/planner.md in its system prompt, and one dispatch-first.
func (r *folderProofLive) plannerRequest() {
	planner := r.token(r.root, claim.RolePlanner)
	r.spawn(r.root, r.root, claim.RolePlanner, "Reply with the word ANSWERED.")
	r.firstTurn(planner)
	r.legionsOnly(planner, "roles/planner.md")
	r.note("the root's planner read Legion's texts: no marker in any of its %d requests, and Legion's roles/planner.md in its system prompt", r.panes[planner].count())
}

// runtimeChecked holds that the local runtime ran no fallback: every launch of the folder passed the
// check against the plugins the profile installed.
func (r *folderProofLive) runtimeChecked() {
	r.mu.Lock()
	fallbacks := slices.Clone(r.runtimeFallbacks)
	r.mu.Unlock()
	if len(fallbacks) > 0 {
		r.fail("the local runtime ran the fallback of %v", fallbacks)
	}
	launched := r.logs(func(record loggedRecord) bool { return record.Message == folderProofLaunched })
	r.note("the daemon logged %d launches of the folder and the local runtime ran no fallback", len(launched))
}

// controlTree stops the daemon and every pane, hands the control tree's root to Legion, and boots
// the daemon again with on_variant at the control arm. That tree's draw records the control arm,
// and its architect's requests and its planner's hold Legion's texts and no marker, and its
// architect's model calls name no folder arm.
func (r *folderProofLive) controlTree() {
	r.d.stop()
	r.killPanes()
	r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.control, map[string]any{"status": "todo", "labels": []string{"legion"}}, http.StatusOK, nil)
	rebindHeldPorts(r.t, &r.cfg)
	r.rt.daemonURL = r.cfg.DaemonURL
	r.declare(folderProofControlName)
	r.boot()
	r.note("the daemon was stopped, its panes ended, %s handed to Legion, and the daemon booted again with on_variant %s", r.control, folderProofControlName)
	architect := r.token(r.control, claim.RoleArchitect)
	r.firstTurn(architect)
	r.legionsOnly(architect, "roles/architect-root.md")
	arms, pid := r.armsOf(architect)
	if strings.Contains(arms, folderProofExperiment+":"+folderProofArmName) {
		r.fail("the control tree's architect's Oh My Pi (pid %d) names legion.arms %q, the folder's arm", pid, arms)
	}
	r.note("the control tree's architect read Legion's texts in each of its %d requests, and its Oh My Pi (pid %d) names legion.arms %q",
		r.panes[architect].count(), pid, arms)
	planner := r.token(r.control, claim.RolePlanner)
	r.spawn(r.control, r.control, claim.RolePlanner, "Reply with the word ANSWERED.")
	r.firstTurn(planner)
	r.legionsOnly(planner, "roles/planner.md")
	r.note("the control tree's planner read Legion's texts in each of its %d requests", r.panes[planner].count())
}

// legionsOnly holds every request token's stand-in has taken to Legion's texts: no marker in any,
// part, a role part of Legion's, in the first one's system prompt, and one dispatch-first.
func (r *folderProofLive) legionsOnly(token claim.Token, part string) {
	stand := r.panes[token]
	for n := range stand.count() {
		if marker := folderProofHoldsAny(string(stand.turn(n)), r.markers.all()); marker != "" {
			r.fail("request %d of %s holds the folder's marker %q", n+1, token, marker)
		}
	}
	request := r.request(token, 0)
	heading := strings.TrimSpace(strings.SplitN(folderProofPrompt(r.t, r.shared.root, part), "\n", 2)[0])
	if !strings.Contains(request.System, heading) {
		r.fail("the first request of %s holds no %q of Legion's %s in its system prompt", token, heading, part)
	}
	if len(request.DispatchFirst) != 1 {
		r.fail("the first request of %s carries %d dispatch-first blocks; want Legion's, once", token, len(request.DispatchFirst))
	}
}

// ---- the fallback cases' checks ----------------------------------------------------------------

// reviewerReadsLegions holds every request of the reviewer to Legion's texts: no marker in any,
// Legion's roles/reviewer.md with the sentence the folder takes out in its first, and Legion's
// dispatch-first once.
func (r *folderProofLive) reviewerReadsLegions() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	stand := r.panes[reviewer]
	for n := range stand.count() {
		if marker := folderProofHoldsAny(string(stand.turn(n)), r.markers.all()); marker != "" {
			r.fail("request %d of the reviewer holds the folder's marker %q", n+1, marker)
		}
	}
	request := r.request(reviewer, 0)
	if !strings.Contains(request.System, folderProofRemoved) {
		r.fail("the reviewer's first request holds no %q of Legion's roles/reviewer.md", folderProofRemoved)
	}
	if len(request.DispatchFirst) != 1 {
		r.fail("the reviewer's first request carries %d dispatch-first blocks; want Legion's, once", len(request.DispatchFirst))
	}
	r.note("none of the reviewer's %d requests holds a marker, and its first holds Legion's roles/reviewer.md and dispatch-first", stand.count())
}

// fallbackLogged holds the daemon's WARN line for the reviewer's launch: its claim, its generation,
// the experiment, the arm, the folder, the file (`none` for the folder as a whole) and the cause.
func (r *folderProofLive) fallbackLogged() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	line := r.fellBackLine(reviewer, 0)
	r.note("%s generation %v: %s, experiment %v, arm %v, folder %v, file %v, cause %v",
		reviewer, line.Attrs["generation"], folderProofFellBack, line.Attrs["experiment"], line.Attrs["arm"], line.Attrs["folder"], line.Attrs["file"], line.Attrs["cause"])
}

// fellBackLine is the first WARN line for token's launch after the mark from, held to the case's
// experiment, arm, folder, file and cause.
func (r *folderProofLive) fellBackLine(token claim.Token, from int) *loggedRecord {
	line := r.awaitLogFrom("the daemon's line that "+string(token)+"'s launch fell back", from, 4*time.Minute, func(record loggedRecord) bool {
		return record.Message == folderProofFellBack && record.Attrs["claim"] == string(token)
	})
	file := r.c.file
	if file == "" {
		file = "none"
	}
	generation, _ := line.Attrs["generation"].(float64)
	for _, field := range []struct {
		name string
		got  any
		want string
	}{
		{"experiment", line.Attrs["experiment"], folderProofExperiment},
		{"arm", line.Attrs["arm"], folderProofArmName},
		{"folder", line.Attrs["folder"], r.folder},
		{"file", line.Attrs["file"], file},
		{"cause", line.Attrs["cause"], r.c.cause},
	} {
		if field.got != field.want {
			r.fail("the daemon's line that %s fell back names %s %v; want %q (the line: %v)", token, field.name, field.got, field.want, line.Attrs)
		}
	}
	if generation < 1 {
		r.fail("the daemon's line that %s fell back names no generation (the line: %v)", token, line.Attrs)
	}
	return line
}

// oneNotice holds the child's issue to one notice of the reviewer's claim, naming the cause, the
// experiment, the arm, the folder and the file.
func (r *folderProofLive) oneNotice() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	notice := r.awaitOneNotice(reviewer)
	r.note("%s holds one notice of %s: %s", r.child, reviewer, notice)
}

// awaitOneNotice waits for the notice of token's claim on the child's issue, then holds that the
// issue still holds exactly one once liveSettle has passed, naming the case's cause, file, arm,
// experiment and folder; it answers that notice.
func (r *folderProofLive) awaitOneNotice(token claim.Token) string {
	r.await("the notice of "+string(token)+" on "+r.child, 2*time.Minute, func() bool { return len(r.noticesOf(token)) > 0 })
	time.Sleep(liveSettle)
	notices := r.noticesOf(token)
	if len(notices) != 1 {
		r.fail("%s holds %d notices of %s; want one for its claim and cause: %q", r.child, len(notices), token, notices)
	}
	file := "the folder as a whole"
	if r.c.file != "" {
		file = fmt.Sprintf("the file %q", r.c.file)
	}
	for _, said := range []string{
		fmt.Sprintf("the cause %q", r.c.cause), file, fmt.Sprintf("arm %q of experiment %q", folderProofArmName, folderProofExperiment),
		fmt.Sprintf("the folder of texts %q", r.folder),
	} {
		if !strings.Contains(notices[0], said) {
			r.fail("the notice of %s on %s says no %s: %q", token, r.child, said, notices[0])
		}
	}
	return notices[0]
}

// childArchitectNotice holds the child's own architect's launch, a claim of its own that met the
// same cause as the reviewer's (the folder read at one fingerprint, the same file and reason), to
// its own WARN line and its own one notice on the child's issue.
func (r *folderProofLive) childArchitectNotice() {
	reviewer, architect := r.token(r.child, claim.RoleReviewer), r.token(r.child, claim.RoleArchitect)
	reviewed, line := r.fellBackLine(reviewer, 0), r.fellBackLine(architect, 0)
	if line.Attrs["fingerprint"] != reviewed.Attrs["fingerprint"] {
		r.fail("the child architect's launch fell back at fingerprint %v and the reviewer's at %v; want one cause", line.Attrs["fingerprint"], reviewed.Attrs["fingerprint"])
	}
	notice := r.awaitOneNotice(architect)
	r.note("%s's launch met the reviewer's cause (fingerprint %v, file %v, cause %v) and %s holds its own one notice: %s",
		architect, line.Attrs["fingerprint"], line.Attrs["file"], line.Attrs["cause"], r.child, notice)
}

// relaunchPostsNoSecondNotice ends the reviewer's process group, its Oh My Pi among it, so the
// daemon relaunches the same claim over the same folder; once the daemon's line names that
// launch's fallback and its second notice applied as none, the issue still holds one notice of the
// reviewer's claim.
func (r *folderProofLive) relaunchPostsNoSecondNotice() {
	reviewer := r.token(r.child, claim.RoleReviewer)
	held := r.claim(reviewer)
	if held.Locator == nil {
		r.fail("%s is %s with no locator, so this step names no process to end", reviewer, held.State)
	}
	from := r.daemon.count()
	pid := r.rt.killProcessGroup(*held.Locator)
	r.note("ended %s's process group (pid %d) at generation %d", reviewer, pid, held.Generation)
	line := r.fellBackLine(reviewer, from)
	r.awaitLogFrom("the daemon's line that "+string(reviewer)+"'s second notice applied as none", from, 2*time.Minute, func(record loggedRecord) bool {
		return record.Message == folderProofNoticeAgain && record.Attrs["claim"] == string(reviewer)
	})
	notice := r.awaitOneNotice(reviewer)
	r.note("the relaunch at generation %v fell back for the same cause, the daemon applied no second notice, and %s still holds one: %s",
		line.Attrs["generation"], r.child, notice)
}

// ---- what a step reads -------------------------------------------------------------------------

// request is the n-th turn (0 the first) token's stand-in took, read by omprequest.Read.
func (r *folderProofLive) request(token claim.Token, n int) omprequest.Request {
	stand := r.panes[token]
	if stand.count() <= n {
		r.fail("%s's stand-in took %d turns; this step reads turn %d", token, stand.count(), n+1)
	}
	request, err := omprequest.Read(stand.turn(n))
	if err != nil {
		r.fail("read turn %d of %s: %v", n+1, token, err)
	}
	return request
}

// claimOf is token as the operator's claim listing shows it now, and false while it holds none.
func (r *folderProofLive) claimOf(token claim.Token) (api.OperatorClaim, bool) {
	r.t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	if status != http.StatusOK {
		r.t.Fatalf("the operator's claim listing = %d %s", status, body)
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		r.t.Fatalf("decode the claim listing %s: %v", body, err)
	}
	for _, held := range list.Claims {
		if held.Token == token {
			return held, true
		}
	}
	return api.OperatorClaim{}, false
}

// awaitPane waits for the daemon to hold token's claim and for its pane to report ready.
func (r *folderProofLive) awaitPane(token claim.Token) {
	r.t.Helper()
	r.await(string(token)+" to be launched", paneReady, func() bool {
		_, held := r.claimOf(token)
		return held
	})
	r.awaitReady(token)
}

// firstTurn waits for token's pane and its first turn, and delivers it a task where its launch
// gave it none within 30 s: a child's own architect, whose start carries no task, among them.
func (r *folderProofLive) firstTurn(token claim.Token) {
	r.t.Helper()
	r.awaitPane(token)
	stand := r.panes[token]
	deadline := time.Now().Add(30 * time.Second)
	for stand.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	if held, _ := r.claimOf(token); stand.count() == 0 && held.Pending == nil {
		r.deliver(token, "Reply with the word ANSWERED.")
	}
	r.await(string(token)+"'s first turn", folderProofTurns, func() bool { return stand.count() > 0 })
}

// deliver gives token's claim one task of the operator's own.
func (r *folderProofLive) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver", api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		r.fail("deliver %q to %s = %d %s, want 200", task, token, status, body)
	}
}

// armsOf is legion.arms in the OTEL_RESOURCE_ATTRIBUTES of the Oh My Pi token's claim runs,
// percent-decoded, "" where it names none, with that process's pid.
func (r *folderProofLive) armsOf(token claim.Token) (string, int) {
	r.t.Helper()
	held := r.claim(token)
	if held.Locator == nil {
		r.fail("%s is %s with no locator, so it names no process to read", token, held.State)
	}
	shim, _, err := parseLocalIncarnation(held.Locator.Incarnation)
	if err != nil {
		r.fail("the locator of %s names no pid: %v", token, err)
	}
	pid := ompChildOf(r.t, r.omp, r.rt.paneLogs, token, shim)
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		r.fail("read the environment of %s's Oh My Pi (pid %d): %v", token, pid, err)
	}
	for _, pair := range strings.Split(string(raw), "\x00") {
		value, found := strings.CutPrefix(pair, "OTEL_RESOURCE_ATTRIBUTES=")
		if !found {
			continue
		}
		for _, member := range strings.Split(value, ",") {
			key, encoded, _ := strings.Cut(strings.TrimSpace(member), "=")
			if key != "legion.arms" {
				continue
			}
			arms, err := url.PathUnescape(encoded)
			if err != nil {
				r.fail("legion.arms %q of %s's Oh My Pi does not decode: %v", encoded, token, err)
			}
			return arms, pid
		}
	}
	return "", pid
}

// noticesOf is every notice of a fallback on the child's issue that names token's claim, as the
// real Dispatch holds them.
func (r *folderProofLive) noticesOf(token claim.Token) []string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bodies, err := dispatch.New(r.dispatch.base, dispatch.BearerTransport(r.dispatch.token, nil)).MessageBodiesSince(ctx, r.child, r.started)
	if err != nil {
		r.fail("read the messages of %s on Dispatch: %v", r.child, err)
	}
	var notices []string
	for _, body := range bodies {
		if strings.Contains(body, folderProofNoticed) && strings.Contains(body, "claim "+string(token)+" at generation") {
			notices = append(notices, body)
		}
	}
	return notices
}

// keepRequests writes every turn each stand-in of the case took into the run's evidence, beside
// the daemon's log, as requests/<case>/<provider>-<n>.json, so what each pane offered its model
// can be read after the run. A run that names no evidence keeps none.
func (r *folderProofLive) keepRequests() {
	daemonLog := os.Getenv(liveDaemonLogEnv)
	if daemonLog == "" {
		return
	}
	dir := filepath.Join(filepath.Dir(daemonLog), "requests", r.c.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Errorf("make the request evidence directory %s: %v", dir, err)
		return
	}
	for _, stand := range append([]*folderProofStandin{r.spare}, slices.Collect(maps.Values(r.panes))...) {
		for n := range stand.count() {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.json", stand.name, n+1)), stand.turn(n), 0o600); err != nil {
				r.t.Errorf("keep turn %d of %s: %v", n+1, stand.name, err)
			}
		}
	}
}

// logAt is the place in the daemon's log of the first record matching want, -1 for none.
func (r *folderProofLive) logAt(want func(loggedRecord) bool) int {
	r.daemon.mu.Lock()
	defer r.daemon.mu.Unlock()
	return slices.IndexFunc(r.daemon.records, want)
}

// killPanes ends every process group the local runtime started, once the daemon has stopped: the
// panes of the stopped daemon, whose next daemon launches its claims again.
func (r *folderProofLive) killPanes() {
	r.t.Helper()
	r.rt.mu.Lock()
	started := slices.Clone(r.rt.started)
	r.rt.mu.Unlock()
	for _, pane := range started {
		_ = syscall.Kill(-pane.pid, syscall.SIGKILL)
	}
	for _, pane := range started {
		select {
		case <-pane.done:
		case <-time.After(localStopGrace):
			r.fail("the worker of %s (pid %d) did not end after SIGKILL; its log is %s", pane.spec.Claim, pane.pid, pane.log)
		}
	}
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------

// writeProfile writes the shared profile's providers for this case: each claim's stand-in and the
// spare, one model each, and the settings a pane needs to run unattended.
func (r *folderProofLive) writeProfile() {
	t := r.t
	agent := filepath.Join(r.shared.home, ".omp", "profiles", r.shared.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	stands := []*folderProofStandin{r.spare}
	for _, pane := range r.claimPanes() {
		stands = append(stands, r.panes[pane.token])
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/texts_folder_live_test.go.\nproviders:\n")
	for _, stand := range stands {
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			stand.name, stand.url, stand.name+standinModelID, stand.name)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(stand.name))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/texts_folder_live_test.go.\nenabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the overlays each pane is told through PI_CONFIG_FILES: the operator's,
// which turns the host's own retries off, and one per claim naming its stand-in's model alone.
func (r *folderProofLive) writeOverlays() {
	t := r.t
	dir := filepath.Join(t.TempDir(), "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	r.overlays = map[claim.Token][]string{}
	for _, pane := range r.claimPanes() {
		path := filepath.Join(dir, pane.provider+".yml")
		writeRoleOverlay(t, path, standinModel(pane.provider))
		r.overlays[pane.token] = []string{operator, path}
	}
	spare := filepath.Join(dir, folderProofSpare+".yml")
	writeRoleOverlay(t, spare, standinModel(folderProofSpare))
	r.spareOverlays = []string{operator, spare}
}

// ---- the model stand-in ------------------------------------------------------------------------

// folderProofCall is one tool call a stand-in answers a turn with: the tool as Oh My Pi names it
// to the model with any leading `_` taken off, and its input.
type folderProofCall struct {
	tool  string
	input map[string]any
}

// folderProofStandin is one claim's model: it records every request (modelStandin) and every turn,
// a request with a system prompt and tools that is not the plugin's own `<btw>` side question,
// and answers each turn with the calls its script names, in one message, or with a WAITING line.
type folderProofStandin struct {
	*modelStandin
	// script answers turn n (0 the first), whose body is body, with the calls it makes; none is a
	// WAITING line. Nil answers every turn so.
	script func(n int, body []byte) []folderProofCall

	mu        sync.Mutex
	turns     [][]byte
	issued    map[string]folderProofCall
	unoffered []string
}

func newFolderProofStandin(t *testing.T, provider string) *folderProofStandin {
	t.Helper()
	s := &folderProofStandin{modelStandin: &modelStandin{name: provider}, issued: map[string]folderProofCall{}}
	s.serve(t, s)
	return s
}

func (s *folderProofStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	var request struct {
		Model    string            `json:"model"`
		System   json.RawMessage   `json:"system"`
		Tools    []offeredTool     `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &request)
	if request.Model == "" {
		request.Model = s.name + standinModelID
	}
	turn := len(request.Tools) > 0 && len(request.Messages) > 0 && len(request.System) > 0 && string(request.System) != "null" && !isSideQuestion(body)
	if !turn {
		folderProofStream(w, request.Model, "end_turn", []folderProofBlock{{text: "PROCEEDING"}})
		return
	}
	s.mu.Lock()
	n := len(s.turns)
	s.turns = append(s.turns, []byte(body))
	var calls []folderProofCall
	if s.script != nil {
		calls = s.script(n, []byte(body))
	}
	var blocks []folderProofBlock
	for i, call := range calls {
		at := slices.IndexFunc(request.Tools, func(tool offeredTool) bool { return strings.TrimLeft(tool.Name, "_") == call.tool })
		if at < 0 {
			s.unoffered = append(s.unoffered, call.tool)
			continue
		}
		id := fmt.Sprintf("toolu_folder_%s_%d_%d", s.name, n, i)
		s.issued[id] = call
		blocks = append(blocks, folderProofBlock{id: id, tool: request.Tools[at].Name, input: call.input})
	}
	s.mu.Unlock()
	if len(blocks) == 0 {
		folderProofStream(w, request.Model, "end_turn", []folderProofBlock{{text: folderProofWaiting}})
		return
	}
	folderProofStream(w, request.Model, "tool_use", blocks)
}

// count is how many turns the stand-in has taken.
func (s *folderProofStandin) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turns)
}

// turn is the body of turn n, 0 the first.
func (s *folderProofStandin) turn(n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns[n]
}

// issuedPath is the `path` input of the call issued under id, "" for none.
func (s *folderProofStandin) issuedPath(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, _ := s.issued[id].input["path"].(string)
	return path
}

// unofferedCalls are the tools a script called that no request offered.
func (s *folderProofStandin) unofferedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.unoffered)
}

// folderProofBlock is one content block of an answer: text, or a call of tool under id.
type folderProofBlock struct {
	text     string
	id, tool string
	input    map[string]any
}

// folderProofStream answers one Anthropic Messages request with blocks, each opened empty and
// written whole by one delta, and a message that stops for stop, in the events the plugins' own
// pane harness writes (packages/pi-shared/test/omp-harness.ts, messageStream): a text block ends
// the turn, and tool_use blocks, one or more in one message, stop for tool_use, so the host runs
// each and sends every result in its next request.
func folderProofStream(w http.ResponseWriter, model, stop string, blocks []folderProofBlock) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(name string, data any) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_folder", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 1},
	}})
	for i, block := range blocks {
		start := map[string]any{"type": "text", "text": ""}
		delta := map[string]any{"type": "text_delta", "text": block.text}
		if block.tool != "" {
			arguments, _ := json.Marshal(block.input)
			start = map[string]any{"type": "tool_use", "id": block.id, "name": block.tool, "input": map[string]any{}}
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(arguments)}
		}
		event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": start})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": 16}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

// folderProofResult is one tool result a request carries: the call's id, its text, and whether
// the host marked it an error.
type folderProofResult struct {
	id, text string
	failed   bool
}

// folderProofResults are every tool result body's messages carry, in order.
func folderProofResults(body []byte) []folderProofResult {
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	var results []folderProofResult
	for _, message := range request.Messages {
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type != "tool_result" {
				continue
			}
			result := folderProofResult{id: block.ToolUseID, failed: block.IsError}
			var plain string
			if json.Unmarshal(block.Content, &plain) == nil {
				result.text = plain
			} else {
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(block.Content, &parts)
				for _, part := range parts {
					if part.Type == "text" {
						result.text += part.Text
					}
				}
			}
			results = append(results, result)
		}
	}
	return results
}

// folderProofAgentsNamed are the task tool's agents named name.
func folderProofAgentsNamed(request omprequest.Request, name string) []omprequest.Agent {
	var named []omprequest.Agent
	for _, agent := range request.Agents {
		if agent.Name == name {
			named = append(named, agent)
		}
	}
	return named
}

// folderProofSkillsNamed are the skills listed under name itself.
func folderProofSkillsNamed(request omprequest.Request, name string) []omprequest.Skill {
	var named []omprequest.Skill
	for _, skill := range request.Skills {
		if skill.Name == name {
			named = append(named, skill)
		}
	}
	return named
}

// folderProofAliases are the skills listed under a collision alias of skill, `<namespace>/<skill>`
// or `<namespace>/<skill>~<n>`, as Oh My Pi names a copy that lost to another of the same name.
func folderProofAliases(request omprequest.Request, skill string) []string {
	var aliases []string
	for _, listed := range request.Skills {
		_, base, namespaced := strings.Cut(listed.Name, "/")
		if base, _, _ = strings.Cut(base, "~"); namespaced && base == skill {
			aliases = append(aliases, listed.Name)
		}
	}
	return aliases
}

// folderProofHoldsAny is the first of markers text holds, "" for none.
func folderProofHoldsAny(text string, markers []string) string {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

// folderProofCut is text as a failure names it: its first 400 bytes.
func folderProofCut(text string) string {
	if len(text) > 400 {
		return text[:400] + "…"
	}
	return text
}
