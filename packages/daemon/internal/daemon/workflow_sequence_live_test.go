//go:build e2e

// The live proof that each issue walks the sequence of phases its record names, on a running
// daemon (dispatch://INFRA-3570): one issue on the `default` sequence and one on `light`, from the
// library phase.Sequences holds, driven side by side through one booted daemon to done.
// scripts/e2e/workflow-sequence.sh builds this driver with `-tags e2e` and owns the nats-server and
// the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real admission, which starts each issue's first phase once
// its design gate is open, the real HTTP API, the real claim supervision and its worker stream,
// the real openWorkflow, whose engine reads each issue's own table (workflow.TableOf), the real
// JetStream consumers and the real outbox lanes. Facts enter only as production's do: GitHub
// envelopes on the ENVOY_NOTIFICATIONS stream for each pull request's opening, the review App's
// approval and the merge; the daemon's own HTTP routes for each role's registration, readiness,
// grants and completions and for the architect's sign-off; and the daemon's worker stream for each
// role's turns, which this driver speaks as `legion worker-shim` does (shimwire frames, through
// phase_entry_converge_live_test.go's convergeClaimant).
//
// The driver acts as whatever role the daemon starts, at whatever phase the daemon puts the issue
// in: it never steers an issue by the list it expects, so an issue the daemon walked down the wrong
// line is driven down that line and the walk the checks read is the daemon's own. Each check
// prints `== <name>`, what it observed and `ok <name>`; the first that fails ends the run non-zero,
// naming it.
//
//   - Both issues start. The seed puts each at admitted with its design gate open and no slot, and
//     the boot's admission starts its first phase: planning for the default issue, implementing for
//     the light one, which is the gate's own row read from the issue's sequence.
//   - Both issues walk to done, a phase of each in turn. Each phase is played as its role plays it:
//     the planner's, the implementer's, the tester's (with its verdict) and the reviewer's
//     completion naming its handoff record; the review App's approval of the head, as Envoy relays
//     a GitHub review; the architect's sign_off where the line has an accepting phase; the
//     implementer's retro; the merger's completion; and GitHub's merge, which the stand-in reports
//     and Envoy relays. After each move the driver reads the daemon's own records of the issue:
//     each phase worker's phases row (record.PhaseRow.Entry, raised by every start a transition
//     makes, with the moment and the task) and the claims GET /legion/v1/operator/claims lists.
//   - The walks are read from the daemon's own log: every `workflow: phase changed` line
//     Engine.transition writes on commit (logOnCommit), from the file LEGION_E2E_DAEMON_LOG names.
//   - The control: the checks below are run against the wrong expectation first — the default
//     issue's walk held to the light line, the light issue's to the default line, the default
//     issue's first phase held to implementing, its records before the review ended held to
//     having started no planner, tester or merger, and its records at done held to having never
//     started the planner and the tester — and each must refuse, or the run fails, since a check
//     that cannot refuse proves nothing.
//   - The checks: the default issue walked planning, implementing, testing, reviewing, accepting,
//     retro, merging, done; the light issue walked implementing, reviewing, merging, done, its
//     first phase implementing; the daemon started at each phase the role that works it, and no
//     one at accepting and done; the light issue's records show no planner, tester or merger
//     before its review ended; and at done the roles never started are none for the default issue
//     and the planner and the tester for the light one.
//   - One line per issue a person can read: its workflow, its phases in order with the seconds it
//     stood in each, the total and the roles never started.
//
// What stands in lies outside the rule. The broker is a native nats-server the script starts and
// names in LEGION_E2E_NATS_URL. GitHub is this proof's REST stand-in for the two pull requests
// (sequenceGitHub), which serves each pull request once its implementer has opened it — open and
// mergeable with merge state `clean`, its reviews as none — and merged once its merger has merged
// it, the base branch's commit and the issue branch the daemon creates there, and nothing else.
// Dispatch is the board the package's whole-daemon live proofs share (liveDispatch, in
// live_proof_test.go), Envoy's listener the package's noticeListener, and the two Apps' tokens are
// workflowTokenRecorder's placeholders, which is also where the review App's login comes from. No
// agent runs: every claim is launched by the package's fake runtime, which says it provisions each
// workspace in the claim's own pod, so the daemon clones nothing from GitHub. The issues, their
// design gates and their architects' claims are seeded through the store, as every daemon test
// seeds its fixture, and so is each issue's sequence (record.Issue.Workflow), which stands in for
// the architect's `set_workflow` before the first phase starts. The project's configuration is
// workflowConfig's, whose production check is off, so a close opens no look.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// sequenceBase is the base branch both pull requests sit on, the repository's default, and
// sequenceBaseCommit the commit GitHub's stand-in names for it, which the issue branch the daemon
// creates for each issue starts at (ghbranch.Create).
const (
	sequenceBase       = "main"
	sequenceBaseCommit = "5e0b7d3a9c1f2e4d6b8a0c2e4f6a8b0d2c4e6f81"
)

// sequenceTaskBound is how long a step waits for a task the daemon sends by itself: a start's
// delivery to a claim that has just come up, and the merge-state read a completion queues. Each
// arrives in well under a second on this rig; the bound only keeps a task that never comes from
// holding the run until the binary's own timeout.
const sequenceTaskBound = time.Minute

// sequenceDefaultWalk and sequenceLightWalk are the walks dispatch://INFRA-3570 sets for the two
// sequences, written here as the spike states them rather than read from the library, so a library
// that drifted from the spike fails the run (sequenceLibraryRefusal) instead of moving the
// expectation with it. Each ends at done, which the merge closes the issue into.
var (
	sequenceDefaultWalk = []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing,
		phase.Accepting, phase.Retro, phase.Merging, phase.Done}
	sequenceLightWalk = []phase.Phase{phase.Implementing, phase.Reviewing, phase.Merging, phase.Done}
)

// workflowSequenceLive is one run of the proof: the booted daemon, whose own issue (liveIssueProof's
// key) is the default issue, the two issues it drives, and GitHub's stand-in for their pull
// requests.
type workflowSequenceLive struct {
	liveIssueProof
	github *sequenceGitHub
	// issues is the default issue, then the light one.
	issues []*sequenceIssue
}

// sequenceIssue is one issue of the run: the sequence its record names and the walk the spike sets
// for it, its pull request, the roles the driver has brought up for it, the daemon's records read
// after each of its moves, and the walk the daemon's log reads.
type sequenceIssue struct {
	// label is the issue as the readable lines name it ("A" or "B").
	label string
	// key, title, number and rank are the issue's and its pull request's, the run's own: the store
	// a pod re-runs the proof against holds every earlier run's.
	key, title string
	number     int
	rank       string
	// workflow is the sequence the seed records for the issue (record.Issue.Workflow), reason the
	// reason its architect would have given, and want the walk the spike sets for that sequence.
	workflow, reason string
	want             []phase.Phase
	// head is the commit the implementer opens the pull request at, and mergeSHA the merge
	// commit GitHub reports for it. Both are full commit SHAs: a review the review App submits
	// decides a round only when its Legion footer names the pull request's head in full
	// (intake.PullRequestReview.LegionHead).
	head, mergeSHA string
	// view is the issue as the harness's one-issue reads take it (liveIssueProof keyed by this
	// issue), set once the daemon is booted.
	view *liveIssueProof
	// claimants is each role the driver has brought up on the issue, by role.
	claimants map[claim.Role]*convergeClaimant
	// reads is the daemon's records of the issue read at the seed and after each move;
	// inReviewing is the read made while the issue stood in reviewing, before the approval that
	// ends the round was published.
	reads       []sequenceRead
	inReviewing *sequenceRead
	// walk is every phase change of the issue the daemon's log reads, in order.
	walk []sequenceChange
}

// sequenceRead is what the daemon's own records said of one issue when the driver read them: the
// phase GET /legion/v1/state put it in, each phase worker's phases row (record.PhaseRow: the entry
// each start a transition makes raises, when it was raised and the task it carries), and the roles
// holding a claim on the issue, with each claim's state, as GET /legion/v1/operator/claims lists
// them.
type sequenceRead struct {
	phase   phase.Phase
	entries map[claim.Role]record.PhaseRow
	claims  map[claim.Role]string
}

// sequenceChange is one `workflow: phase changed` line of the daemon's log for an issue: when it
// was written, the phase the issue left and the one it entered, and the trigger.
type sequenceChange struct {
	at       time.Time
	from, to phase.Phase
	trigger  string
}

// TestEachIssueWalksItsOwnWorkflowSequenceLive drives the proof's steps against one booted daemon,
// in the order each step's state follows from the last: both issues started at their own
// sequence's first phase, both walked to done, the walks read from the daemon's log, the controls,
// the checks, and the two lines a person reads.
func TestEachIssueWalksItsOwnWorkflowSequenceLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newWorkflowSequenceLive(t)
	r.step("both-issues-start-at-the-first-phase-of-their-own-sequence", r.bothStart)
	r.step("both-issues-walk-to-done-on-one-daemon", r.bothWalk)
	r.step("the-walks-as-the-daemons-own-log-reads-them", r.readWalks)
	r.step("control-each-check-refuses-the-wrong-expectation", r.controls)
	r.step("each-issue-walked-its-own-sequence", r.checks)
	r.step("the-two-walks-as-a-person-reads-them", r.readable)
}

// newWorkflowSequenceLive seeds the two issues and boots the daemon over the script's broker with
// GitHub's stand-in for their pull requests. The default issue is the harness's own issue, and the
// light one the next key of the run's project, with the next pull request number.
func newWorkflowSequenceLive(t *testing.T) *workflowSequenceLive {
	t.Helper()
	r := &workflowSequenceLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/workflow-sequence.sh",
		sequenceBase, "the issue that walks the default sequence")}
	r.github = newSequenceGitHub(t)
	r.issues = []*sequenceIssue{
		{label: "A", key: r.key, title: r.title, number: r.number, rank: "A1", workflow: phase.DefaultSequenceName,
			reason: "the whole line: a plan, a test phase and an acceptance", want: sequenceDefaultWalk},
		{label: "B", key: r.cfg.Project + "-2", title: "the issue that walks the light sequence", number: r.number + 1, rank: "A2",
			workflow: "light", reason: "a small change: the review before the landing is enough", want: sequenceLightWalk},
	}
	for _, s := range r.issues {
		s.head, s.mergeSHA = headCommit(s.key, "implement"), headCommit(s.key, "merge")
		s.claimants = map[claim.Role]*convergeClaimant{}
		r.seed(s)
	}
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	for _, s := range r.issues {
		view := r.liveIssueProof
		view.key, view.number, view.title = s.key, s.number, s.title
		s.view = &view
	}
	return r
}

// seed records s as the daemon holds a root whose architect has written its spec, had it approved
// and chosen its sequence, before any phase of it starts: a root handed to Legion at admitted, its
// record naming the sequence and the reason, its design gate approved and ever opened, its row in
// the daemon's mirror of the Dispatch board, its lifecycle open under the workflow, and its
// architect's claim as a launch a previous daemon began and never recorded, which this boot
// relaunches. No slot is written and no phases row: the boot's admission gives the slot and starts
// the first phase, and every entry the proof reads is raised live. The seed's read of the phases
// rows is the baseline every later read is compared with.
func (r *workflowSequenceLive) seed(s *sequenceIssue) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(s.key, "in_progress")
	approved := 1
	r.write("issue "+s.key+", its design gate and its board row", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: s.key, Repo: liveRepo, DefaultBranch: sequenceBase, GitHubRepo: liveRepo, Tree: s.key, Project: r.cfg.Project,
			Title: s.title, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: s.rank, LastDispatchSeq: 1, HandedOver: true, Workflow: s.workflow, WorkflowReason: s.reason,
		}); err != nil {
			return err
		}
		if err := r.records.PutGate(ctx, tx, record.DesignGate{
			Issue: s.key, ArtifactID: "spec-" + s.key, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
		}); err != nil {
			return err
		}
		return r.records.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{Key: s.key, Project: r.cfg.Project, Status: "in_progress", Seq: 1})
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, s.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", s.key, err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: r.tokenOf(s.key, claim.RoleArchitect), Project: r.project, Tree: s.key, Issue: s.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", s.key, err)
	}
	// Every role a move of the issue could launch needs its prompt on disk, as the daemon's own
	// launch writes it.
	for _, role := range claim.Roles {
		writePrompt(t, r.cfg, r.tokenOf(s.key, role))
	}
	baseline := r.entriesOf(s.key)
	if len(baseline) != 0 {
		t.Fatalf("the seed of %s left phases rows %v, want none: every entry this proof reads is raised live", s.key, baseline)
	}
	s.reads = append(s.reads, sequenceRead{phase: phase.Admitted, entries: baseline})
	r.note("seeded: %s at admitted on workflow %q (%s), its design gate approved, its architect mid-launch; no slot and no phases row",
		s.key, s.workflow, s.reason)
}

// tokenOf is the claim token of role on key, as the daemon spells it.
func (r *workflowSequenceLive) tokenOf(key string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// entriesOf is each phases row of key by role, as the daemon's store holds them now. No route
// shows the rows whole: GET /legion/v1/state carries the entry of the issue's current phase alone.
func (r *workflowSequenceLive) entriesOf(key string) map[claim.Role]record.PhaseRow {
	r.t.Helper()
	rows := map[claim.Role]record.PhaseRow{}
	if err := r.st.Tx(context.Background(), func(tx pgx.Tx) error {
		phases, err := r.records.Phases(context.Background(), tx, key)
		for _, row := range phases {
			rows[row.Role] = row
		}
		return err
	}); err != nil {
		r.t.Fatalf("read the phases rows of %s: %v", key, err)
	}
	return rows
}

// claimsOf is each claim of key by role, with its state, as GET /legion/v1/operator/claims lists
// them now.
func (r *workflowSequenceLive) claimsOf(key string) map[claim.Role]string {
	r.t.Helper()
	claims := map[claim.Role]string{}
	for _, c := range r.d.claims() {
		if c.Issue == key {
			claims[c.Role] = c.State
		}
	}
	return claims
}

// read is the daemon's records of s now (sequenceRead).
func (r *workflowSequenceLive) read(s *sequenceIssue) sequenceRead {
	r.t.Helper()
	return sequenceRead{phase: r.issue(s.key).Phase, entries: r.entriesOf(s.key), claims: r.claimsOf(s.key)}
}

// phaseWorkers is every role a transition starts, in the order the default line first starts
// them: the role of each phase of phase.Order, the architect left out, since accepting is the
// tree's architect's and no row starts it (workflow's start).
func phaseWorkers() []claim.Role {
	var roles []claim.Role
	for _, p := range phase.Order {
		if role := phase.RoleFor(p); role != claim.RoleArchitect && !slices.Contains(roles, role) {
			roles = append(roles, role)
		}
	}
	return roles
}

// startedBetween is every phase worker whose phases row the daemon raised to a later entry between
// two reads: the roles a transition between them started (workflow's enter, which every
// transition's start goes through).
func startedBetween(before, after sequenceRead) []claim.Role {
	var roles []claim.Role
	for _, role := range phaseWorkers() {
		if after.entries[role].Entry > before.entries[role].Entry {
			roles = append(roles, role)
		}
	}
	return roles
}

// unstartedRoles is every phase worker the daemon's records hold no start of in read: no entry on
// its phases row, and no claim on the issue.
func unstartedRoles(read sequenceRead) []claim.Role {
	var roles []claim.Role
	for _, role := range phaseWorkers() {
		if _, claimed := read.claims[role]; read.entries[role].Entry == 0 && !claimed {
			roles = append(roles, role)
		}
	}
	return roles
}

// rolesLine is roles as a line names them, "none" for none.
func rolesLine(roles []claim.Role) string {
	if len(roles) == 0 {
		return "none"
	}
	words := make([]string, len(roles))
	for i, role := range roles {
		words[i] = string(role)
	}
	return strings.Join(words, ", ")
}

// Step 1. The boot's admission gives each issue its slot and starts the first phase of its own
// sequence (workflow's StartFirstPhase and the gate's row, which TableOf generates from the
// sequence the record names). The step reads where each issue stands and the daemon's records of
// it; whether that first phase is the right one is the checks' to say, after the controls.
func (r *workflowSequenceLive) bothStart() {
	for _, s := range r.issues {
		got := s.view.waitFor(s.key+" out of admitted", func(got api.Issue) bool { return got.Phase != phase.Admitted })
		read := r.read(s)
		s.reads = append(s.reads, read)
		r.note("read back: %s (workflow %s) stands in %s, slot %s; the daemon started %s",
			s.key, s.workflow, read.phase, slotOf(got), rolesLine(startedBetween(s.reads[len(s.reads)-2], read)))
	}
}

// Step 2. Both issues walk to done on the one daemon, a phase of each in turn, so neither line runs
// on a daemon the other has left alone. Each turn plays the role of the phase the daemon has the
// issue in now, waits for the daemon to move the issue on, and reads its records there. The number
// of turns is bounded: an issue the daemon kept moving without reaching done would otherwise hold
// the run until the binary's timeout.
func (r *workflowSequenceLive) bothWalk() {
	t := r.t
	limit := 2 * len(phase.Order)
	for turn := 0; ; turn++ {
		moving := false
		for _, s := range r.issues {
			at := r.issue(s.key).Phase
			if at == phase.Done {
				continue
			}
			if turn == limit {
				t.Fatalf("%s is still in %s after %d phases of it were played, want it done: its walk so far is %v", s.key, at, limit, readPhases(s.reads))
			}
			moving = true
			r.play(s, at)
			moved := s.view.waitFor(fmt.Sprintf("%s out of %s", s.key, at), func(got api.Issue) bool { return got.Phase != at })
			read := r.read(s)
			s.reads = append(s.reads, read)
			r.note("read back: %s left %s for %s; the daemon started %s", s.key, at, moved.Phase,
				rolesLine(startedBetween(s.reads[len(s.reads)-2], read)))
		}
		if !moving {
			return
		}
	}
}

// readPhases is the phase of each read, in order.
func readPhases(reads []sequenceRead) []phase.Phase {
	phases := make([]phase.Phase, len(reads))
	for i, read := range reads {
		phases[i] = read.phase
	}
	return phases
}

// play acts as the role of at on s, as that role's agent would, up to the fact that ends the
// phase. A phase no role of this driver works is a phase the proof cannot drive, and ends the run.
func (r *workflowSequenceLive) play(s *sequenceIssue, at phase.Phase) {
	switch at {
	case phase.Planning:
		r.plan(s)
	case phase.Implementing:
		r.implement(s)
	case phase.Testing:
		r.test(s)
	case phase.Reviewing:
		r.review(s)
	case phase.Accepting:
		r.accept(s)
	case phase.Retro:
		r.retro(s)
	case phase.Merging:
		r.merge(s)
	default:
		r.t.Fatalf("%s stands in %s, a phase no role of this proof plays: its walk so far is %v", s.key, at, readPhases(s.reads))
	}
}

// claimant is role's claim on s as its agent reaches the daemon: the one the driver already
// brought up, or, the first time, the launch the daemon made brought to ready (register, then the
// connection served and the claim reported ready, as phase_entry_converge_live_test.go's
// servedClaimant does).
func (r *workflowSequenceLive) claimant(s *sequenceIssue, role claim.Role) *convergeClaimant {
	r.t.Helper()
	if c, up := s.claimants[role]; up {
		return c
	}
	session := fmt.Sprintf("ses_%s_%s", strings.ToLower(s.label), role)
	c := &convergeClaimant{liveSession: s.view.register(role, session), prompts: make(chan shimwire.Prompt, 8)}
	go c.serve()
	c.ready()
	s.claimants[role] = c
	return c
}

// task waits for the next task the daemon sends role's claim on s and holds that it is the work of
// at: every task a transition composes names the phase it starts (workflow's continueLine).
func (r *workflowSequenceLive) task(s *sequenceIssue, c *convergeClaimant, at phase.Phase) shimwire.Prompt {
	r.t.Helper()
	task := c.awaitPrompt(fmt.Sprintf("the %s task of %s", at, s.key), sequenceTaskBound)
	if !strings.Contains(task.Message, fmt.Sprintf("Issue: %s. Phase: %s.", s.key, at)) {
		r.t.Fatalf("the %s of %s was sent %q, want the task of phase %s", c.role, s.key, firstLine(task.Message), at)
	}
	return task
}

// completes reports c's completion and holds that the daemon took it (200).
func (r *workflowSequenceLive) completes(s *sequenceIssue, c *convergeClaimant, at phase.Phase, summary, commit, verdict, handoff string) {
	r.t.Helper()
	if status, failure := c.complete(summary, commit, verdict, handoff); status != http.StatusOK {
		r.t.Fatalf("the %s's completion of %s on %s = %d %s %s, want 200", c.role, at, s.key, status, failure.Code, failure.Error)
	}
	named := "no handoff record"
	if handoff != "" {
		named = "handoff record " + handoff
	}
	r.note("drove POST /legion/v1/handoff/complete for %s's %s in %s, naming %s: 200", s.key, c.role, at, named)
}

// plan is the planner's phase: its task, its plan recorded on the board and its completion.
func (r *workflowSequenceLive) plan(s *sequenceIssue) {
	c := r.claimant(s, claim.RolePlanner)
	task := r.task(s, c, phase.Planning)
	r.note("the planner of %s came up and was sent %s (%s)", s.key, task.DeliveryID, firstLine(task.Message))
	r.completes(s, c, phase.Planning, "planned it", "", "", c.handoff("plan"))
	c.endTurn()
}

// implement is the implementer's phase: its task, the pull request it opens at its head — GitHub's
// stand-in serves it from then on, and Envoy relays GitHub's opened — and its completion at that
// head, which the daemon takes once the pull request is recorded open (TriggerImplementationReady).
func (r *workflowSequenceLive) implement(s *sequenceIssue) {
	c := r.claimant(s, claim.RoleImplementer)
	task := r.task(s, c, phase.Implementing)
	r.note("the implementer of %s came up and was sent %s (%s)", s.key, task.DeliveryID, firstLine(task.Message))
	r.github.open(s.number, claim.IssueBranch(s.key), s.head)
	r.publishOpened(s)
	s.view.waitFor(fmt.Sprintf("pull request #%d of %s recorded open at %s", s.number, s.key, s.head), func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.Number == s.number && got.PullRequest.Head == s.head
	})
	r.note("opened pull request #%d on %s at %s, and the daemon recorded it from GitHub's opened", s.number, claim.IssueBranch(s.key), s.head)
	r.completes(s, c, phase.Implementing, "implemented it", s.head, "", c.handoff("implement"))
	c.endTurn()
}

// test is the tester's phase: its task and its completion with the verdict pass.
func (r *workflowSequenceLive) test(s *sequenceIssue) {
	c := r.claimant(s, claim.RoleTester)
	task := r.task(s, c, phase.Testing)
	r.note("the tester of %s came up and was sent %s (%s)", s.key, task.DeliveryID, firstLine(task.Message))
	r.completes(s, c, phase.Testing, "tested it", s.head, "pass", c.handoff("test"))
	c.endTurn()
}

// review is the reviewer's phase: its task, the read of the daemon's records while the round is
// still open, the review App's approval of the head with the reviewer's own Legion footer, as Envoy
// relays a GitHub review, and the reviewer's completion naming its review record. The approval is
// what ends the round (TriggerReviewApproved) once GitHub's answer for the head is one the issue
// may advance on, which the stand-in's `clean` is; the completion may land before or after that
// move, and the daemon takes it either way (workflow's leftItsPhase for the later one).
func (r *workflowSequenceLive) review(s *sequenceIssue) {
	c := r.claimant(s, claim.RoleReviewer)
	task := r.task(s, c, phase.Reviewing)
	r.note("the reviewer of %s came up and was sent %s (%s)", s.key, task.DeliveryID, firstLine(task.Message))
	read := r.read(s)
	s.inReviewing = &read
	r.note("read back while %s stands in reviewing, before the approval: phase workers with a claim or an entry: %s",
		s.key, rolesLine(startedSoFar(read)))
	handoff := c.handoff("review")
	r.publishReview(s, c.session)
	r.note("drove the review App's APPROVE of %s on pull request #%d, its footer naming session %s", s.head, s.number, c.session)
	r.completes(s, c, phase.Reviewing, "reviewed it", "", "", handoff)
	c.endTurn()
}

// startedSoFar is every phase worker read holds a start of: an entry on its phases row, or a claim.
func startedSoFar(read sequenceRead) []claim.Role {
	var roles []claim.Role
	for _, role := range phaseWorkers() {
		if _, claimed := read.claims[role]; read.entries[role].Entry > 0 || claimed {
			roles = append(roles, role)
		}
	}
	return roles
}

// reviewingRead is the read made while s stood in reviewing, before the approval (review). An
// issue the daemon walked past reviewing without the driver playing that phase has none, and the
// check of what was started before its review ended has nothing to read: the run ends, naming it.
func (r *workflowSequenceLive) reviewingRead(s *sequenceIssue) sequenceRead {
	r.t.Helper()
	if s.inReviewing == nil {
		r.t.Fatalf("%s was never played in reviewing, so no read of it stands before its review ended: its walk was %v", s.key, readPhases(s.reads))
	}
	return *s.inReviewing
}

// accept is the architect's phase: the tree's architect, on the claim this boot relaunched, signs
// the issue off over POST /legion/v1/signoff with a grant of its own session (TriggerSignOff).
func (r *workflowSequenceLive) accept(s *sequenceIssue) {
	t := r.t
	c := r.claimant(s, claim.RoleArchitect)
	status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", api.SignOffRequest{GrantID: c.grant(), Issue: s.key}, false)
	if status != http.StatusOK {
		t.Fatalf("the architect's sign_off of %s = %d %s, want 200", s.key, status, body)
	}
	r.note("drove POST /legion/v1/signoff of %s by its architect (session %s): %d", s.key, c.session, status)
}

// retro is the implementer's second phase: its retro task on the claim it worked implementing on,
// and its completion at the head, which names no handoff record (phase.Handoff).
func (r *workflowSequenceLive) retro(s *sequenceIssue) {
	c := r.claimant(s, claim.RoleImplementer)
	task := r.task(s, c, phase.Retro)
	r.note("the implementer of %s was sent its retro %s (%s)", s.key, task.DeliveryID, firstLine(task.Message))
	r.completes(s, c, phase.Retro, "ran the retro", s.head, "", "")
	c.endTurn()
}

// merge is the merger's phase: its task, its completion at the head, and GitHub's merge, which ends
// the phase (TriggerPullRequestMerged). The completion comes first and the merge after the read of
// the pull request that completion queues (readHandedOff) has been answered: a completion that
// lands once the merge has closed the issue is a completion of a done issue, which the route
// refuses, and a read GitHub answers merged is a read of a pull request this proof no longer
// means the daemon to judge. The stand-in then reports the pull request merged, and Envoy relays
// GitHub's closed with merged set.
func (r *workflowSequenceLive) merge(s *sequenceIssue) {
	c := r.claimant(s, claim.RoleMerger)
	task := r.task(s, c, phase.Merging)
	r.note("the merger of %s came up and was sent %s (%s)", s.key, task.DeliveryID, firstLine(task.Message))
	reads := r.github.answered(s.number)
	r.completes(s, c, phase.Merging, "merged it", s.head, "", "")
	c.endTurn()
	testwait.Eventually(r.t, fmt.Sprintf("the read of pull request #%d the merger's completion queues", s.number),
		func() bool { return r.github.answered(s.number) > reads })
	r.github.merge(s.number, s.mergeSHA)
	r.publishMerged(s)
	r.note("GitHub's stand-in reports pull request #%d merged as %s, and Envoy relayed GitHub's closed with merged set", s.number, s.mergeSHA)
}

// publishOpened publishes GitHub's opened of s's pull request at its head, as Envoy relays a GitHub
// pull-request event (intake's decodePullRequest reads it), and waits for the daemon to take it.
func (r *workflowSequenceLive) publishOpened(s *sequenceIssue) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "opened", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(s.number),
		"head_sha": s.head, "base_ref": sequenceBase, "head_ref": claim.IssueBranch(s.key), "title": s.title,
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(s.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the opened payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), s.number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("pr-%d-opened", s.number), string(payload)))
}

// publishReview publishes the review App's approval of s's head, as Envoy relays a GitHub
// pull-request review (intake's decodeReview reads it), with the Legion footer naming the
// reviewer's session and the head, which is what makes it the reviewer's own (workflow's
// byReviewer) and lets it decide the round at that head (workflow's refusesRound), and waits for
// the daemon to take it.
func (r *workflowSequenceLive) publishReview(s *sequenceIssue, session string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "review", "repo": liveRepo, "number": strconv.Itoa(s.number), "action": "submitted",
		"state": "approved", "commit_id": s.head, "head_sha": s.head, "author": reviewAppLogin,
		"body":           "the reviewer's approval of " + s.head + legionFooter(session, s.head),
		"body_truncated": "false", "review_id": strconv.Itoa(1000 + s.number),
		"submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the review payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), s.number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("review-%d-approved", s.number), string(payload)))
}

// publishMerged publishes GitHub's closed of s's pull request with merged set, as Envoy relays a
// GitHub pull-request event (intake's decodePullRequest reads it as a PullRequestMerged), and waits
// for the daemon to take it.
func (r *workflowSequenceLive) publishMerged(s *sequenceIssue) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(s.number),
		"head_sha": s.head, "base_ref": sequenceBase, "head_ref": claim.IssueBranch(s.key), "title": s.title,
		"merged": "true", "merge_commit_sha": s.mergeSHA,
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(s.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the merged payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), s.number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("pr-%d-merged", s.number), string(payload)))
}

// Step 3. Every phase change of each issue, read from the daemon's own log line (workflow's
// transition writes `workflow: phase changed` with from, to and trigger on the commit of the move,
// logOnCommit), with the moment it entered each phase and the role the daemon's records say it
// started there.
func (r *workflowSequenceLive) readWalks() {
	t := r.t
	path := os.Getenv(liveDaemonLogEnv)
	if path == "" {
		t.Fatalf("%s is unset, so there is no daemon log to read the walks from; scripts/e2e/workflow-sequence.sh sets it", liveDaemonLogEnv)
	}
	changes := readPhaseChanges(t, path)
	for _, s := range r.issues {
		s.walk = changes[s.key]
		r.note("%s, workflow %s: %d phase changes in %s", s.key, s.workflow, len(s.walk), path)
		for i, change := range s.walk {
			started := "a role the reads do not cover"
			if i+1 < len(s.reads) {
				started = rolesLine(startedBetween(s.reads[i], s.reads[i+1]))
			}
			r.note("   %s entered %s at %s, from %s by %s; the daemon started %s",
				s.key, change.to, change.at.UTC().Format("15:04:05.000"), change.from, change.trigger, started)
		}
	}
	if unserved := r.github.unservedRequests(); len(unserved) > 0 {
		r.note("GitHub's stand-in answered 404 to %d request(s) it serves nothing for: %s", len(unserved), strings.Join(unserved, "; "))
	}
}

// phaseChangedAttr is one key=value of a slog text line: the value bare, or quoted as slog quotes a
// value with a space or a quote in it.
var phaseChangedAttr = regexp.MustCompile(`(?:^| )([A-Za-z_]+)=("(?:[^"\\]|\\.)*"|[^ ]*)`)

// readPhaseChanges is every `workflow: phase changed` line of the daemon's log at path, by issue,
// each issue's in the order the log holds them, which is the order of the commits that wrote them.
// A line that does not parse is a log this proof cannot read, and ends the run.
func readPhaseChanges(t *testing.T, path string) map[string][]sequenceChange {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the daemon's log %s: %v", path, err)
	}
	defer file.Close()
	changes := map[string][]sequenceChange{}
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for lines.Scan() {
		line := lines.Text()
		if !strings.Contains(line, `msg="workflow: phase changed"`) {
			continue
		}
		attrs := map[string]string{}
		for _, match := range phaseChangedAttr.FindAllStringSubmatch(line, -1) {
			value := match[2]
			if strings.HasPrefix(value, `"`) {
				if value, err = strconv.Unquote(value); err != nil {
					t.Fatalf("read %q of the daemon's log line %q: %v", match[1], line, err)
				}
			}
			attrs[match[1]] = value
		}
		at, err := time.Parse(time.RFC3339Nano, attrs["time"])
		if err != nil || attrs["issue"] == "" || attrs["to"] == "" {
			t.Fatalf("the daemon's log line %q names no time, issue or phase this proof can read (%v)", line, err)
		}
		changes[attrs["issue"]] = append(changes[attrs["issue"]], sequenceChange{
			at: at, from: phase.Phase(attrs["from"]), to: phase.Phase(attrs["to"]), trigger: attrs["trigger"],
		})
	}
	if err := lines.Err(); err != nil {
		t.Fatalf("read the daemon's log %s: %v", path, err)
	}
	return changes
}

// walkPhases is the phase each change entered, in order.
func walkPhases(walk []sequenceChange) []phase.Phase {
	phases := make([]phase.Phase, len(walk))
	for i, change := range walk {
		phases[i] = change.to
	}
	return phases
}

// walkRefusal is why walk is not want, "" when it is: the phases it entered are want's, in want's
// order, each change leaves the phase the one before it entered, and the first leaves admitted on
// the gate's opening.
func walkRefusal(walk []sequenceChange, want []phase.Phase) string {
	if got := walkPhases(walk); !slices.Equal(got, want) {
		return fmt.Sprintf("it walked %v, and the line is %v", got, want)
	}
	from := phase.Admitted
	for _, change := range walk {
		if change.from != from {
			return fmt.Sprintf("it entered %s from %s, where the change before left it in %s", change.to, change.from, from)
		}
		from = change.to
	}
	return firstPhaseRefusal(walk, want[0])
}

// firstPhaseRefusal is why walk's first phase is not want, entered from admitted on the gate's
// opening (TriggerGateOpened), "" when it is.
func firstPhaseRefusal(walk []sequenceChange, want phase.Phase) string {
	if len(walk) == 0 {
		return "the daemon's log holds no phase change of it"
	}
	first := walk[0]
	if first.to != want || first.from != phase.Admitted || first.trigger != "gate_opened" {
		return fmt.Sprintf("its first change entered %s from %s by %s, and the first phase is %s, entered from admitted by gate_opened",
			first.to, first.from, first.trigger, want)
	}
	return ""
}

// startedRefusal is why read holds a start of one of roles, "" when it holds none: an entry on the
// role's phases row, or a claim of that role on the issue.
func startedRefusal(read sequenceRead, roles []claim.Role) string {
	var started []string
	for _, role := range roles {
		state, claimed := read.claims[role]
		if entry := read.entries[role]; entry.Entry > 0 || claimed {
			started = append(started, fmt.Sprintf("the %s (phases row on entry %d, raised %s; claim %q)",
				role, entry.Entry, entry.EntryRaisedAt.UTC().Format("15:04:05.000"), state))
		}
	}
	if len(started) > 0 {
		return "the daemon had started " + strings.Join(started, " and ")
	}
	return ""
}

// neverStartedRefusal is why the roles read holds no start of are not want, "" when they are.
func neverStartedRefusal(read sequenceRead, want []claim.Role) string {
	if got := unstartedRoles(read); !slices.Equal(got, want) {
		return fmt.Sprintf("the roles never started are %s, and the line leaves %s unstarted", rolesLine(got), rolesLine(want))
	}
	return ""
}

// startsRefusal is why the daemon's records of s do not show it starting, at each phase it
// entered, the role that works that phase and nobody else, with a task naming that phase; ""
// when they do. Accepting is the tree architect's, which no row starts, and done starts nobody.
// The reads after the seed are one per phase the driver saw the issue enter, so they are held to
// the walk the log reads too.
func startsRefusal(s *sequenceIssue) string {
	if got, walked := readPhases(s.reads[1:]), walkPhases(s.walk); !slices.Equal(got, walked) {
		return fmt.Sprintf("the driver read the records at %v, and the log walks %v", got, walked)
	}
	for i := 1; i < len(s.reads); i++ {
		at := s.reads[i].phase
		var want []claim.Role
		if role := phase.RoleFor(at); role != "" && role != claim.RoleArchitect {
			want = []claim.Role{role}
		}
		got := startedBetween(s.reads[i-1], s.reads[i])
		if !slices.Equal(got, want) {
			return fmt.Sprintf("at %s the daemon started %s, and the role that works %s is %s", at, rolesLine(got), at, rolesLine(want))
		}
		for _, role := range got {
			if task := s.reads[i].entries[role].EntryTask; !strings.Contains(task, fmt.Sprintf("Phase: %s.", at)) {
				return fmt.Sprintf("at %s the %s's entry carries the task %q, which names another phase", at, role, firstLine(task))
			}
		}
	}
	return ""
}

// sequenceLibraryRefusal is why the library's sequence of s's workflow, done after it, is not the
// walk the spike sets for it, "" when it is.
func sequenceLibraryRefusal(s *sequenceIssue) string {
	sequence, known := phase.SequenceFor(s.workflow)
	if !known {
		return fmt.Sprintf("the library holds no sequence %q", s.workflow)
	}
	if line := append(slices.Clone(sequence.Phases), phase.Done); !slices.Equal(line, s.want) {
		return fmt.Sprintf("the library's %s walks %v, and the spike sets %v", s.workflow, line, s.want)
	}
	return ""
}

// Step 4. The control. Each check of step 5 is run first against the wrong expectation, on the
// same records and the same walks it reads in step 5, and each must refuse: a check that passes
// here could not have failed there either, and would prove nothing. The light line is the default
// line's sub-list, so holding the default walk to it fails on the walk itself; the default
// issue's records hold a planner and a tester started before its review ended and at done hold no
// role unstarted, which is what the two role checks must see.
func (r *workflowSequenceLive) controls() {
	t := r.t
	a, b := r.issues[0], r.issues[1]
	for _, control := range []struct {
		name, refusal string
	}{
		{fmt.Sprintf("%s's walk held to the %s line %v", a.key, b.workflow, b.want), walkRefusal(a.walk, b.want)},
		{fmt.Sprintf("%s's walk held to the %s line %v", b.key, a.workflow, a.want), walkRefusal(b.walk, a.want)},
		{fmt.Sprintf("%s's first phase held to %s", a.key, b.want[0]), firstPhaseRefusal(a.walk, b.want[0])},
		{fmt.Sprintf("%s's records in reviewing held to no planner, tester or merger started", a.key),
			startedRefusal(r.reviewingRead(a), []claim.Role{claim.RolePlanner, claim.RoleTester, claim.RoleMerger})},
		{fmt.Sprintf("%s's records at done held to the planner and the tester never started", a.key),
			neverStartedRefusal(a.reads[len(a.reads)-1], []claim.Role{claim.RolePlanner, claim.RoleTester})},
	} {
		if control.refusal == "" {
			t.Fatalf("control %s passed, want it refused: the check cannot fail, so its pass below would prove nothing", control.name)
		}
		r.note("control: %s — refused, as it must be: %s", control.name, control.refusal)
	}
}

// Step 5. The checks, each failing by name: the library holds the lines the spike sets; each issue
// walked its own line, the light issue from implementing, which is the gate's own row reading the
// issue's sequence; at each phase the daemon started the role that works it; the light issue's
// records show no planner, tester or merger before its review ended; and at done the roles never
// started are none for the default issue and the planner and the tester for the light one. The
// record still names the sequence it was seeded with, which the first move writes when none is
// named, and which no move changes.
func (r *workflowSequenceLive) checks() {
	t := r.t
	a, b := r.issues[0], r.issues[1]
	for _, s := range r.issues {
		if refusal := sequenceLibraryRefusal(s); refusal != "" {
			t.Fatalf("the library's %s line: %s", s.workflow, refusal)
		}
		if refusal := walkRefusal(s.walk, s.want); refusal != "" {
			t.Fatalf("%s did not walk its %s line: %s", s.key, s.workflow, refusal)
		}
		r.note("%s walked its %s line: %v", s.key, s.workflow, walkPhases(s.walk))
		if refusal := startsRefusal(s); refusal != "" {
			t.Fatalf("%s's starts: %s", s.key, refusal)
		}
		r.note("%s: at each phase the daemon started the role that works it, with that phase's task, and nobody at accepting or done", s.key)
		var recorded *record.Issue
		if err := r.st.Tx(context.Background(), func(tx pgx.Tx) error {
			var err error
			recorded, err = r.records.Issue(context.Background(), tx, s.key)
			return err
		}); err != nil || recorded == nil {
			t.Fatalf("read the record of %s: %v", s.key, err)
		}
		if recorded.Workflow != s.workflow {
			t.Fatalf("the record of %s names workflow %q at done, want %q, the line it was seeded on", s.key, recorded.Workflow, s.workflow)
		}
		r.note("%s's record names workflow %q (%s)", s.key, recorded.Workflow, recorded.WorkflowReason)
	}
	if refusal := firstPhaseRefusal(b.walk, phase.Implementing); refusal != "" {
		t.Fatalf("%s's first phase: %s", b.key, refusal)
	}
	r.note("%s's first phase was implementing, entered from admitted by gate_opened: the gate's own row read its %s line", b.key, b.workflow)
	if refusal := startedRefusal(r.reviewingRead(b), []claim.Role{claim.RolePlanner, claim.RoleTester, claim.RoleMerger}); refusal != "" {
		t.Fatalf("%s before its review ended: %s", b.key, refusal)
	}
	r.note("%s before its review ended: no planner, no tester and no merger has an entry or a claim", b.key)
	for _, check := range []struct {
		s    *sequenceIssue
		want []claim.Role
	}{{a, nil}, {b, []claim.Role{claim.RolePlanner, claim.RoleTester}}} {
		if refusal := neverStartedRefusal(check.s.reads[len(check.s.reads)-1], check.want); refusal != "" {
			t.Fatalf("%s at done: %s", check.s.key, refusal)
		}
		r.note("%s at done: roles never started: %s", check.s.key, rolesLine(check.want))
	}
}

// Step 6. One line per issue a person can read: its workflow, each phase it stood in with the
// seconds it stood there, read from the daemon's log, the total from its first phase to done, and
// the roles the daemon never started. No agent runs, so the seconds are the driver's pace and the
// daemon's, not a worker's.
func (r *workflowSequenceLive) readable() {
	for _, s := range r.issues {
		var phases []string
		for i, change := range s.walk {
			if i+1 < len(s.walk) {
				phases = append(phases, fmt.Sprintf("%s %.1fs", change.to, s.walk[i+1].at.Sub(change.at).Seconds()))
			} else {
				phases = append(phases, string(change.to))
			}
		}
		total := s.walk[len(s.walk)-1].at.Sub(s.walk[0].at).Seconds()
		fmt.Printf("issue %s %s, workflow %s: %s; %.1fs from %s to %s; roles never started: %s\n",
			s.label, s.key, s.workflow, strings.Join(phases, " → "), total, s.walk[0].to, s.walk[len(s.walk)-1].to,
			rolesLine(unstartedRoles(s.reads[len(s.reads)-1])))
	}
}

// sequenceGitHub is GitHub's REST API for acme/widgets as this proof's daemon reads it: each issue's
// pull request once its implementer has opened it (open), listed on its branch and answered as GET
// /pulls/{number} answers one — open, `mergeable`, merge state `clean` — with its reviews as none,
// and answered merged, with its merge commit, once its merger has merged it (merge). It also
// answers what the daemon reads and writes around them: any repository, with default branch main,
// the base branch's commit and the create of an issue branch there, and a GraphQL root that knows
// no pull request. Anything else is answered 404 and kept (unservedRequests), so a read this proof
// did not foresee fails the step that waited on it, and is named, rather than reaching github.com.
type sequenceGitHub struct {
	t   *testing.T
	url string
	// mu guards pulls and unserved, and every answer is written under it, so a step that opens or
	// merges a pull request does so for every read after it and none before it.
	mu       sync.Mutex
	pulls    map[int]*sequencePull
	unserved []string
}

// sequencePull is one pull request as the stand-in answers it: its branch and head, its merge
// commit once merged ("" while open), when GitHub last moved it, and how many reads of it the
// stand-in has answered, which is how a step holds that a read the daemon queued was made.
type sequencePull struct {
	branch, head string
	mergeSHA     string
	updatedAt    time.Time
	answered     int
}

func newSequenceGitHub(t *testing.T) *sequenceGitHub {
	t.Helper()
	g := &sequenceGitHub{t: t, pulls: map[int]*sequencePull{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: g.ServeHTTP})
	return g
}

// open puts number on GitHub, open on branch at head.
func (g *sequenceGitHub) open(number int, branch, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = &sequencePull{branch: branch, head: head, updatedAt: time.Now().UTC()}
}

// merge has GitHub answer number merged as mergeSHA, as it does once the merger has merged it.
func (g *sequenceGitHub) merge(number int, mergeSHA string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pull(number)
	pull.mergeSHA, pull.updatedAt = mergeSHA, time.Now().UTC()
}

// answered is how many reads of number the stand-in has answered.
func (g *sequenceGitHub) answered(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pull(number).answered
}

// unservedRequests is every request the stand-in answered 404 because it serves nothing for it.
func (g *sequenceGitHub) unservedRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.unserved)
}

// pull is number's pull request, failing the test when the stand-in serves none: every setter and
// every count names a pull request the run opened. The caller holds the lock.
func (g *sequenceGitHub) pull(number int) *sequencePull {
	g.t.Helper()
	pull, served := g.pulls[number]
	if !served {
		g.t.Fatalf("the GitHub stand-in serves no pull request #%d", number)
	}
	return pull
}

func (g *sequenceGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/graphql":
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
	case req.Method == http.MethodPost && req.URL.Path == "/repos/"+liveRepo+"/git/refs":
		// The create of an issue branch at the base's commit (ghbranch.Create), which every start of
		// the issue's roles waits for (record.IssueBranch).
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	case req.Method == http.MethodGet && g.answer(w, req.URL.Path, req.URL.Query()):
	default:
		g.unserved = append(g.unserved, req.Method+" "+req.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// answer writes the stand-in's answer to a GET of path with query, and says whether it had one.
// The caller holds the lock.
func (g *sequenceGitHub) answer(w http.ResponseWriter, path string, query url.Values) bool {
	rest, ours := strings.CutPrefix(path, "/repos/"+liveRepo)
	if !ours {
		// Any other repository is answered as the launch reads it: a name and its default branch.
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); isRepository && len(parts) == 2 {
			g.write(w, map[string]any{"full_name": other, "default_branch": sequenceBase})
			return true
		}
		return false
	}
	switch rest {
	case "":
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": sequenceBase})
		return true
	case "/git/ref/heads/" + sequenceBase:
		g.write(w, map[string]any{"ref": "refs/heads/" + sequenceBase, "object": map[string]string{"sha": sequenceBaseCommit}})
		return true
	case "/pulls":
		// The open pull requests, of one branch when the read names its head as
		// `<owner>:<branch>` (githubrest.OpenPullRequestOn), every one otherwise.
		owner, _, _ := strings.Cut(liveRepo, "/")
		listed := []any{}
		for number, pull := range g.pulls {
			if head := query.Get("head"); pull.mergeSHA == "" && (head == "" || head == owner+":"+pull.branch) {
				listed = append(listed, g.body(number, pull))
			}
		}
		g.write(w, listed)
		return true
	}
	for number, pull := range g.pulls {
		switch rest {
		case fmt.Sprintf("/pulls/%d/reviews", number):
			g.write(w, []any{})
			return true
		case fmt.Sprintf("/pulls/%d", number):
			pull.answered++
			g.write(w, g.body(number, pull))
			return true
		}
	}
	return false
}

// body is number's pull request as GitHub answers it: open with `mergeable` and merge state
// `clean`, the state record.MergeState.Mergeable holds true of, or closed and merged with its merge
// commit, whose `mergeable` GitHub no longer computes.
func (g *sequenceGitHub) body(number int, pull *sequencePull) map[string]any {
	answer := map[string]any{
		"number": number, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, number), "body": "",
		"base":       map[string]string{"ref": sequenceBase},
		"head":       map[string]any{"ref": pull.branch, "sha": pull.head, "repo": map[string]string{"full_name": liveRepo}},
		"updated_at": pull.updatedAt.Format(time.RFC3339),
	}
	if pull.mergeSHA == "" {
		answer["state"], answer["merged"], answer["mergeable"], answer["mergeable_state"] = "open", false, true, "clean"
		return answer
	}
	answer["state"], answer["merged"], answer["mergeable"], answer["mergeable_state"] = "closed", true, nil, "unknown"
	answer["merged_at"], answer["merge_commit_sha"] = pull.updatedAt.Format(time.RFC3339), pull.mergeSHA
	return answer
}

// write encodes value as the answer.
func (g *sequenceGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}
