// The rig of the whole-daemon tests of an issue that closes when the checks of its goal pass
// (doneByChecksRig): the walk of step_records_rig_test.go, over a real Dispatch whose events are on
// and whose App reads a GitHub stand-in of the rig's own (doneByChecksGitHub), which the daemon reads
// too. A run makes the issues its options name (doneByChecksOptions), each on a pull request of its
// own, and the rig's own steps take each pull request's number and head, so an issue's next change
// walks on a second pull request. Every wait of the rig is on a row the daemon or Dispatch wrote,
// and its log line names that row. TestTheDoneByChecksRigDrivesAnIssueToItsMerge is the rig's own
// acceptance: one issue of the default steps driven to its merge, then a pass and a fail of its
// after-merge check recorded, each read back from Dispatch. It asserts nothing of when the issue
// closes.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// doneByChecksProject is the rig's project, in Dispatch and in the daemon, so its first issue
	// is ACME-1 (doneByChecksIssue).
	doneByChecksProject = "ACME"
	doneByChecksIssue   = "ACME-1"
	// doneByChecksTitle is the title of the issue and of its pull request, which names no issue
	// key: Dispatch attributes the pull request by its head branch alone.
	doneByChecksTitle = "The widgets report their merge"
	// doneByChecksPull is the number of the issue's pull request.
	doneByChecksPull = 41
	// doneByChecksCheck is the one check of the issue's Verification document, which runs after
	// the merge (doneByChecksVerification).
	doneByChecksCheck = "c-after-merge"
	// doneByChecksBaseCommit is main's head when the rig starts, which the issue's branch starts
	// at; doneByChecksMergeCommit is the merge of the issue's pull request (M), and
	// doneByChecksLaterCommit a later commit of main whose history holds it (C1). The pull
	// request's head is stepRecordsHead, the head every walk of stepRecordsRig works at.
	doneByChecksBaseCommit  = "ba5e000000000000000000000000000000000000"
	doneByChecksMergeCommit = "3e46e00000000000000000000000000000000000"
	doneByChecksLaterCommit = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	// doneByChecksAppToken is the installation token the stand-in mints for Dispatch's App.
	doneByChecksAppToken = "ghs_done_by_checks_rig"
	// doneByChecksReadWait bounds a wait for a read of an issue's checks. A read the daemon makes
	// before Dispatch has learned the merge names no merge commit of it, and the daemon reads again
	// a minute later at the soonest (workflow's closeReadAgain), so the wait is the walk's own
	// bound and that minute.
	doneByChecksReadWait = stepRecordsWait + time.Minute
)

// doneByChecksVerification is the issue's Verification document: one check, doneByChecksCheck,
// that runs after the merge.
const doneByChecksVerification = "# Verification\n\n:::check{#" + doneByChecksCheck +
	` surface="cli" when="after_merge" run="go test ./internal/widgets/"}` + "\nThe widgets report their merge once it reaches main.\n:::\n"

// doneByChecksOptions is what a run of the rig varies: the project's production check, the
// period of the controller's tick, and each issue the run makes, in order.
type doneByChecksOptions struct {
	// production is the project's production_check, the role its looks start: claim.RoleTester,
	// or "" for off.
	production claim.Role
	// controllerWake is the controller tick's period (controller_wake_interval_seconds); zero is
	// the configuration's default hour.
	controllerWake time.Duration
	// issues is each issue the run makes, in order, so Dispatch numbers the first ACME-1.
	issues []doneByChecksIssueSpec
}

// doneByChecksIssueSpec is one issue a run makes, in Dispatch and in the daemon's store: its title,
// its Verification document ("" for none), each other document it holds from its creation, by
// name, its pull request's number and head, open on legion/<KEY> from the boot on, the 1-based
// place among the run's issues of the issue it is a child of (0 for a root), and whether it stands
// at its review when the daemon boots, as the release before left it there. A root that does not
// stands at admitted with its gate approved, so its planning starts at the boot.
type doneByChecksIssueSpec struct {
	title        string
	verification string
	documents    map[string]string
	number       int
	head         string
	under        int
	reviewing    bool
}

// doneByChecksRig is stepRecordsRig's walk of the run's issues, over a Dispatch with its events on
// and the rig's own GitHub (standIn), which Dispatch's App and the daemon both read.
type doneByChecksRig struct {
	*stepRecordsRig
	standIn *doneByChecksGitHub
}

// newDoneByChecksRig is the rig of one issue, ACME-1, with doneByChecksVerification as its
// Verification document, in a project that runs no production check: the rig of its own
// acceptance.
func newDoneByChecksRig(t *testing.T) *doneByChecksRig {
	t.Helper()
	return newDoneByChecksRigWith(t, doneByChecksOptions{issues: []doneByChecksIssueSpec{
		{title: doneByChecksTitle, verification: doneByChecksVerification, number: doneByChecksPull, head: stepRecordsHead},
	}})
}

// newDoneByChecksRigWith starts the GitHub stand-in and a scratch Dispatch whose App reads it, with
// its events on the NATS server the daemon consumes. It makes the project ACME on acme/widgets,
// which runs as options say, saves the delivery settings that turn Dispatch's intake of GitHub's
// events on, and makes each issue of options with its documents (makeIssue). It seeds the daemon's
// store with each as its options say (seedIssue), opens each one's pull request on GitHub and
// publishes GitHub's opened of it, boots the daemon and registers each tree's architect.
func newDoneByChecksRigWith(t *testing.T, options doneByChecksOptions) *doneByChecksRig {
	t.Helper()
	natsURL := workflowNATS(t)
	proof := newLiveProofOn(t, natsURL)
	proof.cfg.Project = doneByChecksProject
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {ProductionCheck: options.production}}
	proof.cfg.AdmissionCap = 8
	if options.controllerWake > 0 {
		proof.cfg.ControllerWakeInterval = options.controllerWake
	}
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	standIn := newDoneByChecksGitHub(t)
	scratch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL, github: standIn.url})
	// stepRecordsRig's boot points the daemon at its github's url, which is the rig's own stand-in
	// here. That stand-in holds the pull requests, and the rig merges each itself (mergeOf), so the
	// stepRecordsGitHub holds none.
	r := &doneByChecksRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: scratch,
		github: &stepRecordsGitHub{t: t, url: standIn.url}, claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)},
		standIn: standIn}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Done by checks"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	// Dispatch's intake reads GitHub's events only while delivery settings name a deploy
	// repository, and Dispatch saves them only once its App reads that repository. A Dispatch
	// given no GitHub (Step 4's control) has neither, so it learns of no pull request.
	if r.dispatch.github != "" {
		r.person(http.MethodPut, "/api/v1/settings/delivery", map[string]any{
			"deploy_repo": liveRepo, "deploy_workflow_path": ".github/workflows/deploy.yaml", "production_job_names": []string{"deploy"},
			"pr_checks_workflow_path": ".github/workflows/ci.yaml", "population_authors": []string{stepRecordsPerson},
		}, http.StatusOK, nil)
		t.Logf("Dispatch reads GitHub at %s: delivery settings saved for %s", standIn.url, liveRepo)
	}
	made := make([]createdIssue, len(options.issues))
	for place, issue := range options.issues {
		made[place] = r.makeIssue(issue)
	}
	if made[0].Key != doneByChecksIssue {
		t.Fatalf("Dispatch made the project's first issue %q, want %s", made[0].Key, doneByChecksIssue)
	}
	for place, issue := range options.issues {
		r.on(made[place].Key)
		r.standIn.open(issue.number, claim.IssueBranch(r.key), issue.head)
		r.seedIssue(issue, made[place])
		opened := r.publishPull(issue.number, "opened")
		t.Logf("GitHub opened pull request #%d of %s from %s at %s: envelope %s", issue.number, liveRepo, claim.IssueBranch(r.key), issue.head, opened)
	}
	r.boot()
	for place, issue := range options.issues {
		if issue.under == 0 {
			r.on(made[place].Key)
			r.worker(claim.RoleArchitect)
		}
	}
	r.on(made[0].Key)
	return r
}

// makeIssue makes issue in Dispatch, a child of the run's issue its place under names, which
// Dispatch made before it: its documents, its Verification document, and its status and the legion
// label. The documents are written before the status, so the sequence the daemon is seeded with
// (createdIssue.LastSeq) is past their events. It adds the issue to the run's issues and answers
// it as Dispatch made it.
func (r *doneByChecksRig) makeIssue(issue doneByChecksIssueSpec) createdIssue {
	r.t.Helper()
	create := map[string]string{"project": r.cfg.Project, "title": issue.title}
	if issue.under > 0 {
		create["parent"] = r.issues[issue.under-1].key
	}
	var created createdIssue
	r.person(http.MethodPost, "/api/v1/issues", create, http.StatusCreated, &created)
	names := make([]string, 0, len(issue.documents))
	for name := range issue.documents {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		r.person(http.MethodPost, "/api/v1/issues/"+created.Key+"/artifacts", map[string]string{"name": name, "content": issue.documents[name]}, http.StatusCreated, nil)
	}
	if issue.verification != "" {
		r.person(http.MethodPost, "/api/v1/issues/"+created.Key+"/artifacts",
			map[string]string{"name": "verification.md", "content": issue.verification}, http.StatusCreated, nil)
	}
	status := "in_progress"
	if issue.reviewing {
		status = "needs_review"
	}
	r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
	r.issues = append(r.issues, walkIssue{key: created.Key, title: issue.title, number: issue.number})
	return created
}

// seedIssue seeds the walk's issue, which Dispatch made as created, in the daemon's store as issue
// says. A root is seeded as newStepRecordsRigWith seeds a walk (seed): at admitted, its gate
// approved and its architect's launch unfinished, on its pull request open at its head. A root at
// its review stands in reviewing at generation 1 with every column of this release at its default,
// as the migration leaves a row of the release before. A child stands in reviewing under its
// parent, in its parent's tree, on a pull request of its own, as seedChildInReviewing seeds one.
func (r *doneByChecksRig) seedIssue(issue doneByChecksIssueSpec, created createdIssue) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	pull := stepRecordsPullRequest(r.key, issue.number)
	pull.HeadSHA = issue.head
	if issue.under > 0 {
		parent := r.issues[issue.under-1].key
		if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: parent, Parent: &parent, Project: r.cfg.Project,
				Title: issue.title, Phase: phase.Reviewing, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
				Rank: created.Rank, LastDispatchSeq: created.LastSeq, HandedOver: true,
			}); err != nil {
				return err
			}
			return r.records.PutPullRequest(ctx, tx, pull)
		}); err != nil {
			t.Fatalf("seed %s under %s: %v", r.key, parent, err)
		}
		return
	}
	r.seed(created.Rank, created.LastSeq)
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if issue.reviewing {
			seeded, err := r.records.Issue(ctx, tx, r.key)
			if err != nil || seeded == nil {
				return fmt.Errorf("read the seeded %s: %v", r.key, err)
			}
			seeded.Title, seeded.Phase, seeded.Status, seeded.DispatchStatus = issue.title, phase.Reviewing, "needs_review", "needs_review"
			if err := r.records.PutIssue(ctx, tx, *seeded); err != nil {
				return err
			}
		}
		return r.records.PutPullRequest(ctx, tx, pull)
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
}

// walk runs one step of the walk, which returns once the issue reached its next phase, and logs
// the phase the daemon's state route then shows, the barrier the step waited on.
func (r *doneByChecksRig) walk(step string, run func()) {
	r.t.Helper()
	run()
	r.t.Logf("the walk: %s done; the daemon's state route shows %s in %s", step, r.key, r.d.state().Issues[r.key].Phase)
}

// walkToMerging walks the walk's issue, a root, from its first step to merging on its pull request
// number at head (the steps below), each step's barrier the phase the issue reaches.
func (r *doneByChecksRig) walkToMerging(number int, head string) {
	r.t.Helper()
	r.walkPlanning()
	r.walkImplementing(head)
	r.walkTesting()
	r.walkReview(number, head)
	r.walkSignOff()
	r.walkRetro(head)
}

// walkPlanning is the planner taking its task and completing with a plan written to Dispatch.
func (r *doneByChecksRig) walkPlanning() {
	r.t.Helper()
	r.walk("planning", func() {
		r.stepOn(claim.RolePlanner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	})
}

// walkImplementing is one round of the implementer at head, the head of the pull request the
// issue's work stands on, which must be open on GitHub (HANDOFF_NO_PULL_REQUEST).
func (r *doneByChecksRig) walkImplementing(head string) {
	r.t.Helper()
	r.walk("implementing at "+head, func() {
		r.stepOn(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: head}, phase.Testing)
	})
}

// walkTesting is one round of the tester, which passes.
func (r *doneByChecksRig) walkTesting() {
	r.t.Helper()
	r.walk("testing", func() { r.stepOn(claim.RoleTester, "test", testCompletion, phase.Reviewing) })
}

// walkReview is the reviewer's round and a person's approval of pull request number at head.
func (r *doneByChecksRig) walkReview(number int, head string) {
	r.t.Helper()
	r.walk(fmt.Sprintf("an approving review of #%d at %s", number, head), func() { r.reviewOf(r.key, number, head, "APPROVED", phase.Accepting) })
}

// walkSignOff is the tree architect's sign-off of the walk's issue.
func (r *doneByChecksRig) walkSignOff() {
	r.t.Helper()
	r.walk("the architect's sign-off", r.accept)
}

// walkRetro is the implementer's retro at head, which rates the plan.
func (r *doneByChecksRig) walkRetro(head string) {
	r.t.Helper()
	r.walk("the retro at "+head, func() { r.stepOn(claim.RoleImplementer, "", r.retroAt(head), phase.Merging) })
}

// stepOn is one role's turn of the walk on its issue: it takes the role's next task (taskOf) and
// finishes it (finish).
func (r *doneByChecksRig) stepOn(role claim.Role, word string, req api.HandoffCompleteRequest, next phase.Phase) {
	r.t.Helper()
	c := r.worker(role)
	r.taskOf(c)
	r.finish(c, word, req, next)
}

// retroAt is the implementer's completion of its retro at head, which rates the plan 4.
func (r *doneByChecksRig) retroAt(head string) api.HandoffCompleteRequest {
	req := r.retroCompletion(&record.PlanRating{Rating: 4, Reason: "The plan fit the work."})
	req.Commit = head
	return req
}

// reviewOf is the reviewer of issue, a member of the walk's tree, taking its task and ending its
// turn, and a person submitting state on pull request number at head; issue then reaches next.
func (r *doneByChecksRig) reviewOf(issue string, number int, head, state string, next phase.Phase) {
	r.t.Helper()
	c := r.workerOf(issue, claim.RoleReviewer)
	r.taskOf(c)
	c.endTurn()
	r.publishReviewAt(number, head, state)
	r.waitPhaseOf(issue, next)
}

// retroOf is the implementer's retro of issue, a child of the walk's tree, at head: it takes its
// task and completes it, and the child reaches merging.
func (r *doneByChecksRig) retroOf(issue, head string) {
	t := r.t
	t.Helper()
	c := r.workerOf(issue, claim.RoleImplementer)
	r.taskOf(c)
	if status, body := r.complete(c, r.retroAt(head)); status != http.StatusOK {
		t.Fatalf("the retro of %s = %d %s, want 200", issue, status, body)
	}
	c.endTurn()
	r.waitPhaseOf(issue, phase.Merging)
	t.Logf("the walk: the retro of %s at %s done; the daemon's state route shows it in %s", issue, head, r.d.state().Issues[issue].Phase)
}

// publishReviewAt is a person with write access submitting state on pull request number at head,
// as Envoy relays GitHub's review (publishReview at a head of the walk's own).
func (r *doneByChecksRig) publishReviewAt(number int, head, state string) {
	t := r.t
	t.Helper()
	r.reviews++
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "state": state,
		"commit_id": head, "head_sha": head, "author": stepRecordsPerson, "body": "round " + strconv.Itoa(r.reviews),
		"review_id": strconv.Itoa(number*10 + r.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), number)
	r.publish(subject, "github", fmt.Sprintf("review-%d-%d", number, r.reviews), string(payload))
}

// taskOf waits for the daemon's next task of c's claim and starts the turn that works it, as take
// does, and answers the prompt that carried it. A claim the daemon launched again since c's pane
// came up is met in its new pane first (attach), so the task that launch carries reaches the walk.
// The barrier is the prompt the pane acknowledged.
func (r *doneByChecksRig) taskOf(c *stepClaimant) shimwire.Prompt {
	r.t.Helper()
	return r.taskWithin(c, stepRecordsWait)
}

// taskWithin is taskOf bounded by bound: a look the daemon starts at the wake a pending named waits
// at least workflow.CheckWakeFloor for it.
func (r *doneByChecksRig) taskWithin(c *stepClaimant, bound time.Duration) shimwire.Prompt {
	t := r.t
	t.Helper()
	var prompt shimwire.Prompt
	if !testwait.Holds(bound, 50*time.Millisecond, func() bool {
		select {
		case prompt = <-c.prompts:
			return true
		default:
		}
		if lastLaunch(t, r.rt, c.token).BootToken != c.bootToken {
			r.attach(c)
		}
		return false
	}) {
		held := r.d.claim(c.token)
		t.Fatalf("%s was told no task within %s: the daemon holds its claim %s at generation %d", c.token, bound, held.State, held.Generation)
	}
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	return prompt
}

// attach is c's pane coming up in its claim's latest launch: a resume of c's own session, or a
// spawn of a session of its own, which c holds from then on. It says hello with that launch's boot
// token, registers in that session and says ready, as the plugin does (ownerStepsRig's reattach,
// which takes a resume alone).
func (r *doneByChecksRig) attach(c *stepClaimant) {
	t := r.t
	t.Helper()
	launch := lastLaunch(t, r.rt, c.token)
	file := "/sessions/" + c.session + ".jsonl"
	if launch.ResumeSessionFile != file {
		c.session = "ses_" + string(c.role) + "_" + randomSuffix(t)[:6]
		file = "/sessions/" + c.session + ".jsonl"
	}
	t.Logf("the daemon launched %s again (%q resumed); its pane comes up in session %s", c.token, launch.ResumeSessionFile, c.session)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c.mu.Lock()
	_ = c.sh.conn.Close()
	c.mu.Unlock()
	sh := dialShimUntilAcked(t, address, launch.BootToken)
	c.mu.Lock()
	c.sh, c.bootToken, c.open, c.inTurn = sh, launch.BootToken, nil, false
	c.mu.Unlock()
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: c.session, OmpSessionFile: file, AgentID: c.session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s again in session %s = %d %s, want 200", c.token, c.session, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", c.token, err)
	}
	c.secret = registered.Secret
	d := r.d
	sh.serve(func() { c.serve(sh, d) })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", c.token, status, body)
	}
}

// merge is mergeOf the walk's issue's pull request as doneByChecksMergeCommit, M, which it answers.
func (r *doneByChecksRig) merge() string {
	r.t.Helper()
	r.mergeOf(r.key, r.number, doneByChecksMergeCommit)
	return doneByChecksMergeCommit
}

// mergeOf waits for the launch of the merger of issue, merges pull request number on GitHub as
// commit, publishes GitHub's closed and merged of it and waits for the daemon's processed_events
// row of that envelope (mergeFact), whose time it answers. The merger never registers, so no
// completion of it lands.
func (r *doneByChecksRig) mergeOf(issue string, number int, commit string) time.Time {
	t := r.t
	t.Helper()
	merger := r.tokenOf(issue, claim.RoleMerger)
	r.awaitLaunch(merger)
	t.Logf("the merger launched: the fake runtime's launch of %s", merger)
	r.standIn.merge(number, commit)
	return r.mergeFact(number)
}

// mergeAndComplete is the merger of the walk's issue at work: it takes its task (taskOf), GitHub
// merges pull request number as commit, the daemon applies the merge (mergeFact), and the merger
// completes and ends its turn. It answers the merge fact's time, and the daemon's status and body
// for the completion, which lands in verifying once the merge has moved the issue there.
func (r *doneByChecksRig) mergeAndComplete(number int, commit string) (time.Time, int, string) {
	t := r.t
	t.Helper()
	merger := r.worker(claim.RoleMerger)
	r.taskOf(merger)
	t.Logf("the merger took its task: %s in session %s", merger.token, merger.session)
	r.standIn.merge(number, commit)
	at := r.mergeFact(number)
	status, body := r.complete(merger, api.HandoffCompleteRequest{Summary: "merged #" + strconv.Itoa(number)})
	merger.endTurn()
	t.Logf("the merger's completion of #%d after the merge = %d %s", number, status, body)
	return at, status, body
}

// mergeFact publishes GitHub's closed and merged of pull request number, as the stand-in holds it
// now, and waits for the daemon's processed_events row of that envelope, which the intake writes
// only in the transaction that applies the merge's fact. It answers the row's time.
func (r *doneByChecksRig) mergeFact(number int) time.Time {
	t := r.t
	t.Helper()
	pull, _ := r.standIn.pullRequest(number)
	event := r.publishPull(number, "closed")
	processedAt := r.awaitProcessed("github", event)
	t.Logf("the merge fact applied: processed_events row (source github, event_id %s, processed_at %s) of GitHub's merge of #%d at %s, head %s, onto main",
		event, processedAt.UTC().Format(time.RFC3339Nano), number, pull.merge, pull.head)
	return processedAt
}

// awaitProcessed waits for the daemon's processed_events row of event from source and answers its
// time.
func (r *doneByChecksRig) awaitProcessed(source, event string) time.Time {
	t := r.t
	t.Helper()
	var processedAt time.Time
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		err := r.st.Pool().QueryRow(context.Background(),
			"select processed_at from processed_events where source = $1 and event_id = $2", source, event).Scan(&processedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read the processed event %s of %s: %v", event, source, err)
		}
		return err == nil
	}) {
		t.Fatalf("the daemon wrote no processed_events row of %s event %s within %s", source, event, stepRecordsWait)
	}
	return processedAt
}

// publishPullRequest is publishPull of the walk's issue's pull request.
func (r *doneByChecksRig) publishPullRequest(action string) string {
	r.t.Helper()
	return r.publishPull(r.number, action)
}

// publishPull publishes GitHub's pull_request event of action on pull request number, as the
// stand-in holds it now, on notifications.github.acme.widgets.pr.<n>: the subject Dispatch's
// intake and the daemon's both read. It carries the fields Envoy's listener relays
// (contracts.githubPayload), the head repository Dispatch holds the attribution to among them, and
// a merge's commit, time and author. It answers the envelope's event id.
func (r *doneByChecksRig) publishPull(number int, action string) string {
	r.t.Helper()
	pull, held := r.standIn.pullRequest(number)
	if !held {
		r.t.Fatalf("GitHub holds no pull request #%d to publish %s of", number, action)
	}
	fields := map[string]string{
		"action": action, "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(pull.number), "title": doneByChecksTitle,
		"url": pull.url(), "body": "", "author": stepRecordsPerson, "head_ref": pull.branch, "head_sha": pull.head,
		"head_repo": liveRepo, "base_ref": "main", "merged": strconv.FormatBool(pull.merged),
		"updated_at": pull.updatedAt.UTC().Format(time.RFC3339),
	}
	if pull.merged {
		fields["merge_commit_sha"], fields["merged_by"] = pull.merge, stepRecordsPerson
		fields["merged_at"] = pull.mergedAt.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		r.t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), pull.number)
	return r.publish(subject, "github", fmt.Sprintf("pr-%d-%s", pull.number, action), string(payload))
}

// record records verdict of doneByChecksCheck at commit (recordCheck).
func (r *doneByChecksRig) record(verdict, commit string) string {
	r.t.Helper()
	return r.recordCheck(doneByChecksCheck, verdict, commit)
}

// recordCheck records verdict of check, a check of the walk's issue, at commit through Dispatch's
// results route, as the tester's session with the agent bearer, and answers the result's id.
func (r *doneByChecksRig) recordCheck(check, verdict, commit string) string {
	t := r.t
	t.Helper()
	tester, ok := r.claims[r.token(claim.RoleTester)]
	if !ok {
		t.Fatalf("no tester of %s registered before its result", r.key)
	}
	status, answer := r.agent(http.MethodPost, "/api/v1/issues/"+r.key+"/results", map[string]any{
		"actor": map[string]string{"kind": "session", "id": tester.session}, "check": check, "role": "tester",
		"verdict": verdict, "commit": commit, "surface": "cli", "run": "go test ./internal/widgets/", "observed": "the widgets reported " + verdict,
	})
	if status != http.StatusCreated {
		t.Fatalf("record a %s of %s at %s = %d %s, want 201", verdict, check, commit, status, answer)
	}
	var recorded struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(answer, &recorded); err != nil || recorded.Result.ID == "" {
		t.Fatalf("decode the recorded result %s: %v", answer, err)
	}
	t.Logf("recorded a %s of %s of %s at %s as the tester (%s): result %s", verdict, check, r.key, commit, tester.session, recorded.Result.ID)
	return recorded.Result.ID
}

// agent sends one request to Dispatch with the agent bearer, as a role's session sends one, and
// answers the status and the body.
func (r *doneByChecksRig) agent(method, path string, body any) (int, []byte) {
	t := r.t
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	return r.send(request)
}

// doneByChecksAnswer is the issue's check as GET /api/v1/issues/{key}/results answers it: what
// Step 2's acceptance reads of it, and the parts the log shows as Dispatch wrote them (raw).
type doneByChecksAnswer struct {
	PassStands struct {
		Answer   string                       `json:"answer"`
		Reason   string                       `json:"reason"`
		Results  []string                     `json:"results"`
		Commit   string                       `json:"commit"`
		Subjects []dispatch.PassStandsSubject `json:"subjects"`
	} `json:"pass_stands"`
	ComparedWith []struct {
		Kind        string `json:"kind"`
		Repo        string `json:"repo"`
		Branch      string `json:"branch"`
		MergeCommit string `json:"merge_commit"`
	} `json:"compared_with"`
	raw struct {
		PassStands      json.RawMessage `json:"pass_stands"`
		ComparedWith    json.RawMessage `json:"compared_with"`
		ComparedNothing string          `json:"compared_nothing"`
	}
}

// String is the answer as the log shows it: pass_stands and compared_with as Dispatch wrote them.
func (a doneByChecksAnswer) String() string {
	shown := fmt.Sprintf("pass_stands %s compared_with %s", a.raw.PassStands, a.raw.ComparedWith)
	if a.raw.ComparedNothing != "" {
		shown += " compared_nothing " + a.raw.ComparedNothing
	}
	return shown
}

// answer reads the issue's results from Dispatch and answers doneByChecksCheck (answerOf).
func (r *doneByChecksRig) answer() (doneByChecksAnswer, error) { return r.answerOf(doneByChecksCheck) }

// answerOf reads the walk's issue's results from Dispatch and answers check.
func (r *doneByChecksRig) answerOf(check string) (doneByChecksAnswer, error) {
	var read struct {
		Checks []json.RawMessage `json:"checks"`
	}
	if err := r.personRead("/api/v1/issues/"+r.key+"/results", &read); err != nil {
		return doneByChecksAnswer{}, err
	}
	for _, raw := range read.Checks {
		var block struct {
			BlockID string `json:"block_id"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return doneByChecksAnswer{}, err
		}
		if block.BlockID != check {
			continue
		}
		var answer doneByChecksAnswer
		if err := json.Unmarshal(raw, &answer); err != nil {
			return doneByChecksAnswer{}, err
		}
		if err := json.Unmarshal(raw, &answer.raw); err != nil {
			return doneByChecksAnswer{}, err
		}
		return answer, nil
	}
	return doneByChecksAnswer{}, fmt.Errorf("the results of %s hold no check %s", r.key, check)
}

// awaitAnswer is awaitAnswerOf doneByChecksCheck.
func (r *doneByChecksRig) awaitAnswer(what string, hold func(doneByChecksAnswer) error) {
	r.t.Helper()
	r.awaitAnswerOf(doneByChecksCheck, what, hold)
}

// awaitAnswerOf reads the walk's issue's results until Dispatch's answer for check holds hold, and
// logs the last answer read: Dispatch's answer after what. An answer that never holds within
// stepRecordsWait is an error of the test, naming why, and the walk goes on, so the log shows each
// later answer as well.
func (r *doneByChecksRig) awaitAnswerOf(check, what string, hold func(doneByChecksAnswer) error) {
	r.t.Helper()
	var last doneByChecksAnswer
	var why error
	held := testwait.Holds(stepRecordsWait, time.Second, func() bool {
		answer, err := r.answerOf(check)
		if err != nil {
			why = err
			return false
		}
		last, why = answer, hold(answer)
		return why == nil
	})
	r.t.Logf("Dispatch's answer for %s after %s: %s", check, what, last)
	if !held {
		r.t.Errorf("Dispatch's answer for %s after %s does not hold within %s: %v", check, what, stepRecordsWait, why)
	}
}

// passStandsAt is Step 2's acceptance of the answer after the pass at merge: the check answers yes,
// and the compared_with entry of main names merge as its merge_commit.
func passStandsAt(merge string) func(doneByChecksAnswer) error {
	return func(answer doneByChecksAnswer) error {
		if answer.PassStands.Answer != "yes" {
			return fmt.Errorf("pass_stands.answer is %q, want yes", answer.PassStands.Answer)
		}
		for _, basis := range answer.ComparedWith {
			if basis.Kind != "branch" || basis.Branch != "main" || (basis.Repo != "" && basis.Repo != liveRepo) {
				continue
			}
			if basis.MergeCommit != merge {
				return fmt.Errorf("the compared_with entry of main names merge_commit %q, want %s", basis.MergeCommit, merge)
			}
			return nil
		}
		return errors.New("compared_with names no entry of branch main")
	}
}

// failDecides is Step 2's acceptance of the answer after the fail recorded as result fail at
// commit: the check answers no for reason fail, its results name the fail, and its commit is
// commit.
func failDecides(fail, commit string) func(doneByChecksAnswer) error {
	return func(answer doneByChecksAnswer) error {
		stands := answer.PassStands
		switch {
		case stands.Answer != "no" || stands.Reason != "fail":
			return fmt.Errorf("pass_stands answers %q for reason %q, want no for reason fail", stands.Answer, stands.Reason)
		case !slices.Contains(stands.Results, fail):
			return fmt.Errorf("pass_stands.results are %v, want them to name the fail %s", stands.Results, fail)
		case stands.Commit != commit:
			return fmt.Errorf("pass_stands.commit is %q, want %s", stands.Commit, commit)
		}
		return nil
	}
}

// awaitAttribution reads the issue's events from Dispatch until one says Dispatch attributed the
// issue's pull request to the issue, and logs it. An attribution that never comes within
// stepRecordsWait is an error of the test, and the walk goes on.
func (r *doneByChecksRig) awaitAttribution() {
	t := r.t
	t.Helper()
	type attribution struct {
		Seq     int    `json:"seq"`
		Type    string `json:"type"`
		Payload struct {
			Repo   string  `json:"repo"`
			Number int     `json:"number"`
			From   *string `json:"from"`
			To     *string `json:"to"`
			Source string  `json:"source"`
		} `json:"payload"`
	}
	var found attribution
	var why error
	held := testwait.Holds(stepRecordsWait, time.Second, func() bool {
		const page = 200
		for after := 0; ; {
			var events []attribution
			if err := r.personRead(fmt.Sprintf("/api/v1/issues/%s/events?after=%d&limit=%d", r.key, after, page), &events); err != nil {
				why = err
				return false
			}
			for _, event := range events {
				after = event.Seq
				if event.Type == "issue.pull_request_attribution_changed" && event.Payload.Repo == liveRepo &&
					event.Payload.Number == r.number && event.Payload.To != nil && *event.Payload.To == r.key {
					found = event
					return true
				}
			}
			if len(events) < page {
				why = fmt.Errorf("the events of %s up to seq %d hold no attribution of #%d to it", r.key, after, r.number)
				return false
			}
		}
	})
	if !held {
		t.Errorf("Dispatch attributed pull request #%d to %s nowhere in its events within %s: %v", r.number, r.key, stepRecordsWait, why)
		return
	}
	from := "nobody"
	if found.Payload.From != nil {
		from = *found.Payload.From
	}
	t.Logf("the pull request attributed: %s event %d of %s moves %s#%d from %s to %s, source %s",
		found.Type, found.Seq, r.key, found.Payload.Repo, found.Payload.Number, from, *found.Payload.To, found.Payload.Source)
}

// doneByChecksEvent is one event of an issue's log as GET /api/v1/issues/{key}/events answers it:
// its id, which names the envelope Dispatch publishes it in (dispatch-<id>), its sequence on the
// issue, its type and its payload.
type doneByChecksEvent struct {
	ID      int64           `json:"id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// eventOf is the first event of issue's log after seq after that match holds for, and whether one
// does.
func (r *doneByChecksRig) eventOf(issue string, after int64, match func(doneByChecksEvent) bool) (doneByChecksEvent, bool, error) {
	const page = 200
	for {
		var events []doneByChecksEvent
		if err := r.personRead(fmt.Sprintf("/api/v1/issues/%s/events?after=%d&limit=%d", issue, after, page), &events); err != nil {
			return doneByChecksEvent{}, false, err
		}
		for _, event := range events {
			after = event.Seq
			if match(event) {
				return event, true, nil
			}
		}
		if len(events) < page {
			return doneByChecksEvent{}, false, nil
		}
	}
}

// awaitApplied waits for the daemon to apply Dispatch's event of issue that match holds for, what:
// its processed_events row (source dispatch, the envelope's dedupe key dispatch-<id>), which the
// intake writes only in the transaction that applies the event's fact. It logs the row and answers
// its time.
func (r *doneByChecksRig) awaitApplied(what, issue string, match func(doneByChecksEvent) bool) time.Time {
	t := r.t
	t.Helper()
	var event doneByChecksEvent
	var processedAt time.Time
	var why error
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		if event.ID == 0 {
			found, held, err := r.eventOf(issue, 0, match)
			if err != nil || !held {
				why = fmt.Errorf("Dispatch's log of %s holds no such event yet: %v", issue, err)
				return false
			}
			event = found
		}
		err := r.st.Pool().QueryRow(context.Background(),
			"select processed_at from processed_events where source = 'dispatch' and event_id = $1", fmt.Sprintf("dispatch-%d", event.ID)).Scan(&processedAt)
		if err != nil {
			why = fmt.Errorf("no processed_events row of dispatch-%d (%s, seq %d): %v", event.ID, event.Type, event.Seq, err)
			return false
		}
		return true
	}) {
		t.Fatalf("the daemon applied no event of %s for %s within %s: %v", issue, what, stepRecordsWait, why)
	}
	t.Logf("%s applied: processed_events row (source dispatch, event_id dispatch-%d, processed_at %s) of %s event %d of %s, payload %s",
		what, event.ID, processedAt.UTC().Format(time.RFC3339Nano), event.Type, event.Seq, issue, event.Payload)
	return processedAt
}

// awaitResultFact waits for the daemon to apply Dispatch's result.recorded of result on the walk's
// issue (awaitApplied): the DispatchResultChanged fact. It answers the row's time.
func (r *doneByChecksRig) awaitResultFact(result string) time.Time {
	r.t.Helper()
	return r.awaitApplied("the DispatchResultChanged fact of result "+result, r.key, func(event doneByChecksEvent) bool {
		var payload struct {
			ResultID string `json:"result_id"`
		}
		return event.Type == "result.recorded" && json.Unmarshal(event.Payload, &payload) == nil && payload.ResultID == result
	})
}

// doneByChecksRead is one read of an issue's checks as the daemon's store keeps it (close_reads,
// the CloseChecksRead fact the outbox's read carried back): the generation it was read for, when
// its row began its first Dispatch call, when the workflow stored it, and what Dispatch answered.
type doneByChecksRead struct {
	issue      string
	generation uint64
	readAt     time.Time
	recordedAt time.Time
	answer     record.CheckReadAnswer
}

// check is the read's check id, and whether the read holds it.
func (read doneByChecksRead) check(id string) (dispatch.CheckBlock, bool) {
	for _, block := range read.answer.Results.Checks {
		if block.BlockID == id {
			return block, true
		}
	}
	return dispatch.CheckBlock{}, false
}

// child is the read's Dispatch child key, and whether the read lists it.
func (read doneByChecksRead) child(key string) (dispatch.IssueChild, bool) {
	for _, child := range read.answer.Children {
		if child.Key == key {
			return child, true
		}
	}
	return dispatch.IssueChild{}, false
}

// String is the read as the log shows it: its times and generation, each check with Dispatch's
// answer and reason, the merge commit its branch basis of main names and each subject's head with
// its answer, and each child with its status.
func (read doneByChecksRead) String() string {
	var text strings.Builder
	fmt.Fprintf(&text, "read of %s at generation %d, read_at %s, recorded_at %s:", read.issue, read.generation,
		read.readAt.UTC().Format(time.RFC3339Nano), read.recordedAt.UTC().Format(time.RFC3339Nano))
	for _, block := range read.answer.Results.Checks {
		fmt.Fprintf(&text, " %s (%s) %s", block.BlockID, block.When, strings.Trim(block.PassStands.Answer+"/"+block.PassStands.Reason, "/"))
		if merge := mainMergeCommit(block); merge != "" {
			fmt.Fprintf(&text, " main merge_commit %s", merge)
		}
		for _, subject := range block.PassStands.Subjects {
			fmt.Fprintf(&text, " subject %s %s", subject.Basis.Commit, strings.Trim(subject.Answer+"/"+subject.Reason, "/"))
		}
		text.WriteString(";")
	}
	for _, child := range read.answer.Children {
		fmt.Fprintf(&text, " child %s %s;", child.Key, child.Status)
	}
	return text.String()
}

// mainMergeCommit is the merge_commit block's branch basis of main names, "" for none.
func mainMergeCommit(block dispatch.CheckBlock) string {
	for _, basis := range block.ComparedWith {
		if basis.Kind == dispatch.BasisBranch && basis.Branch == "main" && (basis.Repo == "" || basis.Repo == liveRepo) {
			return basis.MergeCommit
		}
	}
	return ""
}

// subjectAt is block's subject whose basis names commit, and whether it has one.
func subjectAt(block dispatch.CheckBlock, commit string) (dispatch.PassStandsSubject, bool) {
	for _, subject := range block.PassStands.Subjects {
		if subject.Basis.Commit == commit {
			return subject, true
		}
	}
	return dispatch.PassStandsSubject{}, false
}

// awaitCloseRead waits for the daemon's read of issue's checks (close_reads: the CloseChecksRead
// fact's row) that began after after and answered, whose answer holds hold, and logs it: the read
// after what. A read that never comes within doneByChecksReadWait fails the test with the last
// read and why it did not hold.
func (r *doneByChecksRig) awaitCloseRead(issue, what string, after time.Time, hold func(doneByChecksRead) error) doneByChecksRead {
	t := r.t
	t.Helper()
	var read doneByChecksRead
	var why error
	if !testwait.Holds(doneByChecksReadWait, 200*time.Millisecond, func() bool {
		var answer []byte
		got := doneByChecksRead{issue: issue}
		err := r.st.Pool().QueryRow(context.Background(),
			"select generation, read_at, recorded_at, answer from close_reads where issue = $1 and failure = '' and read_at > $2",
			issue, after).Scan(&got.generation, &got.readAt, &got.recordedAt, &answer)
		if err != nil {
			why = fmt.Errorf("no read of %s that answered began after %s: %v", issue, after.UTC().Format(time.RFC3339Nano), err)
			return false
		}
		if err := json.Unmarshal(answer, &got.answer); err != nil {
			why = fmt.Errorf("decode the read of %s: %v", issue, err)
			return false
		}
		read, why = got, hold(got)
		return why == nil
	}) {
		t.Fatalf("the daemon made no read of %s's checks after %s that holds within %s: %v; the last read: %s", issue, what, doneByChecksReadWait, why, read)
	}
	t.Logf("the CloseChecksRead row after %s: %s", what, read)
	return read
}

// awaitIssue waits for issue, as the daemon's state route shows it, to hold hold, and logs it: the
// issue at what. It fails the test with the last answer and why it did not hold.
func (r *doneByChecksRig) awaitIssue(issue, what string, hold func(api.Issue) error) api.Issue {
	t := r.t
	t.Helper()
	var got api.Issue
	var why error
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		shown, ok := r.d.state().Issues[issue]
		if !ok {
			why = fmt.Errorf("the state shows no %s", issue)
			return false
		}
		got, why = shown, hold(shown)
		return why == nil
	}) {
		t.Fatalf("%s at %s does not hold within %s: %v; the state shows %s", issue, what, stepRecordsWait, why, stateOf(got))
	}
	t.Logf("the state route at %s: %s", what, stateOf(got))
	return got
}

// stateOf is issue as the log shows it: its phase, status, generation, slot and what its close
// waits for, as the state route wrote it.
func stateOf(issue api.Issue) string {
	wait, _ := json.Marshal(issue.CloseWait)
	slot := "none"
	if issue.Slot != nil {
		slot = "held"
	}
	return fmt.Sprintf("%s in %s, status %s, generation %d, slot %s, closeWait %s", issue.Key, issue.Phase, issue.Status, issue.Generation, slot, wait)
}

// issueRow is issue as the daemon's store holds it.
func (r *doneByChecksRig) issueRow(issue string) record.Issue {
	t := r.t
	t.Helper()
	var held *record.Issue
	if err := store.TransactWith(context.Background(), r.st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		held, err = r.records.Issue(ctx, tx, issue)
		return err
	}); err != nil || held == nil {
		t.Fatalf("read %s from the store: %v", issue, err)
	}
	return *held
}

// storedWait is the wait issue's row stores (issues.close_wait).
func (r *doneByChecksRig) storedWait(issue string) record.StoredWait {
	r.t.Helper()
	var wait record.StoredWait
	if row := r.issueRow(issue); len(row.CloseWait) > 0 {
		if err := json.Unmarshal(row.CloseWait, &wait); err != nil {
			r.t.Fatalf("decode the stored wait of %s: %v", issue, err)
		}
	}
	return wait
}

// lookOf waits for c, the claimant of the checking role of the walk's issue, to be told look of its
// production check (taskWithin bound), with that look open and its start row done, and starts the
// turn that works it. It answers the check's opening the task names (check_opened_at), which the
// look's report names, read from the task and never from the store.
func (r *doneByChecksRig) lookOf(c *stepClaimant, look int, bound time.Duration) time.Time {
	t := r.t
	t.Helper()
	prompt := r.taskWithin(c, bound)
	_, value, found := strings.Cut(prompt.Message, "check_opened_at `")
	value, _, closed := strings.Cut(value, "`")
	if !found || !closed {
		t.Fatalf("the task of look %d of %s's production check names no check_opened_at", look, r.key)
	}
	opened, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("the check_opened_at of look %d of %s is no RFC 3339 instant: %v", look, r.key, err)
	}
	check := readProductionCheck(t, r.st.Pool(), r.records, r.key)
	if check.Looks != look || !check.LookOpen || !check.Open() || !check.OpenedAt.Equal(opened) {
		t.Fatalf("%s's production check at its task is %+v, want look %d open of the check opened at %s", r.key, check, look, opened)
	}
	var starts int
	if !testwait.Holds(stepRecordsWait, 50*time.Millisecond, func() bool {
		if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
			and (payload->>'checkLook')::int = $2`, r.key, look).Scan(&starts); err != nil {
			t.Fatalf("count the start rows of look %d of %s: %v", look, r.key, err)
		}
		return starts == 0
	}) {
		t.Fatalf("%d start rows of look %d of %s stand in the outbox %s after its task reached %s, want none", starts, look, r.key, stepRecordsWait, c.token)
	}
	t.Logf("look %d of %s's production check (generation %d, merge %s, opened %s) started: its start row done, its task delivered to %s",
		look, r.key, check.Generation, check.MergeSHA, opened.UTC().Format(time.RFC3339Nano), c.token)
	return opened
}

// reportLook is c's report of verdict on look of the walk's issue's production check, which opened
// at opened, and the daemon's processed_events row of it (source api, the report's event id),
// whose time it answers. A pending names wake as its next look. The caller holds c in a turn.
func (r *doneByChecksRig) reportLook(c *stepClaimant, opened time.Time, look int, verdict string, wake *time.Time) time.Time {
	t := r.t
	t.Helper()
	if status, body := r.complete(c, api.HandoffCompleteRequest{Summary: "production reads " + verdict, Verdict: verdict, CheckOpenedAt: &opened, WakeAt: wake}); status != http.StatusOK {
		t.Fatalf("the report of %s on look %d of %s's production check = %d %s, want 200", verdict, look, r.key, status, body)
	}
	event := fmt.Sprintf("check:%s:%d:%d:%s", r.key, opened.UnixMicro(), look, verdict)
	at := r.awaitProcessed("api", event)
	t.Logf("the report applied: processed_events row (source api, event_id %s, processed_at %s)", event, at.UTC().Format(time.RFC3339Nano))
	return at
}

// closeWaitVersions is each version of issue's close-wait.md in Dispatch, oldest first, as its
// markdown: nil where the issue holds no such document.
func (r *doneByChecksRig) closeWaitVersions(issue string) ([]string, error) {
	var artifacts []struct {
		ID       string `json:"id"`
		Slug     string `json:"slug"`
		Versions []struct {
			Number int `json:"number"`
		} `json:"versions"`
	}
	if err := r.personRead("/api/v1/issues/"+issue+"/artifacts", &artifacts); err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		if artifact.Slug != record.CloseWaitDocumentSlug {
			continue
		}
		numbers := make([]int, 0, len(artifact.Versions))
		for _, version := range artifact.Versions {
			numbers = append(numbers, version.Number)
		}
		slices.Sort(numbers)
		texts := make([]string, 0, len(numbers))
		for _, number := range numbers {
			var version struct {
				Markdown string `json:"markdown"`
			}
			if err := r.personRead(fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", artifact.ID, number), &version); err != nil {
				return nil, err
			}
			texts = append(texts, version.Markdown)
		}
		return texts, nil
	}
	return nil, nil
}

// awaitCloseWaitDocument waits for issue's close-wait.md in Dispatch to hold, as its newest
// version, the issue's stored wait as the daemon renders it (record.CloseWaitDocument), and logs
// it: the document at what. It answers every version, oldest first.
func (r *doneByChecksRig) awaitCloseWaitDocument(issue, what string) []string {
	t := r.t
	t.Helper()
	var versions []string
	var why error
	if !testwait.Holds(stepRecordsWait, 200*time.Millisecond, func() bool {
		rendered, err := record.CloseWaitDocument(r.issueRow(issue))
		if err != nil {
			why = err
			return false
		}
		if versions, err = r.closeWaitVersions(issue); err != nil {
			why = err
			return false
		}
		if len(versions) == 0 || versions[len(versions)-1] != rendered {
			why = fmt.Errorf("Dispatch holds %d versions of %s's close-wait.md, the newest not the stored wait's\n%s", len(versions), issue, rendered)
			return false
		}
		return true
	}) {
		t.Fatalf("close-wait.md of %s at %s: %v", issue, what, why)
	}
	t.Logf("close-wait.md of %s at %s, version %d:\n%s", issue, what, len(versions), versions[len(versions)-1])
	return versions
}

// daemonMessages is how many messages issue holds in Dispatch. The test posts none and its role
// stand-ins post none, so each is the daemon's.
func (r *doneByChecksRig) daemonMessages(issue string) int {
	r.t.Helper()
	var messages []json.RawMessage
	if err := r.personRead("/api/v1/issues/"+issue+"/messages", &messages); err != nil {
		r.t.Fatalf("read the messages of %s: %v", issue, err)
	}
	return len(messages)
}

// controllerNotices is every notice of kind the daemon published to the project's controller
// topic, each payload replaced by the notice's reason.
func (r *doneByChecksRig) controllerNotices(kind string) []listenerPublish {
	r.t.Helper()
	var got []listenerPublish
	for _, published := range r.notices.acceptedOn(notify.ControllerTopic(r.project)) {
		var payload record.Notice
		if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
			r.t.Fatalf("decode the controller notice %q: %v", published.Payload, err)
		}
		if string(payload.Kind) == kind {
			published.Payload = payload.Reason
			got = append(got, published)
		}
	}
	return got
}

// sessionStatus is c's session writing status on issue in Dispatch with the agent bearer, as a
// role's `dispatch` call writes it, and answers the sequence of that write on the issue.
func (r *doneByChecksRig) sessionStatus(issue string, c *stepClaimant, status string) int64 {
	t := r.t
	t.Helper()
	code, body := r.agent(http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{
		"status": status, "actor": map[string]string{"kind": "session", "id": c.session},
	})
	if code != http.StatusOK {
		t.Fatalf("%s's write of %s on %s = %d %s, want 200", c.token, status, issue, code, body)
	}
	var written createdIssue
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("decode %s's write of %s on %s: %v", c.token, status, issue, err)
	}
	t.Logf("%s (session %s) wrote %s on %s in Dispatch: seq %d", c.token, c.session, status, issue, written.LastSeq)
	return written.LastSeq
}

// editDocument is c's session writing content as a new version of issue's document name, over the
// version Dispatch holds now (its whole-document token), as a role's `dispatch` call writes one,
// and answers Dispatch's status and body.
func (r *doneByChecksRig) editDocument(issue string, c *stepClaimant, name, content string) (int, []byte) {
	t := r.t
	t.Helper()
	slug := strings.ReplaceAll(name, ".", "-")
	var current struct {
		Token string `json:"token"`
	}
	if err := r.personRead("/api/v1/issues/"+issue+"/artifacts/"+slug+"/text", &current); err != nil || current.Token == "" {
		t.Fatalf("read the token of %s of %s: %v", name, issue, err)
	}
	return r.agent(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{
		"name": name, "content": content, "actor": map[string]string{"kind": "session", "id": c.session},
		"precondition": map[string]string{"document": current.Token},
	})
}

// The rig's own acceptance (plan Task 11a, Step 2). One issue of the default steps walks to its
// merge, M, and the daemon applies GitHub's merge. Then the test records a pass of the issue's
// after-merge check at M, and reads Dispatch's answer: yes, with main's merge_commit M. Then a fail
// at C1, a later commit of main that holds M, and the answer: no, for reason fail, naming the fail,
// at C1. Dispatch reads GitHub at the rig's stand-in, and the log names each request the stand-in
// was sent. When the issue closes is no part of it: the daemon of main closes it at its merge.
func TestTheDoneByChecksRigDrivesAnIssueToItsMerge(t *testing.T) {
	t.Parallel()
	r := newDoneByChecksRig(t)
	r.walk("planning", r.plan)
	r.walk("implementing", r.implement)
	r.walk("testing", r.test)
	r.walk("an approving review", func() { r.review("APPROVED", phase.Accepting) })
	r.walk("the architect's sign-off", r.accept)
	r.walk("the retro", r.retro)
	merge := r.merge()

	r.record("pass", merge)
	r.awaitAnswer("the pass at "+merge, passStandsAt(merge))
	r.awaitAttribution()

	later := r.standIn.commitOnMain(doneByChecksLaterCommit)
	t.Logf("main moved to %s, whose history holds %s: the stand-in's compare %s...%s answers behind", later, merge, later, merge)
	fail := r.record("fail", later)
	r.awaitAnswer("the fail at "+later, failDecides(fail, later))
	r.standIn.logRequests()
}

// doneByChecksGitHub is GitHub's REST API as the rig's Dispatch and daemon read it, for acme/widgets
// alone: Dispatch's App, its installation and its token, the repository with default branch main,
// each pull request with its commits and the pull requests of a commit, each commit and the compare
// of two, and the refs of main and of each issue's branch. Every other request is answered 404,
// and the test's log names it. Every request is logged with who sent it: the daemon's App token is
// workflowAppToken's, and every other is Dispatch's.
type doneByChecksGitHub struct {
	t   *testing.T
	url string
	mu  sync.Mutex
	// parents is each commit GitHub holds, by its parents: a merge has two, the base none.
	parents map[string][]string
	// refs is each branch's head commit, by the branch's name.
	refs map[string]string
	// pulls is each pull request GitHub holds, by its number, and opened their numbers in the
	// order they opened.
	pulls  map[int]*doneByChecksPullRequest
	opened []int
	// refWrites is each write of a branch's ref GitHub took, in order.
	refWrites []doneByChecksRefWrite
	// served counts the requests a route answered; unserved is each request no route answered,
	// as its caller, method and path.
	served   int
	unserved []string
}

// doneByChecksRefWrite is one write of a branch's ref GitHub took: its method (POST makes the
// branch, PATCH moves it), the branch, the commit it stands at after the write, and who sent it.
type doneByChecksRefWrite struct {
	method, branch, sha, caller string
}

// doneByChecksPullRequest is one pull request as GitHub holds it.
type doneByChecksPullRequest struct {
	number       int
	branch, head string
	merged       bool
	merge        string
	openedAt     time.Time
	updatedAt    time.Time
	mergedAt     time.Time
}

// url is the pull request's page on GitHub.
func (p doneByChecksPullRequest) url() string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, p.number)
}

// answer is the pull request as GitHub's REST API answers it, the list form of a commit's pull
// requests included: open and clean until it merges, closed and merged as merge from then on.
func (p doneByChecksPullRequest) answer() map[string]any {
	answer := map[string]any{
		"number": p.number, "title": doneByChecksTitle, "body": "", "html_url": p.url(),
		"user": map[string]any{"login": stepRecordsPerson}, "labels": []any{map[string]any{"name": "non-task"}},
		"state": "open", "merged": false, "merged_at": nil, "closed_at": nil, "merge_commit_sha": nil,
		"mergeable": true, "mergeable_state": "clean", "additions": 1, "deletions": 0,
		"created_at": p.openedAt.UTC().Format(time.RFC3339), "updated_at": p.updatedAt.UTC().Format(time.RFC3339),
		"head": map[string]any{"ref": p.branch, "sha": p.head, "repo": map[string]any{"full_name": liveRepo}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": liveRepo}},
	}
	if p.merged {
		merged := p.mergedAt.UTC().Format(time.RFC3339)
		answer["state"], answer["merged"], answer["merged_at"], answer["closed_at"] = "closed", true, merged, merged
		answer["merge_commit_sha"], answer["mergeable_state"] = p.merge, "unknown"
	}
	return answer
}

// newDoneByChecksGitHub starts the stand-in with main at doneByChecksBaseCommit and no pull request.
func newDoneByChecksGitHub(t *testing.T) *doneByChecksGitHub {
	t.Helper()
	g := &doneByChecksGitHub{t: t, parents: map[string][]string{doneByChecksBaseCommit: nil}, refs: map[string]string{"main": doneByChecksBaseCommit},
		pulls: map[int]*doneByChecksPullRequest{}}
	repo := "/repos/" + liveRepo
	routes := http.NewServeMux()
	routes.HandleFunc("GET /app/installations", g.installations)
	routes.HandleFunc("GET "+repo+"/installation", g.installation)
	routes.HandleFunc("POST /app/installations/{id}/access_tokens", g.accessToken)
	routes.HandleFunc("GET /installation/repositories", g.repositories)
	routes.HandleFunc("GET "+repo, g.repository)
	routes.HandleFunc("GET "+repo+"/pulls", g.pullsRoute)
	routes.HandleFunc("GET "+repo+"/pulls/{number}", g.pullRequestRoute)
	routes.HandleFunc("GET "+repo+"/pulls/{number}/commits", g.pullCommits)
	routes.HandleFunc("GET "+repo+"/pulls/{number}/reviews", g.reviews)
	routes.HandleFunc("GET "+repo+"/commits/{ref}", g.commit)
	routes.HandleFunc("GET "+repo+"/commits/{sha}/pulls", g.commitPulls)
	routes.HandleFunc("GET "+repo+"/compare/{basehead}", g.compare)
	routes.HandleFunc("GET "+repo+"/git/ref/heads/{branch...}", g.ref)
	routes.HandleFunc("GET "+repo+"/git/refs/heads/{branch...}", g.ref)
	routes.HandleFunc("PATCH "+repo+"/git/refs/heads/{branch...}", g.updateRef)
	routes.HandleFunc("POST "+repo+"/git/refs", g.createRef)
	routes.HandleFunc("GET "+repo+"/collaborators/{user}/permission", g.permission)
	routes.HandleFunc("POST /graphql", g.graphql)
	routes.HandleFunc("/", g.noRoute)
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		answered := &doneByChecksStatus{ResponseWriter: w, status: http.StatusOK}
		routes.ServeHTTP(answered, r)
		t.Logf("github stand-in: %s %s %s answered %d", standInCaller(r), r.Method, r.URL.RequestURI(), answered.status)
	}})
	return g
}

// doneByChecksStatus is a response writer that keeps the status its handler wrote.
type doneByChecksStatus struct {
	http.ResponseWriter
	status int
}

func (a *doneByChecksStatus) WriteHeader(status int) {
	a.status = status
	a.ResponseWriter.WriteHeader(status)
}

// standInCaller is who sent r: the daemon, whose App leases carry workflowAppToken, or Dispatch,
// whose App signs a JWT and mints doneByChecksAppToken.
func standInCaller(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+workflowAppToken) {
		return "daemon"
	}
	return "dispatch"
}

// write answers body as JSON with status, and counts one request a route answered.
func (g *doneByChecksGitHub) write(w http.ResponseWriter, status int, body any) {
	g.mu.Lock()
	g.served++
	g.mu.Unlock()
	encoded, err := json.Marshal(body)
	if err != nil {
		g.t.Errorf("encode the stand-in's answer: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// noRoute answers a request no route serves 404, and keeps it for the log.
func (g *doneByChecksGitHub) noRoute(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.unserved = append(g.unserved, standInCaller(r)+" "+r.Method+" "+r.URL.RequestURI())
	g.mu.Unlock()
	g.t.Logf("github stand-in: no route for %s %s from %s: 404", r.Method, r.URL.RequestURI(), standInCaller(r))
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

// logRequests logs how many requests the stand-in was sent, how many a route answered, and each
// that no route answered.
func (g *doneByChecksGitHub) logRequests() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.unserved) == 0 {
		g.t.Logf("github stand-in: %d requests, each answered by a route", g.served)
		return
	}
	g.t.Logf("github stand-in: %d requests, %d answered by a route, %d with no route, each answered 404: %s",
		g.served+len(g.unserved), g.served, len(g.unserved), strings.Join(g.unserved, "; "))
}

// doneByChecksInstallation is the App's one installation, on acme, as GitHub answers it.
func doneByChecksInstallation() map[string]any {
	return map[string]any{"id": 77, "app_slug": "dispatch-scratch", "account": map[string]any{"login": "acme"}, "permissions": doneByChecksPermissions()}
}

// doneByChecksPermissions is what the App may do on acme/widgets: read each thing Dispatch reads.
func doneByChecksPermissions() map[string]string {
	return map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "statuses": "read", "checks": "read", "metadata": "read"}
}

func (g *doneByChecksGitHub) installations(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, []any{doneByChecksInstallation()})
}

func (g *doneByChecksGitHub) installation(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, doneByChecksInstallation())
}

func (g *doneByChecksGitHub) accessToken(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusCreated, map[string]any{
		"token": doneByChecksAppToken, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "permissions": doneByChecksPermissions(),
	})
}

func (g *doneByChecksGitHub) repositories(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, map[string]any{"total_count": 1, "repositories": []any{map[string]any{"full_name": liveRepo}}})
}

func (g *doneByChecksGitHub) repository(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, map[string]any{"full_name": liveRepo, "default_branch": "main", "private": false})
}

// open is GitHub's pull request number from branch at head, a commit after the branch's tip, or
// after main's head for a branch GitHub does not have yet; branch stands at head from now on.
func (g *doneByChecksGitHub) open(number int, branch, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	parent, held := g.refs[branch]
	if !held {
		parent = g.refs["main"]
	}
	g.parents[head] = []string{parent}
	g.refs[branch] = head
	g.pulls[number] = &doneByChecksPullRequest{number: number, branch: branch, head: head, openedAt: now, updatedAt: now}
	g.opened = append(g.opened, number)
}

// merge is GitHub's merge of pull request number onto main as commit, whose parents are main's
// head and the pull request's; main stands at it from now on. It answers the merge.
func (g *doneByChecksGitHub) merge(number int, commit string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, held := g.pulls[number]
	if !held || pull.merged {
		g.t.Fatalf("GitHub merged pull request #%d, which is not open", number)
	}
	now := time.Now()
	g.parents[commit] = []string{g.refs["main"], pull.head}
	g.refs["main"] = commit
	pull.merged, pull.merge, pull.mergedAt, pull.updatedAt = true, commit, now, now
	return commit
}

// commitOnMain is a commit pushed to main after its head, which main stands at from now on. It
// answers the commit.
func (g *doneByChecksGitHub) commitOnMain(commit string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.parents[commit] = []string{g.refs["main"]}
	g.refs["main"] = commit
	return commit
}

// pullRequest is pull request number as GitHub holds it now, and whether GitHub holds it.
func (g *doneByChecksGitHub) pullRequest(number int) (doneByChecksPullRequest, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, held := g.pulls[number]
	if !held {
		return doneByChecksPullRequest{}, false
	}
	return *pull, true
}

// head is branch's head commit as GitHub holds it now, "" for a branch it does not have.
func (g *doneByChecksGitHub) head(branch string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refs[branch]
}

// refWritesOf is each write of branch's ref GitHub took, in order.
func (g *doneByChecksGitHub) refWritesOf(branch string) []doneByChecksRefWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	var writes []doneByChecksRefWrite
	for _, write := range g.refWrites {
		if write.branch == branch {
			writes = append(writes, write)
		}
	}
	return writes
}

// thePullRequest is the pull request numbered by r's {number}, and whether it is GitHub's.
func (g *doneByChecksGitHub) thePullRequest(r *http.Request) (doneByChecksPullRequest, bool) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return doneByChecksPullRequest{}, false
	}
	return g.pullRequest(number)
}

// pullsRoute is GET /repos/{repo}/pulls: the pull requests of the state the query names (open
// where it names none, as GitHub's default is), of the head branch it names where it names one
// (`owner:branch`), in the order they opened. Every page after the first is empty.
func (g *doneByChecksGitHub) pullsRoute(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if page := query.Get("page"); page != "" && page != "1" {
		g.write(w, http.StatusOK, []any{})
		return
	}
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	_, branch, named := strings.Cut(query.Get("head"), ":")
	g.mu.Lock()
	listed := []any{}
	for _, number := range g.opened {
		pull := g.pulls[number]
		if state == "open" && pull.merged || state == "closed" && !pull.merged || named && pull.branch != branch {
			continue
		}
		listed = append(listed, pull.answer())
	}
	g.mu.Unlock()
	g.write(w, http.StatusOK, listed)
}

func (g *doneByChecksGitHub) pullRequestRoute(w http.ResponseWriter, r *http.Request) {
	pull, held := g.thePullRequest(r)
	if !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, pull.answer())
}

// pullCommits is the pull request's one commit, its head, written when it opened.
func (g *doneByChecksGitHub) pullCommits(w http.ResponseWriter, r *http.Request) {
	pull, held := g.thePullRequest(r)
	if !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	at := pull.openedAt.UTC().Format(time.RFC3339)
	g.write(w, http.StatusOK, []any{map[string]any{"sha": pull.head, "commit": map[string]any{
		"message": doneByChecksTitle, "author": map[string]any{"date": at}, "committer": map[string]any{"date": at},
	}}})
}

func (g *doneByChecksGitHub) reviews(w http.ResponseWriter, r *http.Request) {
	if _, held := g.thePullRequest(r); !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, []any{})
}

// resolve is the commit ref names, a branch or a commit, and whether GitHub holds it. Its caller
// holds g.mu.
func (g *doneByChecksGitHub) resolve(ref string) (string, bool) {
	if head, branch := g.refs[ref]; branch {
		return head, true
	}
	_, held := g.parents[ref]
	return ref, held
}

// holds says whether commit's history holds ancestor, commit itself among it. Its caller holds g.mu.
func (g *doneByChecksGitHub) holds(commit, ancestor string) bool {
	if commit == ancestor {
		return true
	}
	for _, parent := range g.parents[commit] {
		if g.holds(parent, ancestor) {
			return true
		}
	}
	return false
}

// commit is GET /repos/{repo}/commits/{ref}: the commit a branch or a commit names, as its SHA alone
// where the caller asks for that media type (HasCommit, Ref), and GitHub's 422 for a commit it does
// not hold.
func (g *doneByChecksGitHub) commit(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	sha, held := g.resolve(r.PathValue("ref"))
	g.mu.Unlock()
	if !held {
		g.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "No commit found for SHA: " + r.PathValue("ref")})
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "vnd.github.sha") {
		g.mu.Lock()
		g.served++
		g.mu.Unlock()
		_, _ = io.WriteString(w, sha)
		return
	}
	g.write(w, http.StatusOK, map[string]string{"sha": sha})
}

// commitPulls is the pull requests GitHub associates with a commit: each pull request whose head
// it is and, once merged, whose merge it is.
func (g *doneByChecksGitHub) commitPulls(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	g.mu.Lock()
	listed := []any{}
	for _, number := range g.opened {
		if pull := g.pulls[number]; sha == pull.head || pull.merged && sha == pull.merge {
			listed = append(listed, pull.answer())
		}
	}
	g.mu.Unlock()
	g.write(w, http.StatusOK, listed)
}

// compare is GET /repos/{repo}/compare/{base}...{head}, each side a branch or a commit: identical
// for one commit, behind where base's history holds head, ahead where head's holds base, and
// diverged otherwise. A side GitHub does not hold is its 404.
func (g *doneByChecksGitHub) compare(w http.ResponseWriter, r *http.Request) {
	baseRef, headRef, cut := strings.Cut(r.PathValue("basehead"), "...")
	g.mu.Lock()
	base, baseHeld := g.resolve(baseRef)
	head, headHeld := g.resolve(headRef)
	status := "diverged"
	switch {
	case base == head:
		status = "identical"
	case g.holds(base, head):
		status = "behind"
	case g.holds(head, base):
		status = "ahead"
	}
	g.mu.Unlock()
	if !cut || !baseHeld || !headHeld {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, map[string]any{"status": status, "base_commit": map[string]string{"sha": base}, "commits": []any{}})
}

// doneByChecksRef is refs/heads/branch at sha, as GitHub answers a ref.
func doneByChecksRef(branch, sha string) map[string]any {
	return map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha, "type": "commit"}}
}

// ref is GET /repos/{repo}/git/ref/heads/{branch}: the branch's head, or GitHub's 404 for a branch
// it does not have.
func (g *doneByChecksGitHub) ref(w http.ResponseWriter, r *http.Request) {
	branch := r.PathValue("branch")
	g.mu.Lock()
	head, held := g.refs[branch]
	g.mu.Unlock()
	if !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, doneByChecksRef(branch, head))
}

// createRef is POST /repos/{repo}/git/refs: a branch made at a commit GitHub holds, or GitHub's 422
// for a branch it already has.
func (g *doneByChecksGitHub) createRef(w http.ResponseWriter, r *http.Request) {
	var made struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&made); err != nil {
		g.write(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	branch, isBranch := strings.CutPrefix(made.Ref, "refs/heads/")
	g.mu.Lock()
	_, exists := g.refs[branch]
	_, held := g.parents[made.SHA]
	if isBranch && !exists && held {
		g.refs[branch] = made.SHA
		g.refWrites = append(g.refWrites, doneByChecksRefWrite{method: http.MethodPost, branch: branch, sha: made.SHA, caller: standInCaller(r)})
	}
	g.mu.Unlock()
	switch {
	case !isBranch || !held:
		g.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "Object does not exist"})
	case exists:
		g.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
	default:
		g.write(w, http.StatusCreated, doneByChecksRef(branch, made.SHA))
	}
}

// updateRef is PATCH /repos/{repo}/git/refs/heads/{branch}: the branch moved to a commit GitHub
// holds, or GitHub's 422 for a branch it does not have.
func (g *doneByChecksGitHub) updateRef(w http.ResponseWriter, r *http.Request) {
	var moved struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&moved); err != nil {
		g.write(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	branch := r.PathValue("branch")
	g.mu.Lock()
	_, exists := g.refs[branch]
	_, held := g.parents[moved.SHA]
	if exists && held {
		g.refs[branch] = moved.SHA
		g.refWrites = append(g.refWrites, doneByChecksRefWrite{method: http.MethodPatch, branch: branch, sha: moved.SHA, caller: standInCaller(r)})
	}
	g.mu.Unlock()
	if !exists || !held {
		g.write(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference does not exist"})
		return
	}
	g.write(w, http.StatusOK, doneByChecksRef(branch, moved.SHA))
}

// permission is the person of the walk's reviews, who may write to acme/widgets.
func (g *doneByChecksGitHub) permission(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("user") != stepRecordsPerson {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, map[string]any{"permission": "write", "user": map[string]string{"login": stepRecordsPerson}})
}

// graphql is the daemon's GraphQL root, which knows no pull request, as stepRecordsGitHub's does.
// Dispatch's GraphQL reads are no route of the stand-in.
func (g *doneByChecksGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	if standInCaller(r) != "daemon" {
		g.noRoute(w, r)
		return
	}
	g.write(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}}})
}
