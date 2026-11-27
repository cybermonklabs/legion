//go:build e2e

// The live control of the count of returns (C7 of the forward-only design): nine issues in
// three projects walk through real daemons to their retro and their merge, each return is the
// role's own `request_backward_move` through the daemon's route, and the count program reads what
// Dispatch, GitHub and the roles' transcripts then hold. scripts/e2e/forward-count-control.sh
// builds this driver with `-tags e2e`, owns the nats-server and the PostgreSQL it runs against,
// and judges the summaries this run's `collect` makes.
//
// Nothing of the record path is stood in for. Dispatch is the real server this checkout builds
// (startScratchDispatchOn with its events on), on a database of its own. Each project has a whole
// daemon of its own (the daemon serves one project), booted at that Dispatch, so every step
// record and every reason is written by the daemon's own outbox through its Dispatch client. The
// issues walk as the package's whole-daemon rigs walk them (ownerStepsRig over stepRecordsRig):
// each role over the worker stream and the daemon's own routes, a person's review on the broker,
// and the architect's sign-off through its own route.
//
// What is stood in for lies outside the records: GitHub (forwardCountGitHub, which Dispatch's App,
// the daemons and the count program all read), the Apps' tokens, Envoy's listener, and the panes,
// which the package's fake runtime launches and the driver plays. The DECOY mergers' transcripts
// are written as files in the form the transcript store imports, and the count reads them with
// `--transcripts dir:`.
//
// FORWARD_COUNT_CONTROL names one of the four controls the script runs (unread is the script's
// own, since it changes only the readings `resolve` is given):
//
//	no-failure  the stand-in reports the check run unit-limits passing at ACME-2's head
//	blocked     DECOY-2's timeline holds a removal from the merge queue after its clear answer
//	captured    DECOY-4's transcript gains the result of a curl to the GraphQL route
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// The three projects of C7's table, each with a daemon of its own.
	forwardCountACME  = "ACME"
	forwardCountDECOY = "DECOY"
	forwardCountCLEAR = "CLEAR"
	// forwardCountBase is main's head when the run starts; every pull request stands at
	// stepRecordsHead, the head each rig's completions name.
	forwardCountBase = "ba5e110000000000000000000000000000000000"
	// forwardCountJob is the check run a CI job writes for ACME-2, and forwardCountCheck the
	// block of its Verification document that CI proves.
	forwardCountJob   = "unit-limits"
	forwardCountCheck = "c-unit-limits"
	// forwardCountControl is the environment variable the script names a control in.
	forwardCountControl = "FORWARD_COUNT_CONTROL"
)

// The reasons the roles give, word for word, as the daemon's message then carries them.
const (
	acmeOneReason = "the change's reader keeps the file open after it answers, so a second read of one path answers the first entry. " +
		"I fix that in this round rather than merge it."
	acmeTwoReason   = "the required check " + forwardCountJob + " failed at this head: its one case reported a failure, so the head must change."
	acmeThreeReason = "r2 of the spec is unmet at this head: the reader answers exit 1 for a file it cannot open, and r2 asks for exit 2."
	decoyOneReason  = "the change's conflict handling loses data: resolveTree drops the base's side where both sides changed one line, " +
		"so a merge of main silently takes this branch's text."
	decoyTwoReason = "Conflict with main's new defaults behavior: src/config.ts:loadConfig still swallows parse errors and returns DEFAULTS."
	decoyRest      = "the head conflicts with main since the base moved, and the change itself writes the cache key before it reads the " +
		"entry, so a second read of one key answers the first entry."
)

// forwardCountLive is one run of the control: the Dispatch and the GitHub stand-in every project
// shares, the three projects' rigs, the directory the DECOY transcripts are written to, and the
// evidence directory the run's files go in.
type forwardCountLive struct {
	t           *testing.T
	control     string
	root        string
	evidence    string
	natsURL     string
	standIn     *forwardCountGitHub
	dispatch    *scratchDispatch
	transcripts string
	projects    map[string]*ownerStepsRig
	// number is the next pull request number the run opens.
	number int
}

// TestForwardCountControlEventsLive drives C7's nine issues through three daemons and runs the
// count's `collect` over what they wrote. The script judges the summaries `resolve` then makes.
func TestForwardCountControlEventsLive(t *testing.T) {
	l := newForwardCountLive(t)
	l.step("acme-one-own-defect-one-failed-check-one-goal-check", l.acme)
	l.step("decoy-five-returns-whose-readers-confirm-a-block", l.decoy)
	l.step("clear-one-issue-whose-retro-and-merge-pass", l.clear)
	l.step("collect-reads-dispatch-github-and-the-transcripts", l.collect)
}

// newForwardCountLive starts the GitHub stand-in, the Dispatch every project shares, and one
// daemon for each project.
func newForwardCountLive(t *testing.T) *forwardCountLive {
	t.Helper()
	fixtureenv.Live(t)
	natsURL := workflowNATS(t)
	standIn := newForwardCountGitHub(t)
	// The driver runs from the directory the script ran in, not the package's, so the scratch
	// Dispatch is told the checkout it builds and serves from, as handoff-records.sh's driver
	// tells it (startHandoffDispatch).
	root := liveCLICheckout(t)
	dispatch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL, github: standIn.url, root: root})
	l := &forwardCountLive{
		t: t, control: os.Getenv(forwardCountControl), root: root, evidence: forwardCountEvidence(t),
		natsURL: natsURL, standIn: standIn, dispatch: dispatch, projects: map[string]*ownerStepsRig{}, number: 7100,
	}
	l.transcripts = filepath.Join(l.evidence, "transcripts")
	if err := os.MkdirAll(l.transcripts, 0o755); err != nil {
		t.Fatalf("make the transcript directory: %v", err)
	}
	t.Logf("the control is %q; Dispatch %s, GitHub %s, evidence %s", l.control, dispatch.base, standIn.url, l.evidence)
	return l
}

// forwardCountEvidence is the run's evidence directory: the one the proof's script named, which
// it hands the driver as the directory of the daemon's log, else a directory of the test's own.
func forwardCountEvidence(t *testing.T) string {
	t.Helper()
	if log := os.Getenv(liveDaemonLogEnv); log != "" {
		return filepath.Dir(log)
	}
	return t.TempDir()
}

// step runs one of the run's steps between its `== name` and `ok name` lines, as the repository's
// live proofs print them.
func (l *forwardCountLive) step(name string, body func()) {
	l.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

func (l *forwardCountLive) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// acme drives the three issues of ACME: a merger's return for its own defect, a merger's return
// for a failed required check, and a retro's return on a task that ran the implementer alone.
func (l *forwardCountLive) acme() {
	p := l.project(forwardCountACME, []forwardCountIssue{
		{title: "The widgets report their merge", spec: "The widgets report their merge once it reaches main.\n"},
		{title: "A second change limits its units", spec: "The unit limits hold at every head.\n", verification: forwardCountVerification},
		{title: "Third piece answers a missing file", spec: ":::requirement{#r2}\nThe reader answers exit 2 for a file it cannot open.\n:::\n"},
	})
	p.on("ACME-1")
	l.walk(p, "ACME-1", "implementer", "reviewer", "acceptance:owner")
	p.retro()
	l.returns(p, claim.RoleMerger, acmeOneReason)
	l.note("ACME-1: the merger returned the task for its own defect, with no block stated")

	p.on("ACME-2")
	l.walk(p, "ACME-2", "implementer", "reviewer", "acceptance:owner")
	p.retro()
	l.standIn.setCheckRun(stepRecordsHead, forwardCountJob, l.control == "no-failure")
	l.returns(p, claim.RoleMerger, acmeTwoReason)
	l.note("ACME-2: the merger returned the task for the required check %s, which the stand-in reports %s at %s",
		forwardCountJob, map[bool]string{true: "passing", false: "failing"}[l.control == "no-failure"], stepRecordsHead)

	p.on("ACME-3")
	l.walk(p, "ACME-3", "implementer")
	l.returns(p, claim.RoleImplementer, acmeThreeReason)
	l.note("ACME-3: the retro of a task that ran the implementer alone returned it for an unmet requirement")
}

// decoy drives the five issues of DECOY, each a merger's return whose readers confirm a block, and
// writes each merger's transcript as C7's table names it.
func (l *forwardCountLive) decoy() {
	p := l.project(forwardCountDECOY, []forwardCountIssue{
		{title: "One loses data on a merge", spec: "A merge keeps both sides.\n"},
		{title: "Two swallows parse errors", spec: "A parse error is answered, never swallowed.\n"},
		{title: "Three answers a cache key", spec: "A cache key is read before it is written.\n"},
		{title: "Four repeats the third case", spec: "The same work, with no answer about the head.\n"},
		{title: "Five relays the payload", spec: "The same work, with the payload in text alone.\n"},
	})
	reasons := []string{decoyOneReason, decoyTwoReason, decoyRest, decoyRest, decoyRest}
	for place, reason := range reasons {
		issue := fmt.Sprintf("DECOY-%d", place+1)
		p.on(issue)
		l.walk(p, issue, "implementer", "reviewer", "acceptance:owner")
		p.retro()
		merger := l.returns(p, claim.RoleMerger, reason)
		l.writeTranscript(issue, merger.session, p.number)
		l.note("%s: the merger returned the task, and its transcript stands in %s", issue, l.transcripts)
	}
	if l.control == "blocked" {
		l.standIn.addRemoval(l.pullOf(p, "DECOY-2"), stepRecordsHead, time.Now().Add(-5*time.Minute))
		l.note("the control blocked: DECOY-2's timeline holds a removal from the merge queue at %s, after its clear answer", stepRecordsHead)
	}
}

// clear drives the one issue of CLEAR to its merge: its retro and its merger pass, and no task
// returns.
func (l *forwardCountLive) clear() {
	p := l.project(forwardCountCLEAR, []forwardCountIssue{
		{title: "A clean change walks to its merge", spec: "The change walks to its merge with no return.\n"},
	})
	p.on("CLEAR-1")
	l.walk(p, "CLEAR-1", "implementer", "reviewer", "acceptance:owner")
	p.retro()
	l.mergeOnGitHub(p)
	l.note("CLEAR-1: its retro passed and GitHub merged its pull request, so its task's last step passed")
}

// collect runs the count's first phase against this run's Dispatch, its GitHub and the DECOY
// transcripts, and holds that it found the eight returns of C7's table.
func (l *forwardCountLive) collect() {
	t := l.t
	out := filepath.Join(l.evidence, "count")
	tokens := filepath.Join(l.evidence, "tokens")
	if err := os.MkdirAll(tokens, 0o700); err != nil {
		t.Fatalf("make the token directory: %v", err)
	}
	dispatchToken := filepath.Join(tokens, "dispatch")
	githubToken := filepath.Join(tokens, "github")
	for file, value := range map[string]string{dispatchToken: l.dispatch.token, githubToken: "forward-count-reader"} {
		if err := os.WriteFile(file, []byte(value+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	from := time.Now().AddDate(0, 0, -1).UTC().Format("2006-01-02")
	to := time.Now().AddDate(0, 0, 1).UTC().Format("2006-01-02")
	program := filepath.Join(l.root, "packages", "pi-legion", "scripts", "forward-count", "forward-count.ts")
	command := exec.Command("bun", program, "collect",
		"--dispatch-url", l.dispatch.base, "--token-file", dispatchToken,
		"--github-api-url", l.standIn.url, "--github-token-file", githubToken,
		"--projects", strings.Join([]string{forwardCountACME, forwardCountDECOY, forwardCountCLEAR}, ","),
		"--from", from, "--to", to, "--transcripts", "dir:"+l.transcripts, "--reasons", "all", "--out", out)
	command.Dir = l.root
	said, err := command.CombinedOutput()
	t.Logf("collect over %s to %s:\n%s", from, to, strings.TrimSpace(string(said)))
	if err != nil {
		t.Fatalf("the count's collect failed: %v", err)
	}
	lines := forwardCountLines(t, filepath.Join(out, "returns.jsonl"))
	want := []string{
		"ACME-1/a/merge/1", "ACME-2/a/merge/1", "ACME-3/a/retro/1",
		"DECOY-1/a/merge/1", "DECOY-2/a/merge/1", "DECOY-3/a/merge/1", "DECOY-4/a/merge/1", "DECOY-5/a/merge/1",
	}
	found := map[string]bool{}
	for _, line := range lines {
		found[line.ID] = true
		t.Logf("return %s: head %s, causes %s, answers %d, reason %q", line.ID, line.Head, forwardCountCauses(line), len(line.Answers), line.Reason)
	}
	for _, id := range want {
		if !found[id] {
			t.Errorf("returns.jsonl holds no return %s; it holds %d returns", id, len(lines))
		}
	}
	if len(lines) != len(want) {
		t.Errorf("returns.jsonl holds %d returns, want the %d of C7's table", len(lines), len(want))
	}
}

// forwardCountReturn is one line of returns.jsonl, as this test reads it back.
type forwardCountReturn struct {
	ID      string `json:"id"`
	Issue   string `json:"issue"`
	Project string `json:"project"`
	Head    string `json:"head"`
	Reason  string `json:"reason"`
	Causes  []struct {
		Cause string `json:"cause"`
		State string `json:"state"`
		Step  *int   `json:"step"`
	} `json:"causes"`
	Answers []struct {
		Source string `json:"source"`
		Place  string `json:"place"`
		Value  string `json:"value"`
		Head   string `json:"head"`
	} `json:"answers"`
}

func forwardCountLines(t *testing.T, path string) []forwardCountReturn {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []forwardCountReturn
	for _, line := range strings.Split(strings.TrimSpace(string(text)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var read forwardCountReturn
		if err := json.Unmarshal([]byte(line), &read); err != nil {
			t.Fatalf("decode a line of %s: %v", path, err)
		}
		lines = append(lines, read)
	}
	return lines
}

// forwardCountCauses is a return's stated causes with their states, as the log shows them.
func forwardCountCauses(line forwardCountReturn) string {
	said := make([]string, 0, len(line.Causes))
	for _, cause := range line.Causes {
		step := ""
		if cause.Step != nil {
			step = fmt.Sprintf(" (step %d)", *cause.Step)
		}
		said = append(said, cause.Cause+" "+cause.State+step)
	}
	if len(said) == 0 {
		return "none"
	}
	return strings.Join(said, ", ")
}

// forwardCountIssue is one issue of a project: its title, its spec and, where it has one, its
// Verification document.
type forwardCountIssue struct {
	title        string
	spec         string
	verification string
}

// forwardCountVerification is ACME-2's Verification document: one check a CI job proves.
const forwardCountVerification = ":::check{#" + forwardCountCheck +
	` negative_control="Break the unit limits and watch the run fail." ci_check="` + forwardCountJob + "\"}\n" +
	"The unit limits hold at the head.\n:::\n"

// project makes one project with its issues, opens a pull request of its own for each on the
// stand-in, publishes GitHub's opened of it, and boots a daemon of that project at the shared
// Dispatch. It answers the walk's rig.
func (l *forwardCountLive) project(key string, issues []forwardCountIssue) *ownerStepsRig {
	t := l.t
	t.Helper()
	proof := forwardCountProof(t, l.natsURL)
	proof.cfg.Project = key
	proof.cfg.Projects = map[string]config.Project{key: {}}
	proof.cfg.AdmissionCap = 8
	var err error
	if proof.project, err = claim.ProjectToken(key); err != nil {
		t.Fatal(err)
	}
	r := &ownerStepsRig{
		stepRecordsRig: &stepRecordsRig{
			liveProof: proof, dispatch: l.dispatch, github: &stepRecordsGitHub{t: t, url: l.standIn.url},
			claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16),
		},
		specs: map[string]string{},
	}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(l.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = l.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": "Forward count " + key}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+key+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	// Dispatch reads GitHub's events only while its delivery settings name a deploy repository,
	// and it saves them only once its App reads that repository. Without them it learns of no
	// pull request, and the count could read no head.
	r.person(http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo": liveRepo, "deploy_workflow_path": ".github/workflows/deploy.yaml", "production_job_names": []string{"deploy"},
		"pr_checks_workflow_path": ".github/workflows/ci.yaml", "population_authors": []string{stepRecordsPerson},
	}, http.StatusOK, nil)
	for _, issue := range issues {
		var created struct {
			createdIssue
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		r.person(http.MethodPost, "/api/v1/issues",
			map[string]any{"project": key, "title": issue.title, "spec": issue.spec, "force": true}, http.StatusCreated, &created)
		if created.PrimaryArtifactID == "" {
			t.Fatalf("Dispatch created %s with no primary document", created.Key)
		}
		if issue.verification != "" {
			r.person(http.MethodPost, "/api/v1/issues/"+created.Key+"/artifacts",
				map[string]string{"name": "verification.md", "content": issue.verification}, http.StatusCreated, nil)
		}
		made := created.createdIssue
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key,
			map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &made)
		l.number++
		r.specs[created.Key] = created.PrimaryArtifactID
		r.issues = append(r.issues, walkIssue{key: created.Key, title: issue.title, number: l.number})
		r.on(created.Key)
		l.standIn.open(l.number, claim.IssueBranch(created.Key), stepRecordsHead)
		r.seedAdmitted(made.Rank, made.LastSeq)
	}
	r.boot()
	for _, issue := range r.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
		l.publishOpened(r, issue.number)
	}
	r.on(r.issues[0].key)
	l.projects[key] = r
	t.Logf("%s: %d issues on the shared Dispatch, a daemon of its own, pull requests %d to %d",
		key, len(issues), r.issues[0].number, r.issues[len(r.issues)-1].number)
	return r
}

// publishOpened publishes GitHub's opened of a pull request, which Dispatch's intake reads and
// attributes to the issue of its branch.
func (l *forwardCountLive) publishOpened(r *ownerStepsRig, number int) {
	r.t.Helper()
	pull := l.standIn.pull(number)
	fields := map[string]string{
		"action": "opened", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(number), "title": "The change",
		"url": pull.url(), "body": "", "author": stepRecordsPerson, "head_ref": pull.branch, "head_sha": pull.head,
		"head_repo": liveRepo, "base_ref": "main", "merged": "false",
		"updated_at": pull.updatedAt.UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		r.t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), number)
	r.publish(subject, "github", fmt.Sprintf("pr-%d-opened", number), string(payload))
}

// pullOf is the number of issue's pull request.
func (l *forwardCountLive) pullOf(r *ownerStepsRig, issue string) int {
	l.t.Helper()
	for _, walk := range r.issues {
		if walk.key == issue {
			return walk.number
		}
	}
	l.t.Fatalf("%s is no issue of the run", issue)
	return 0
}

// walk gives issue a plan of steps, opens its gate and runs each step to its retro.
func (l *forwardCountLive) walk(r *ownerStepsRig, issue string, steps ...string) {
	t := l.t
	t.Helper()
	r.on(issue)
	r.givePlan(steps...)
	r.openGate(stepPhase(t, steps[0]))
	for place, step := range steps {
		next := phase.Retro
		if place+1 < len(steps) {
			next = stepPhase(t, steps[place+1])
		}
		r.runStep(step, answerPass, next)
	}
}

// returns is one role's own `request_backward_move` through the daemon's route, from the turn it
// is in and under a grant of its own, which ends its run as failed and posts the reason on the
// issue. It answers the claimant that asked.
func (l *forwardCountLive) returns(r *ownerStepsRig, role claim.Role, reason string) *stepClaimant {
	t := l.t
	t.Helper()
	c := r.worker(role)
	c.take()
	status, body := r.d.request(http.MethodPost, "/legion/v1/phase/backward",
		api.PhaseBackwardRequest{GrantID: c.grant(), To: phase.Implementing, Reason: reason}, false)
	if status != http.StatusOK {
		t.Fatalf("%s's request_backward_move of %s = %d %s, want 200", role, r.key, status, body)
	}
	c.endTurn()
	r.waitPhase(phase.Implementing)
	return c
}

// mergeOnGitHub merges CLEAR-1's pull request on the stand-in and publishes GitHub's merge, then
// waits for Dispatch to hold the merge's own step record passed, which is what the count reads.
func (l *forwardCountLive) mergeOnGitHub(r *ownerStepsRig) {
	t := l.t
	t.Helper()
	r.awaitLaunch(r.token(claim.RoleMerger))
	l.standIn.merge(r.number)
	r.waitProcessed(r.publishMergeOf(r.number, r.key, stepRecordsHead, "main"))
	issue := r.key
	var last string
	if !testwait.Holds(stepRecordsWait, time.Second, func() bool {
		read, err := r.readStepsOf(issue)
		if err != nil {
			last = err.Error()
			return false
		}
		for _, run := range read.records() {
			if run.Kind == "merge" && run.Result == "passed" {
				return true
			}
			last = fmt.Sprintf("%s %s %s", run.Kind, run.Role, run.Result)
		}
		return false
	}) {
		t.Fatalf("Dispatch holds no passed merge record of %s within %s; the last record read was %s", issue, stepRecordsWait, last)
	}
}

// writeTranscript writes one DECOY merger's session as C7's table names it, in the form the
// transcript store imports, into the directory the count reads with `--transcripts dir:`.
func (l *forwardCountLive) writeTranscript(issue, session string, number int) {
	t := l.t
	t.Helper()
	at := time.Now().Add(-10 * time.Minute)
	clear := fmt.Sprintf(`{"headRefOid":%q,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}`, stepRecordsHead)
	conflicting := fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"headRefOid":%q,"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}}}}`,
		stepRecordsHead)
	curl := fmt.Sprintf("curl -fsS -H @auth-header %s/graphql -d @query.json", l.standIn.url)
	view := fmt.Sprintf("gh pr view %d --json headRefOid,mergeable,mergeStateStatus", number)
	var lines []string
	add := func(line map[string]any) {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	call := func(id, tool string, args map[string]any, when time.Time) {
		add(map[string]any{"type": "message", "timestamp": when.UTC().Format(time.RFC3339),
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": id, "name": tool, "arguments": args}}}})
	}
	answered := func(id, tool, text string, when time.Time) {
		add(map[string]any{"type": "message", "timestamp": when.UTC().Format(time.RFC3339),
			"message": map[string]any{"role": "toolResult", "toolName": tool, "toolCallId": id, "isError": false,
				"content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	add(map[string]any{"type": "session", "id": session})
	switch issue {
	case "DECOY-1", "DECOY-2":
		call("c1", "bash", map[string]any{"command": view}, at)
		answered("c1", "bash", clear, at.Add(time.Second))
	case "DECOY-3":
		call("c1", "bash", map[string]any{"command": curl}, at)
		answered("c1", "bash", conflicting, at.Add(time.Second))
	case "DECOY-4":
		call("c1", "bash", map[string]any{"command": "dispatch steps --issue " + issue}, at)
		answered("c1", "bash", issue+": 6 steps in 1 tasks", at.Add(time.Second))
		if l.control == "captured" {
			call("c2", "bash", map[string]any{"command": curl}, at.Add(2*time.Second))
			answered("c2", "bash", conflicting, at.Add(3*time.Second))
			l.note("the control captured: DECOY-4's transcript holds the result of a curl to the GraphQL route")
		}
	case "DECOY-5":
		add(map[string]any{"type": "message", "timestamp": at.UTC().Format(time.RFC3339),
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text",
				"text": "GitHub answers " + conflicting}}}})
		call("c1", "envoy_publish", map[string]any{"topic": "notifications.role.architect", "message": conflicting}, at.Add(time.Second))
		answered("c1", "envoy_publish", "published", at.Add(2*time.Second))
		call("c2", "bash", map[string]any{"command": "echo '" + conflicting + "'"}, at.Add(3*time.Second))
		answered("c2", "bash", conflicting, at.Add(4*time.Second))
		// The payload goes in a block scalar, so it stands in the delivery's text as GitHub wrote
		// it. Escaping its quotes would put a payload no reader of a payload can parse, and then a
		// classifier that wrongly counted a delivery would read no answer either: the control of
		// that fault could not fail.
		add(map[string]any{"type": "custom_message", "customType": "envoy-message", "timestamp": at.Add(5 * time.Minute).UTC().Format(time.RFC3339),
			"content": []any{map[string]any{"type": "text", "text": "envoy:\n  to: you (" + session + ")\n  from: the architect\n  message: |\n    " +
				"GitHub answers " + conflicting}}})
	}
	path := filepath.Join(l.transcripts, strings.ToLower(issue)+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// The GitHub stand-in
// ---------------------------------------------------------------------------

// forwardCountGitHub is GitHub as this run's Dispatch, its daemons and the count program read it:
// the App and its token, the repository, each issue's pull request with its commits, reviews and
// timeline, the commits of main, the check runs of a head, and a GraphQL root that answers the
// timeline query the count makes and nothing else. Every other request is answered 404 and logged.
type forwardCountGitHub struct {
	t   *testing.T
	url string
	mu  sync.Mutex
	// parents is each commit GitHub holds, by its parents, and refs each branch's head.
	parents map[string][]string
	refs    map[string]string
	pulls   map[int]*forwardCountPull
	// runs is the check runs of a head, by its commit.
	runs     map[string][]map[string]any
	served   int
	unserved []string
}

// forwardCountPull is one pull request of the run.
type forwardCountPull struct {
	number       int
	branch, head string
	merged       bool
	merge        string
	openedAt     time.Time
	updatedAt    time.Time
	mergedAt     time.Time
	// removals is each removal from the merge queue of this pull request's timeline.
	removals []forwardCountRemoval
}

// forwardCountRemoval is one removal from the merge queue, as GitHub's timeline answers it.
type forwardCountRemoval struct {
	at   time.Time
	head string
}

func (p forwardCountPull) url() string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, p.number)
}

// answer is the pull request as GitHub's REST API answers it.
func (p forwardCountPull) answer() map[string]any {
	answer := map[string]any{
		"number": p.number, "title": "The change", "body": "", "html_url": p.url(),
		"user": map[string]any{"login": stepRecordsPerson}, "labels": []any{},
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

func newForwardCountGitHub(t *testing.T) *forwardCountGitHub {
	t.Helper()
	g := &forwardCountGitHub{
		t: t, parents: map[string][]string{forwardCountBase: nil}, refs: map[string]string{"main": forwardCountBase},
		pulls: map[int]*forwardCountPull{}, runs: map[string][]map[string]any{},
	}
	repo := "/repos/" + liveRepo
	routes := http.NewServeMux()
	routes.HandleFunc("GET /app/installations", g.installations)
	routes.HandleFunc("GET "+repo+"/installation", g.installation)
	routes.HandleFunc("POST /app/installations/{id}/access_tokens", g.accessToken)
	routes.HandleFunc("GET /installation/repositories", g.repositories)
	routes.HandleFunc("GET "+repo, g.repository)
	routes.HandleFunc("GET "+repo+"/pulls", g.list)
	routes.HandleFunc("GET "+repo+"/pulls/{number}", g.one)
	routes.HandleFunc("GET "+repo+"/pulls/{number}/commits", g.commitsOf)
	routes.HandleFunc("GET "+repo+"/pulls/{number}/reviews", g.reviews)
	routes.HandleFunc("GET "+repo+"/commits/{ref}", g.commit)
	routes.HandleFunc("GET "+repo+"/commits/{sha}/check-runs", g.checkRuns)
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
	}})
	return g
}

// open is GitHub's pull request number from branch at head, a commit after main's head.
func (g *forwardCountGitHub) open(number int, branch, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().Add(-time.Hour)
	g.parents[head] = []string{g.refs["main"]}
	g.refs[branch] = head
	g.pulls[number] = &forwardCountPull{number: number, branch: branch, head: head, openedAt: now, updatedAt: now}
}

// merge is GitHub's merge of pull request number onto main.
func (g *forwardCountGitHub) merge(number int) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, held := g.pulls[number]
	if !held {
		g.t.Fatalf("GitHub merged pull request #%d, which the run never opened", number)
	}
	commit := fmt.Sprintf("%040x", number)
	now := time.Now()
	g.parents[commit] = []string{g.refs["main"], pull.head}
	g.refs["main"] = commit
	pull.merged, pull.merge, pull.mergedAt, pull.updatedAt = true, commit, now, now
	return commit
}

// pull is the pull request number as GitHub holds it now.
func (g *forwardCountGitHub) pull(number int) forwardCountPull {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, held := g.pulls[number]
	if !held {
		g.t.Fatalf("the run opened no pull request #%d", number)
	}
	return *pull
}

// setCheckRun gives the head one check run of name, passed or failed.
func (g *forwardCountGitHub) setCheckRun(head, name string, passed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	conclusion := "failure"
	if passed {
		conclusion = "success"
	}
	g.runs[head] = []map[string]any{{
		"id": 9001, "name": name, "status": "completed", "conclusion": conclusion,
		"completed_at": time.Now().UTC().Format(time.RFC3339),
		"html_url":     "https://github.com/" + liveRepo + "/runs/9001",
		"output":       map[string]any{"title": nil, "summary": nil},
		"app":          map[string]any{"slug": "github-actions"},
	}}
}

// addRemoval puts a removal from the merge queue on a pull request's timeline.
func (g *forwardCountGitHub) addRemoval(number int, head string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, held := g.pulls[number]
	if !held {
		g.t.Fatalf("the run opened no pull request #%d", number)
	}
	pull.removals = append(pull.removals, forwardCountRemoval{at: at, head: head})
}

func (g *forwardCountGitHub) write(w http.ResponseWriter, status int, body any) {
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

func (g *forwardCountGitHub) noRoute(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.unserved = append(g.unserved, standInCaller(r)+" "+r.Method+" "+r.URL.RequestURI())
	g.mu.Unlock()
	g.t.Logf("github stand-in: no route for %s %s from %s: 404", r.Method, r.URL.RequestURI(), standInCaller(r))
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

func forwardCountInstallation() map[string]any {
	return map[string]any{"id": 77, "app_slug": "dispatch-scratch", "account": map[string]any{"login": "acme"}, "permissions": doneByChecksPermissions()}
}

func (g *forwardCountGitHub) installations(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, []any{forwardCountInstallation()})
}

func (g *forwardCountGitHub) installation(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, forwardCountInstallation())
}

func (g *forwardCountGitHub) accessToken(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusCreated, map[string]any{
		"token": doneByChecksAppToken, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "permissions": doneByChecksPermissions(),
	})
}

func (g *forwardCountGitHub) repositories(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, map[string]any{"total_count": 1, "repositories": []any{map[string]any{"full_name": liveRepo}}})
}

func (g *forwardCountGitHub) repository(w http.ResponseWriter, _ *http.Request) {
	g.write(w, http.StatusOK, map[string]any{"full_name": liveRepo, "default_branch": "main", "private": false})
}

func (g *forwardCountGitHub) list(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	answers := make([]any, 0, len(g.pulls))
	for _, pull := range g.pulls {
		answers = append(answers, pull.answer())
	}
	g.mu.Unlock()
	g.write(w, http.StatusOK, answers)
}

// find is the pull request r names, and whether GitHub holds it.
func (g *forwardCountGitHub) find(r *http.Request) (forwardCountPull, bool) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return forwardCountPull{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, held := g.pulls[number]
	if !held {
		return forwardCountPull{}, false
	}
	return *pull, true
}

func (g *forwardCountGitHub) one(w http.ResponseWriter, r *http.Request) {
	pull, held := g.find(r)
	if !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, pull.answer())
}

// commitsOf is the pull request's one commit, its head, written when it opened.
func (g *forwardCountGitHub) commitsOf(w http.ResponseWriter, r *http.Request) {
	pull, held := g.find(r)
	if !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	at := pull.openedAt.UTC().Format(time.RFC3339)
	g.write(w, http.StatusOK, []any{map[string]any{"sha": pull.head, "commit": map[string]any{
		"message": "The change", "author": map[string]any{"date": at}, "committer": map[string]any{"date": at},
	}}})
}

func (g *forwardCountGitHub) reviews(w http.ResponseWriter, r *http.Request) {
	if _, held := g.find(r); !held {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, []any{})
}

// checkRuns is GET /repos/{repo}/commits/{sha}/check-runs, which Dispatch reads for a check CI
// proves.
func (g *forwardCountGitHub) checkRuns(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	runs := g.runs[r.PathValue("sha")]
	g.mu.Unlock()
	g.write(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
}

// resolve is the commit ref names, and whether GitHub holds it. Its caller holds g.mu.
func (g *forwardCountGitHub) resolve(ref string) (string, bool) {
	if head, branch := g.refs[ref]; branch {
		return head, true
	}
	_, held := g.parents[ref]
	return ref, held
}

// holds says whether commit's history holds ancestor. Its caller holds g.mu.
func (g *forwardCountGitHub) holds(commit, ancestor string) bool {
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

func (g *forwardCountGitHub) commit(w http.ResponseWriter, r *http.Request) {
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

func (g *forwardCountGitHub) commitPulls(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	g.mu.Lock()
	answers := []any{}
	for _, pull := range g.pulls {
		if sha == pull.head || (pull.merged && sha == pull.merge) {
			answers = append(answers, pull.answer())
		}
	}
	g.mu.Unlock()
	g.write(w, http.StatusOK, answers)
}

func (g *forwardCountGitHub) compare(w http.ResponseWriter, r *http.Request) {
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

func (g *forwardCountGitHub) ref(w http.ResponseWriter, r *http.Request) {
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

func (g *forwardCountGitHub) createRef(w http.ResponseWriter, r *http.Request) {
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

func (g *forwardCountGitHub) updateRef(w http.ResponseWriter, r *http.Request) {
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

func (g *forwardCountGitHub) permission(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("user") != stepRecordsPerson {
		g.write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.write(w, http.StatusOK, map[string]any{"permission": "write", "user": map[string]string{"login": stepRecordsPerson}})
}

// graphql answers the count's timeline query for the pull request it names, and the daemon's own
// query with no pull request, as the package's other stand-ins do.
func (g *forwardCountGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		g.write(w, http.StatusBadRequest, map[string]string{"message": "cannot read the query"})
		return
	}
	var asked struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &asked); err != nil || !strings.Contains(asked.Query, "timelineItems") {
		g.write(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}}})
		return
	}
	number := 0
	switch asked := asked.Variables["number"].(type) {
	case float64:
		number = int(asked)
	case string:
		number, _ = strconv.Atoi(asked)
	}
	g.mu.Lock()
	pull, held := g.pulls[number]
	nodes := []any{}
	if held {
		for _, removal := range pull.removals {
			nodes = append(nodes, map[string]any{
				"__typename": "RemovedFromMergeQueueEvent", "createdAt": removal.at.UTC().Format(time.RFC3339),
				"beforeCommit": map[string]any{"oid": removal.head},
			})
		}
	}
	g.mu.Unlock()
	if !held {
		g.write(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}}})
		return
	}
	g.write(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{
		"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": nodes}},
	}}})
}
