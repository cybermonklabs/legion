package signals

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// The rig's project, its deploy repository (its default repository, which its one source reads),
// the rule's production jobs and apply step.
const (
	alpha     = "ALPHA"
	widgets   = "acme/widgets"
	prodJob   = "deploy / production"
	canaryJob = "deploy / canary"
	applyStep = "Run ./.github/actions/apply"
)

var (
	success = new(delivery.DeliveryJobConclusion(delivery.DeliveryJobConclusionSuccess))
	failure = new(delivery.DeliveryJobConclusion(delivery.DeliveryJobConclusionFailure))
	tester  = model.Actor{Kind: "session", ID: "tester-session"}
	person  = model.Actor{Kind: "user", ID: "alice"}
)

// base is three days before the test runs, so every run the rig stores started before the pass's
// clock.
var base = time.Now().UTC().Truncate(24 * time.Hour).Add(-72 * time.Hour)

// at is minutes after base.
func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

// sha is a 40-hex commit made of c.
func sha(c string) string { return strings.Repeat(c, 40)[:40] }

type rig struct {
	t    *testing.T
	ctx  context.Context
	pool *store.Pool
}

// newRig is a database with project ALPHA, whose default repository acme/widgets is its one deploy
// source, under the rule rule(prodJob).
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, ctx: t.Context(), pool: storetest.Open(t).Pool}
	r.exec("make the project", `insert into projects (key, name, default_assignee, default_repo) values ($1, $1, 'alice', $2)`, alpha, widgets)
	r.saveRule(prodJob)
	return r
}

func (r *rig) exec(what, statement string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, statement, args...); err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

// rule is acme/widgets's rule with production jobs jobs and the apply step.
func rule(jobs ...string) delivery.DeliverySettings {
	return delivery.DeliverySettings{
		Project: alpha, DeployRepo: widgets, DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: jobs, ProductionApplyStepName: new(applyStep),
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"octocat"},
	}
}

// saveRule saves ALPHA's delivery settings with acme/widgets's rule naming jobs.
func (r *rig) saveRule(jobs ...string) {
	r.t.Helper()
	if _, err := deliverytest.SaveRules(r.ctx, r.pool, alpha, rule(jobs...)); err != nil {
		r.t.Fatalf("save the delivery settings: %v", err)
	}
}

// deployRun stores deploy run id of acme/widgets on main, created and started at created, with head
// head and one job, job, which runs from created and, where conclusion is not nil, ends ten
// minutes later with conclusion and its apply step's conclusion step. It writes the run as the
// intake and the reconcile do (delivery.UpsertRun) and its job as every job writer does
// (delivery.UpsertRunJobs).
func (r *rig) deployRun(id int64, created time.Time, head, job string, conclusion, step *delivery.DeliveryJobConclusion) {
	r.t.Helper()
	run := delivery.DeliveryRun{
		Repo: widgets, RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: head, HeadCommitAt: created.Add(-time.Minute),
		StartedAt: created, CreatedAt: new(created), URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", widgets, id),
		HeadBranch: new("main"), Event: new("push"),
	}
	jobRow := delivery.DeliveryRunJob{Repo: widgets, RunID: id, Name: job, StartedAt: new(created), ApplyStep: new(applyStep)}
	if conclusion != nil {
		done := created.Add(10 * time.Minute)
		run.CompletedAt, run.Conclusion = &done, new(delivery.DeliveryRunConclusion(delivery.DeliveryRunConclusionSuccess))
		jobRow.CompletedAt, jobRow.Conclusion, jobRow.ApplyStepConclusion = &done, conclusion, step
	}
	if err := delivery.UpsertRun(r.ctx, r.pool, run); err != nil {
		r.t.Fatalf("store run %d: %v", id, err)
	}
	if err := delivery.UpsertRunJobs(r.ctx, r.pool, widgets, id, []delivery.DeliveryRunJob{jobRow}, conclusion != nil); err != nil {
		r.t.Fatalf("store the jobs of run %d: %v", id, err)
	}
}

// pullRequest stores population pull request acme/widgets#number, merged at merged with merge
// commit commit, attributed to issue where issue is not empty.
func (r *rig) pullRequest(number int, merged time.Time, commit, issue string) {
	r.t.Helper()
	pr := delivery.DeliveryPullRequest{
		Repo: widgets, Number: number, Title: fmt.Sprintf("Change %d", number),
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", widgets, number), Author: "octocat",
		CreatedAt: new(merged.Add(-time.Hour)), MergedAt: new(merged), MergeCommitSHA: new(commit),
	}
	if issue != "" {
		pr.IssueKey = new(issue)
	}
	if err := delivery.UpsertPullRequest(r.ctx, r.pool, pr); err != nil {
		r.t.Fatalf("store pull request #%d: %v", number, err)
	}
}

// passDeploys runs one pass of the deploys feed.
func (r *rig) passDeploys() {
	r.t.Helper()
	if err := passDeploys(r.ctx, r.pool); err != nil {
		r.t.Fatalf("the deploys feed's pass: %v", err)
	}
}

// safe is the newest event's id: every event the rig wrote has committed.
func (r *rig) safe() int64 {
	r.t.Helper()
	var safe int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce(max(id), 0) from events`).Scan(&safe); err != nil {
		r.t.Fatalf("read the newest event: %v", err)
	}
	return safe
}

// passEvents runs one pass of each event feed up to the newest event.
func (r *rig) passEvents() {
	r.t.Helper()
	safe := r.safe()
	if err := passChecks(r.ctx, r.pool, safe); err != nil {
		r.t.Fatalf("the checks feed's pass: %v", err)
	}
	if err := passIncidents(r.ctx, r.pool, safe); err != nil {
		r.t.Fatalf("the incidents feed's pass: %v", err)
	}
}

// deploys is every GitHub deploy row of acme/widgets, oldest finish first.
func (r *rig) deploys() []prodevents.Event {
	r.t.Helper()
	rows, err := prodevents.GitHubDeploys(r.ctx, r.pool, widgets, nil)
	if err != nil {
		r.t.Fatalf("read the deploy rows: %v", err)
	}
	return rows
}

// deployOf is the GitHub deploy row of run id, failing the test where none is stored.
func (r *rig) deployOf(id int64) prodevents.Event {
	r.t.Helper()
	for _, row := range r.deploys() {
		if row.SourceRef == prodevents.GitHubSourceRef(id) {
			return row
		}
	}
	r.t.Fatalf("no deploy row of run %d among %+v", id, r.deploys())
	return prodevents.Event{}
}

// failures is every failure row of the record, oldest first.
func (r *rig) failures() []prodevents.Event {
	r.t.Helper()
	page, _, err := prodevents.List(r.ctx, r.pool, prodevents.ListQuery{
		Project: alpha, From: base.Add(-365 * 24 * time.Hour), To: time.Now().Add(24 * time.Hour),
		Kind: prodevents.KindFailure, Limit: 100,
	})
	if err != nil {
		r.t.Fatalf("read the failures: %v", err)
	}
	return page
}

// issue makes issue ALPHA-number labelled labels, and answers its key.
func (r *rig) issue(number int, labels ...string) string {
	r.t.Helper()
	key := fmt.Sprintf("%s-%d", alpha, number)
	r.exec("make issue "+key, `insert into issues (key, project_key, number, title, status, created_by, rank, labels)
		values ($1, $2, $3, $1, 'todo', '{"kind":"user","id":"alice"}', $1, $4)`, key, alpha, number, append([]string{}, labels...))
	return key
}

// event appends an event of type kind on issue with payload, as actor, and answers its seq.
func (r *rig) event(issue, kind string, actor model.Actor, payload any) int64 {
	r.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		r.t.Fatalf("encode the payload of %s: %v", kind, err)
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		r.t.Fatalf("encode the actor of %s: %v", kind, err)
	}
	var seq int64
	if err := r.pool.QueryRow(r.ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1), $2, $3, $4, false)
		returning seq`, issue, kind, actorJSON, body).Scan(&seq); err != nil {
		r.t.Fatalf("append %s on %s: %v", kind, issue, err)
	}
	return seq
}

// checkResult records a result of check block of issue's Verification document at commit, as the
// record route writes it: its result.recorded event on the issue, then its row naming that event.
// It answers the result's id.
func (r *rig) checkResult(issue, block, commit, verdict, when string) string {
	r.t.Helper()
	var artifact string
	if err := r.pool.QueryRow(r.ctx, `
		with held as (select id from artifacts where issue_key = $1 and slug = 'verification-md'),
		made as (
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			select $1, $2, 'verification-md', 'verification.md', 'doc', '{"kind":"user","id":"alice"}'
			where not exists (select from held)
			returning id
		)
		select id::text from held union all select id::text from made`, issue, alpha).Scan(&artifact); err != nil {
		r.t.Fatalf("make the Verification document of %s: %v", issue, err)
	}
	id := uuid.NewString()
	seq := r.event(issue, "result.recorded", tester, model.CheckResultEventPayload{
		ResultID: id, Issue: issue, CheckIssue: issue, CheckArtifactID: artifact, CheckBlockID: block, CheckVersion: 1,
		When: when, Verdict: verdict, Role: "tester", CommitSHA: commit,
	})
	actorJSON, _ := json.Marshal(tester)
	r.exec("record a result of "+block, `
		insert into check_results (id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, actor, event_seq)
		values ($1, $2, $2, $3, $4, 1, 'digest', 'tester', $5, $6, $7, $8)`,
		id, issue, artifact, block, commit, verdict, actorJSON, seq)
	return id
}

// verification is the id of issue's Verification document, which checkResult makes.
func (r *rig) verification(issue string) string {
	r.t.Helper()
	var artifact string
	if err := r.pool.QueryRow(r.ctx, `select id::text from artifacts where issue_key = $1 and slug = 'verification-md'`,
		issue).Scan(&artifact); err != nil {
		r.t.Fatalf("read the Verification document of %s: %v", issue, err)
	}
	return artifact
}
