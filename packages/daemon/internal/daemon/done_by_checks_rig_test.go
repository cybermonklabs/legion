// The rig of the whole-daemon tests of an issue that closes when the checks of its goal pass
// (doneByChecksRig): the walk of step_records_rig_test.go, over a real Dispatch whose events are on
// and whose App reads a GitHub stand-in of the rig's own (doneByChecksGitHub), which the daemon reads
// too. TestTheDoneByChecksRigDrivesAnIssueToItsMerge is the rig's own acceptance: one issue of the
// default steps driven to its merge, then a pass and a fail of its after-merge check recorded, each
// read back from Dispatch. It asserts nothing of when the issue closes.
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

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// doneByChecksProject is the rig's project, in Dispatch and in the daemon, so its one issue is
	// ACME-1 (doneByChecksIssue).
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
)

// doneByChecksVerification is the issue's Verification document: one check, doneByChecksCheck,
// that runs after the merge.
const doneByChecksVerification = "# Verification\n\n:::check{#" + doneByChecksCheck +
	` surface="cli" when="after_merge" run="go test ./internal/widgets/"}` + "\nThe widgets report their merge once it reaches main.\n:::\n"

// doneByChecksRig is stepRecordsRig's walk of one issue, ACME-1, over a Dispatch with its events
// on and the rig's own GitHub (standIn), which Dispatch's App and the daemon both read.
type doneByChecksRig struct {
	*stepRecordsRig
	standIn *doneByChecksGitHub
}

// newDoneByChecksRig starts the GitHub stand-in and a scratch Dispatch whose App reads it, with
// its events on the NATS server the daemon consumes. It makes the project ACME on acme/widgets,
// saves the delivery settings that turn Dispatch's intake of GitHub's events on, and makes ACME-1
// with its Verification document. It seeds the daemon's store as newStepRecordsRigWith seeds a walk:
// the issue admitted, its gate approved, its pull request open at stepRecordsHead. It publishes
// GitHub's opened of that pull request, boots the daemon and registers the tree's architect.
func newDoneByChecksRig(t *testing.T) *doneByChecksRig {
	t.Helper()
	natsURL := workflowNATS(t)
	proof := newLiveProofOn(t, natsURL)
	proof.cfg.Project = doneByChecksProject
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	standIn := newDoneByChecksGitHub(t)
	dispatch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL, github: standIn.url})
	// stepRecordsRig's boot points the daemon at its github's url, which is the rig's own stand-in
	// here. That stand-in holds the pull request, and the rig merges it itself (merge), so the
	// stepRecordsGitHub holds none.
	r := &doneByChecksRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: dispatch,
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
	var created createdIssue
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": doneByChecksTitle}, http.StatusCreated, &created)
	if created.Key != doneByChecksIssue {
		t.Fatalf("Dispatch made the project's first issue %q, want %s", created.Key, doneByChecksIssue)
	}
	// The document is written before the status, so the sequence the daemon is seeded with
	// (created.LastSeq) is past its events.
	r.person(http.MethodPost, "/api/v1/issues/"+created.Key+"/artifacts",
		map[string]string{"name": "verification.md", "content": doneByChecksVerification}, http.StatusCreated, nil)
	r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
	r.issues = []walkIssue{{key: created.Key, title: doneByChecksTitle, number: doneByChecksPull}}
	r.on(created.Key)
	r.standIn.open(r.number, claim.IssueBranch(r.key), stepRecordsHead)
	r.seed(created.Rank, created.LastSeq)
	opened := r.publishPullRequest("opened")
	t.Logf("GitHub opened pull request #%d of %s from %s at %s: envelope %s", r.number, liveRepo, claim.IssueBranch(r.key), stepRecordsHead, opened)
	r.boot()
	r.worker(claim.RoleArchitect)
	return r
}

// walk runs one step of the walk, which returns once the issue reached its next phase, and logs
// the phase the daemon's state route then shows, the barrier the step waited on.
func (r *doneByChecksRig) walk(step string, run func()) {
	r.t.Helper()
	run()
	r.t.Logf("the walk: %s done; the daemon's state route shows %s in %s", step, r.key, r.d.state().Issues[r.key].Phase)
}

// merge waits for the merger's launch, merges the pull request on GitHub as M, publishes GitHub's
// closed and merged of it and waits for the daemon's processed_events row of that envelope, which
// the intake writes only in the transaction that applies the merge's fact. It answers M.
func (r *doneByChecksRig) merge() string {
	t := r.t
	t.Helper()
	merger := r.token(claim.RoleMerger)
	r.awaitLaunch(merger)
	t.Logf("the merger launched: the fake runtime's launch of %s", merger)
	merge := r.standIn.merge()
	event := r.publishPullRequest("closed")
	var processedAt time.Time
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		err := r.st.Pool().QueryRow(context.Background(),
			"select processed_at from processed_events where source = 'github' and event_id = $1", event).Scan(&processedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read the processed event %s: %v", event, err)
		}
		return err == nil
	}) {
		t.Fatalf("the daemon wrote no processed_events row of GitHub's merge %s within %s", event, stepRecordsWait)
	}
	t.Logf("the merge fact applied: processed_events row (source github, event_id %s, processed_at %s) of GitHub's merge of #%d at %s, head %s, onto main",
		event, processedAt.UTC().Format(time.RFC3339Nano), r.number, merge, stepRecordsHead)
	return merge
}

// publishPullRequest publishes GitHub's pull_request event of action on the issue's pull request,
// as the stand-in holds it now, on notifications.github.acme.widgets.pr.<n>: the subject Dispatch's
// intake and the daemon's both read. It carries what each reads, the head repository Dispatch
// holds the attribution to among them, and it answers the envelope's event id.
func (r *doneByChecksRig) publishPullRequest(action string) string {
	r.t.Helper()
	pull := r.standIn.pullRequest()
	fields := map[string]string{
		"action": action, "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(pull.number), "title": doneByChecksTitle,
		"url": pull.url(), "body": "", "author": stepRecordsPerson, "head_ref": pull.branch, "head_sha": pull.head,
		"head_repo": liveRepo, "base_ref": "main", "merged": strconv.FormatBool(pull.merged),
		"updated_at": pull.updatedAt.UTC().Format(time.RFC3339),
	}
	if pull.merged {
		fields["merge_commit_sha"] = pull.merge
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		r.t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), pull.number)
	return r.publish(subject, "github", fmt.Sprintf("pr-%d-%s", pull.number, action), string(payload))
}

// record records verdict of the issue's check at commit through Dispatch's results route, as the
// tester's session with the agent bearer, and answers the result's id.
func (r *doneByChecksRig) record(verdict, commit string) string {
	t := r.t
	t.Helper()
	tester, ok := r.claims[r.token(claim.RoleTester)]
	if !ok {
		t.Fatalf("no tester of %s registered before its result", r.key)
	}
	encoded, err := json.Marshal(map[string]any{
		"actor": map[string]string{"kind": "session", "id": tester.session}, "check": doneByChecksCheck, "role": "tester",
		"verdict": verdict, "commit": commit, "surface": "cli", "run": "go test ./internal/widgets/", "observed": "the widgets reported " + verdict,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.dispatch.base+"/api/v1/issues/"+r.key+"/results", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.send(request)
	if status != http.StatusCreated {
		t.Fatalf("record a %s of %s at %s = %d %s, want 201", verdict, doneByChecksCheck, commit, status, answer)
	}
	var recorded struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(answer, &recorded); err != nil || recorded.Result.ID == "" {
		t.Fatalf("decode the recorded result %s: %v", answer, err)
	}
	t.Logf("recorded a %s of %s at %s as the tester (%s): result %s", verdict, doneByChecksCheck, commit, tester.session, recorded.Result.ID)
	return recorded.Result.ID
}

// doneByChecksAnswer is the issue's check as GET /api/v1/issues/{key}/results answers it: what
// Step 2's acceptance reads of it, and the parts the log shows as Dispatch wrote them (raw).
type doneByChecksAnswer struct {
	PassStands struct {
		Answer  string   `json:"answer"`
		Reason  string   `json:"reason"`
		Results []string `json:"results"`
		Commit  string   `json:"commit"`
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

// answer reads the issue's results from Dispatch and answers its check.
func (r *doneByChecksRig) answer() (doneByChecksAnswer, error) {
	var read struct {
		Checks []json.RawMessage `json:"checks"`
	}
	if err := r.personRead("/api/v1/issues/"+r.key+"/results", &read); err != nil {
		return doneByChecksAnswer{}, err
	}
	for _, raw := range read.Checks {
		var check struct {
			BlockID string `json:"block_id"`
		}
		if err := json.Unmarshal(raw, &check); err != nil {
			return doneByChecksAnswer{}, err
		}
		if check.BlockID != doneByChecksCheck {
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
	return doneByChecksAnswer{}, fmt.Errorf("the results of %s hold no check %s", r.key, doneByChecksCheck)
}

// awaitAnswer reads the issue's results until Dispatch's answer for its check holds hold, and logs
// the last answer read: Dispatch's answer after what. An answer that never holds within
// stepRecordsWait is an error of the test, naming why, and the walk goes on, so the log shows each
// later answer as well.
func (r *doneByChecksRig) awaitAnswer(what string, hold func(doneByChecksAnswer) error) {
	r.t.Helper()
	var last doneByChecksAnswer
	var why error
	held := testwait.Holds(stepRecordsWait, time.Second, func() bool {
		answer, err := r.answer()
		if err != nil {
			why = err
			return false
		}
		last, why = answer, hold(answer)
		return why == nil
	})
	r.t.Logf("Dispatch's answer after %s: %s", what, last)
	if !held {
		r.t.Errorf("Dispatch's answer after %s does not hold within %s: %v", what, stepRecordsWait, why)
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
// the issue's pull request with its commits and the pull requests of a commit, each commit and the
// compare of two, and the refs of main and of the issue's branch. Every other request is answered
// 404, and the test's log names it. Every request is logged with who sent it: the daemon's App
// token is workflowAppToken's, and every other is Dispatch's.
type doneByChecksGitHub struct {
	t   *testing.T
	url string
	mu  sync.Mutex
	// parents is each commit GitHub holds, by its parents: the merge has two, the base none.
	parents map[string][]string
	// refs is each branch's head commit, by the branch's name.
	refs map[string]string
	pull doneByChecksPullRequest
	// served counts the requests a route answered; unserved is each request no route answered,
	// as its caller, method and path.
	served   int
	unserved []string
}

// doneByChecksPullRequest is the issue's pull request as GitHub holds it; opened is false until
// the rig opens it.
type doneByChecksPullRequest struct {
	number       int
	branch, head string
	opened       bool
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
	g := &doneByChecksGitHub{t: t, parents: map[string][]string{doneByChecksBaseCommit: nil}, refs: map[string]string{"main": doneByChecksBaseCommit}}
	repo := "/repos/" + liveRepo
	routes := http.NewServeMux()
	routes.HandleFunc("GET /app/installations", g.installations)
	routes.HandleFunc("GET "+repo+"/installation", g.installation)
	routes.HandleFunc("POST /app/installations/{id}/access_tokens", g.accessToken)
	routes.HandleFunc("GET /installation/repositories", g.repositories)
	routes.HandleFunc("GET "+repo, g.repository)
	routes.HandleFunc("GET "+repo+"/pulls", g.pulls)
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

// open is GitHub's pull request number from branch at head, a commit after main's head, which
// branch stands at.
func (g *doneByChecksGitHub) open(number int, branch, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.parents[head] = []string{g.refs["main"]}
	g.refs[branch] = head
	g.pull = doneByChecksPullRequest{number: number, branch: branch, head: head, opened: true, openedAt: now, updatedAt: now}
}

// merge is GitHub's merge of the pull request onto main as doneByChecksMergeCommit, whose parents
// are main's head and the pull request's; main stands at it from now on. It answers the merge.
func (g *doneByChecksGitHub) merge() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.pull.opened || g.pull.merged {
		g.t.Fatalf("GitHub merged pull request #%d, which is not open", g.pull.number)
	}
	now := time.Now()
	g.parents[doneByChecksMergeCommit] = []string{g.refs["main"], g.pull.head}
	g.refs["main"] = doneByChecksMergeCommit
	g.pull.merged, g.pull.merge, g.pull.mergedAt, g.pull.updatedAt = true, doneByChecksMergeCommit, now, now
	return doneByChecksMergeCommit
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

// pullRequest is the pull request as GitHub holds it now.
func (g *doneByChecksGitHub) pullRequest() doneByChecksPullRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pull
}

// thePullRequest is the pull request numbered by r's {number}, and whether it is GitHub's.
func (g *doneByChecksGitHub) thePullRequest(r *http.Request) (doneByChecksPullRequest, bool) {
	pull := g.pullRequest()
	number, err := strconv.Atoi(r.PathValue("number"))
	return pull, err == nil && pull.opened && number == pull.number
}

func (g *doneByChecksGitHub) pulls(w http.ResponseWriter, _ *http.Request) {
	pull := g.pullRequest()
	if !pull.opened {
		g.write(w, http.StatusOK, []any{})
		return
	}
	g.write(w, http.StatusOK, []any{pull.answer()})
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

// commitPulls is the pull requests GitHub associates with a commit: the pull request for its head
// and, once merged, for its merge.
func (g *doneByChecksGitHub) commitPulls(w http.ResponseWriter, r *http.Request) {
	pull, sha := g.pullRequest(), r.PathValue("sha")
	if pull.opened && (sha == pull.head || pull.merged && sha == pull.merge) {
		g.write(w, http.StatusOK, []any{pull.answer()})
		return
	}
	g.write(w, http.StatusOK, []any{})
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
