// An issue that ran before the release of the owner's plan goes on with the steps it had.
// The store is the one the release before leaves: every embedded migration below the first one of
// the owner's plan (inFlightPlanMigration), applied by hand, and the record of seven issues written
// in that schema's columns with plain SQL, one in each phase of today's line. The branch's own
// migrations then run on it, as a deploy runs them, and the whole daemon of the branch boots on it
// (stepRecordsRig's boot) over the real Dispatch of this package's tests.
package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// inFlightPlanMigration ends the name of the migration that adds the plan rows, the first one of
// the release of the owner's plan. It is found by its name rather than its number, so the store of
// the release before is the same whatever number a later merge gives it.
const inFlightPlanMigration = "_issue_plans.up.sql"

// inFlightIssue is one issue the release before left mid-walk: where it stands, and the session the
// earlier release recorded for each of its claims.
type inFlightIssue struct {
	walkIssue
	stands   phase.Phase
	sessions map[claim.Role]inFlightSession
}

// inFlightSession is a session the earlier release recorded on a claim: its id and its file.
type inFlightSession struct {
	id, file string
}

// inFlightRig is the walk's rig on the store the release before leaves, with the seven issues and
// the control the seed wrote.
type inFlightRig struct {
	*stepRecordsRig
	walks   []*inFlightIssue
	control *inFlightIssue
}

// inFlightTitles is the title of each issue of the run. Dispatch refuses a title that is a near
// copy of an open one's (POSSIBLE_DUPLICATE), so no two share a word that counts.
var inFlightTitles = []string{"Planner drafts outline", "Implementer writes code", "Tester checks behaviour",
	"Reviewer reads diff", "Owner accepts result", "Retrospective records lessons", "Merger lands branch", "Control seeds nothing"}

// An issue that ran before the release goes on. Seven issues of the release before,
// one at each phase of today's line, each with its sessions and its phase rows and its claims at the
// plugin contract below the owner's plan, are migrated and walked to done on the branch's daemon.
// Each goes on from its phase: its first worker launch is a resume of the session the release
// before recorded for the role of that phase, and no planner starts for an issue past planning. Each
// claim keeps its session, the counts of the issue in reviewing go on on its implementer's phases
// row from where the release before left them, no phases row names a step, and no issue has plan
// rows at the end. The control is an eighth issue seeded as the one in testing with its phase
// deleted from the seed: it starts again from its first step, and the same check must say so.
func TestAnIssueThatRanBeforeTheReleaseGoesOn(t *testing.T) {
	t.Parallel()
	r := newInFlightRig(t)
	for _, issue := range r.walks {
		r.on(issue.key)
		r.goOn(issue)
	}
	ctx := context.Background()
	var keys []string
	for _, issue := range r.walks {
		keys = append(keys, issue.key)
		if err := r.wentOn(issue); err != nil {
			t.Fatal(err)
		}
		if got := string(r.d.state().Issues[issue.key].Phase); got != string(phase.Done) {
			t.Fatalf("%s, seeded in %s, stands in %s after its walk, want done", issue.key, issue.stands, got)
		}
	}
	reviewing := r.walks[3]
	r.on(reviewing.key)
	if implementer := r.phaseOf(claim.RoleImplementer); implementer.Pass != 2 || implementer.Rounds != 2 {
		t.Fatalf("%s's implementer row holds pass %d and rounds %d after one round that requested changes, want 2 and 2: the release before left 1 and 1 there, and an issue with no plan rows counts on that row",
			reviewing.key, implementer.Pass, implementer.Rounds)
	}
	var named int
	if err := r.st.Pool().QueryRow(ctx, `select count(*) from phases where issue = any($1) and (step_task <> '' or step_place <> 0)`,
		keys).Scan(&named); err != nil {
		t.Fatal(err)
	}
	if named != 0 {
		t.Fatalf("%d phases rows of the issues that ran before the release name a step, want none", named)
	}
	var plans, tasks, steps int
	if err := r.st.Pool().QueryRow(ctx, `select (select count(*) from issue_plans where issue = any($1)),
		(select count(*) from plan_tasks where issue = any($1)), (select count(*) from plan_steps where issue = any($1))`,
		keys).Scan(&plans, &tasks, &steps); err != nil {
		t.Fatal(err)
	}
	if plans+tasks+steps != 0 {
		t.Fatalf("the issues that ran before the release hold %d issue_plans, %d plan_tasks and %d plan_steps rows, want none", plans, tasks, steps)
	}

	// The control: the issue seeded as the one in testing, with its phase deleted from the seed,
	// starts again at its first step, and the check names it.
	r.waitPhaseOf(r.control.key, phase.Planning)
	r.awaitLaunch(r.tokenOf(r.control.key, claim.RolePlanner))
	err := r.wentOn(r.control)
	if err == nil {
		t.Fatalf("control: %s, whose phase the seed deleted, passed the check of an issue that went on from testing", r.control.key)
	}
	if want := "started again from its first step"; !strings.Contains(err.Error(), want) {
		t.Fatalf("control: the check of %s says %q, want it to say it %s", r.control.key, err, want)
	}
}

// goOn walks issue to done from where the release before left it, as each role completes today:
// its role's claim, resumed in the session the release before recorded, takes the phase's task,
// and each step after it runs as on any issue. The issue in reviewing has one round that requests
// changes before the round that approves.
func (r *inFlightRig) goOn(issue *inFlightIssue) {
	r.t.Helper()
	switch issue.stands {
	case phase.Planning:
		r.plan()
		fallthrough
	case phase.Implementing:
		r.implement()
		fallthrough
	case phase.Testing:
		r.test()
		fallthrough
	case phase.Reviewing:
		if issue.stands == phase.Reviewing {
			r.review("CHANGES_REQUESTED", phase.Implementing)
			r.implement()
			r.test()
		}
		r.review("APPROVED", phase.Accepting)
		fallthrough
	case phase.Accepting:
		r.accept()
		fallthrough
	case phase.Retro:
		r.retro()
		fallthrough
	case phase.Merging:
		r.merge()
	}
}

// wentOn is the check that one issue went on from the phase the release before
// left it in. Its first launch of a worker is a resume of the session the release before recorded
// for that phase's role (the architect's for accepting, whose first launch of a worker is the
// implementer's retro), no planner was launched for an issue past planning, each claim the release
// before recorded holds the same session, and the issue has no plan rows. The error names the first
// thing that does not hold.
func (r *inFlightRig) wentOn(issue *inFlightIssue) error {
	r.t.Helper()
	stood := issue.stands
	launches := r.launchesOf(issue.key)
	if stood != phase.Planning {
		for _, launch := range launches {
			if launch.Spec.Role == claim.RolePlanner {
				return fmt.Errorf("%s, left in %s by the release before, started again from its first step: the daemon launched its planner (%s)",
					issue.key, stood, launch.Method)
			}
		}
	}
	first := phase.RoleFor(stood)
	if stood == phase.Accepting {
		first = claim.RoleArchitect
	}
	var resumed *fake.Call
	for i := range launches {
		if launches[i].Spec.Role == first {
			resumed = &launches[i]
			break
		}
		if launches[i].Spec.Role != claim.RoleArchitect {
			return fmt.Errorf("%s, left in %s, first launched its %s, want its %s", issue.key, stood, launches[i].Spec.Role, first)
		}
	}
	session := issue.sessions[first]
	if resumed == nil || resumed.Method != "Resume" || resumed.Spec.ResumeSessionFile != session.file {
		return fmt.Errorf("%s, left in %s, launched its %s as %+v, want a resume of the session file %s the release before recorded",
			issue.key, stood, first, resumed, session.file)
	}
	for role, kept := range issue.sessions {
		if role == claim.RoleMerger {
			// The walk's merger never registers: GitHub's merge ends merging, and its launch above
			// carries the session.
			continue
		}
		if got := r.d.claim(r.tokenOf(issue.key, role)).Session; got != kept.id {
			return fmt.Errorf("%s's %s holds the session %q, want %q, the one the release before recorded", issue.key, role, got, kept.id)
		}
	}
	var stored *record.StoredPlan
	if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = r.records.Plan(context.Background(), tx, issue.key, 1)
		return err
	}); err != nil {
		return fmt.Errorf("read the plan of %s: %w", issue.key, err)
	}
	if stored != nil {
		return fmt.Errorf("%s, left in %s by the release before, holds plan rows %+v: it started again from its first step on a plan", issue.key, stood, stored.Version)
	}
	return nil
}

// launchesOf is every launch of a claim of issue the daemon has made, in order.
func (r *stepRecordsRig) launchesOf(issue string) []fake.Call {
	var launches []fake.Call
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Issue == issue {
			launches = append(launches, call)
		}
	}
	return launches
}

// newInFlightRig is the store the release before leaves, seeded with the seven issues and the
// control (seedInFlight), migrated to the branch's schema, with the daemon booted on it at the real
// Dispatch of this package's tests, whose events reach the daemon on the broker they share
// (newPlanRig), and each issue's recorded claims resumed in their sessions.
func newInFlightRig(t *testing.T) *inFlightRig {
	t.Helper()
	natsURL := workflowNATS(t)
	proof := newLiveProofOn(t, natsURL)
	st, dsn := storeBeforeThePlans(t)
	proof.st, proof.cfg.PostgresDSN = st, dsn
	proof.cfg.Project = "IF" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 16
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &inFlightRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL}),
		github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "In flight"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	stands := append(slices.Clone(phase.Order), phase.Admitted)
	for place, stood := range stands {
		status := workflow.PhaseStatus(stood)
		if stood == phase.Admitted {
			status = "in_progress"
		}
		var created createdIssue
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": inFlightTitles[place]}, http.StatusCreated, &created)
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
		issue := &inFlightIssue{walkIssue: walkIssue{key: created.Key, title: inFlightTitles[place], number: int(number) + 1 + place},
			stands: stood, sessions: map[claim.Role]inFlightSession{}}
		r.issues = append(r.issues, issue.walkIssue)
		r.github.open("legion/"+issue.key, issue.number)
		seeded := stood
		if stood == phase.Admitted {
			// The control is the issue in testing with its phase deleted from the seed.
			seeded = phase.Testing
			r.control = issue
		} else {
			r.walks = append(r.walks, issue)
		}
		r.seedInFlight(issue, seeded, status, created)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store the release before left to the branch's schema: %v", err)
	}
	r.boot()
	for _, issue := range append(slices.Clone(r.walks), r.control) {
		r.on(issue.key)
		for role := range issue.sessions {
			if role != claim.RoleMerger {
				r.resume(issue, role)
			}
		}
	}
	return r
}

// storeBeforeThePlans is a database of this test's own carrying every embedded migration whose
// version is below the first one of the owner's plan (inFlightPlanMigration), applied by hand in
// version order (applyMigrationsByHand), which is the schema the release before leaves.
func storeBeforeThePlans(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := testdb.Database(t, "legion_in_flight")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store the release before leaves: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	first := 0
	for _, migration := range all {
		if strings.HasSuffix(migration.Name, inFlightPlanMigration) {
			first = migration.Version
		}
	}
	if first == 0 {
		t.Fatalf("no embedded migration ends %s, the first one of the owner's plan", inFlightPlanMigration)
	}
	var before []migrations.Migration
	for _, migration := range all {
		if migration.Version < first {
			before = append(before, migration)
		}
	}
	slices.SortFunc(before, func(x, y migrations.Migration) int { return cmp.Compare(x.Version, y.Version) })
	applyMigrationsByHand(t, st, before)
	return st, dsn
}

// seedInFlight writes issue as the release before leaves it in seeded, with plain SQL in that
// release's columns: the root handed to Legion, its tree under the workflow, its approved design
// gate, its slot while its phase holds one, the pull request it opened once it is past
// implementing, the implementer's phases row once it has implemented (pass and rounds 1 on the
// issue in reviewing, which had one round that requested changes), the row of the role at work at
// entry 1, and the claim of that role and of the architect each a launch the release before began
// in the session it recorded, which the boot resumes. Its phase is the seeded one's but on the
// control, which stands at admitted. The release before's consumer applied every event Dispatch
// logged for the issue up to the seed, under the id it applies a Dispatch event under
// (processed_events, `dispatch-<id>`), which a status write of this release reads to know the
// record has seen them (the outbox's awaitSeen, decision 11 of dispatch://INFRA-4730).
func (r *inFlightRig) seedInFlight(issue *inFlightIssue, seeded phase.Phase, status string, created createdIssue) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := r.st.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s in the columns of the release before: %v", what, err)
		}
	}
	key := issue.key
	exec("the root "+key, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
		handed_over, dispatch_status, repo, default_branch, github_repo)
		values ($1, $1, $2, $3, $4, 1, $5, $6, $7, true, $5, $8, 'main', $8)`,
		key, r.cfg.Project, issue.title, string(issue.stands), status, created.Rank, created.LastSeq, liveRepo)
	var events []struct {
		ID int64 `json:"id"`
	}
	r.person(http.MethodGet, fmt.Sprintf("/api/v1/issues/%s/events?after=0&limit=200", key), nil, http.StatusOK, &events)
	for _, event := range events {
		exec(fmt.Sprintf("the application of event %d of %s", event.ID, key), `insert into processed_events (source, event_id) values ('dispatch', $1)`,
			fmt.Sprintf("dispatch-%d", event.ID))
	}
	exec("the tree of "+key, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`, r.project, key)
	exec("the design gate of "+key, `insert into design_gates (issue, artifact_id, latest_version, approved_version, ever_opened, step)
		values ($1, $2, 1, 1, true, 'spec')`, key, "spec-"+key)
	order := slices.Index(phase.Order, seeded)
	if record.PhaseKeepsSlot(seeded) {
		exec("the slot of "+key, `insert into slots (issue, index, admitted_at) values ($1, $2, now() - interval '1 day')`,
			key, slices.Index(inFlightTitles, issue.title)+1)
	}
	if order > slices.Index(phase.Order, phase.Implementing) {
		exec("the pull request of "+key, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
			failing, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, state, mergeability, merge_sha)
			values ($1, $2, $3, $4, $5, now() - interval '3 hours', '[]', 0, 0, '[]', 1, '', '', 'open', 'MERGEABLE', '')`,
			key, liveRepo, issue.number, claim.IssueBranch(key), stepRecordsHead)
		counts := 0
		if seeded == phase.Reviewing {
			counts = 1
		}
		if seeded != phase.Retro {
			exec("the implementer's row of "+key, `insert into phases (issue, role, claim, handoff_commit, rounds, pass, verdict, summary,
				last_handoff, completed_at) values ($1, 'implementer', $2, $3, $4, $4, '', 'implemented', $5, now() - interval '2 hours')`,
				key, string(r.tokenOf(key, claim.RoleImplementer)), stepRecordsHead, counts, "implement-record-"+key)
		}
	}
	working := phase.RoleFor(seeded)
	if seeded == phase.Accepting {
		working = claim.RoleArchitect
	}
	if working != claim.RoleArchitect {
		handoff, last := "", ""
		if seeded == phase.Retro {
			handoff, last = stepRecordsHead, "implement-record-"+key
		}
		// The entry's task is the resume task of the seeded phase as the release before wrote it,
		// a literal of that release's text: this release's engine (workflow's Engine.ResumeTask)
		// reads the plan tables, which the store of the release before does not hold.
		resume := fmt.Sprintf("Continue %s. Issue: %s. Phase: %s. Resume the existing phase work.", issue.title, key, seeded)
		exec("the "+string(working)+"'s row of "+key, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, summary,
			last_handoff, entry, entry_task, entry_raised_at) values ($1, $2, $3, $4, 0, '', '', $5, 1, $6, now() - interval '1 hour')`,
			key, string(working), string(r.tokenOf(key, working)), handoff, last, resume)
	}
	claims := []claim.Role{claim.RoleArchitect}
	if working != claim.RoleArchitect {
		claims = append(claims, working)
	}
	for _, role := range claims {
		token := r.tokenOf(key, role)
		kept := inFlightSession{id: "ses_" + string(role) + "_" + strings.ToLower(key), file: filepath.Join(r.cfg.StateDir, key+"-"+string(role)+".jsonl")}
		issue.sessions[role] = kept
		exec("the "+string(role)+"'s claim on "+key, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, boot_token_hash, serving_generation,
			tree_epoch, plugin_contract) values ($1, $2, $3, $3, $4, 1, $5, $6, 'launching', 0, 0, 0, 0, $7, 1, 1, $8)`,
			string(token), r.project, key, string(role), kept.id, kept.file, supervise.HashBootToken("interrupted-"+string(token)),
			api.OwnerPlanContract-1)
		writePrompt(t, r.cfg, token)
	}
}

// resume is the pane of role's claim on issue coming up in the launch the boot made of it: the
// launch resumes the session the release before recorded, and the pane says hello with the launch's
// boot token, registers in that session and says ready, as the plugin of the deployed release does.
func (r *inFlightRig) resume(issue *inFlightIssue, role claim.Role) *stepClaimant {
	t := r.t
	t.Helper()
	token := r.tokenOf(issue.key, role)
	kept := issue.sessions[role]
	r.awaitLaunch(token)
	launch := lastLaunch(t, r.rt, token)
	if launch.ResumeSessionFile != kept.file {
		t.Fatalf("the boot launched %s with the session file %q, want a resume of %s, the one the release before recorded", token, launch.ResumeSessionFile, kept.file)
	}
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	// Each issue the release before left is a root, its own tree (seedInFlight).
	c := &stepClaimant{r: r.stepRecordsRig, token: token, issue: issue.key, tree: issue.key, role: role, session: kept.id, model: stepRecordsModel,
		bootToken: launch.BootToken, sh: dialShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: kept.id, OmpSessionFile: kept.file, AgentID: kept.id, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s in its session %s = %d %s, want 200", token, kept.id, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	sh, d := c.sh, r.d
	sh.serve(func() { c.serve(sh, d) })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: kept.id, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	r.claims[token] = c
	return c
}
