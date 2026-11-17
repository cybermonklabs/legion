package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Daemons of several projects share one database, and admission is each project's own. Another
// project's slots count against none of this daemon's cap, its waiting root is not this daemon's
// to promote, its stale slot is not this daemon's to release, and its outbox row, due before any of
// this daemon's, is not this daemon's to run: none of its issues is this daemon's to touch in
// Dispatch, and none of them ever gets a branch created on GitHub (refs/heads/legion/<key>) by this
// daemon.
func TestAnotherProjectsIssuesAreNotThisDaemonsToAdmit(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.AdmissionCap = 2
	other := "OTHER" + randomSuffix(t)
	running, stale, waiting := other+"-1", other+"-2", other+"-3"
	own := cfg.Project + "-1"

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the shared store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the shared store: %v", err)
	}
	records := record.NewStore()
	now := time.Now().UTC()
	// A slot's index is unique across every project's slots, so the seeded ones take indices above
	// any the shared database already holds.
	var base int
	root := func(key, status, rank string) record.Issue {
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: other, Title: "Another project's root", Phase: phase.Admitted, Generation: 1, Status: status, Rank: rank}
	}
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		taken, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		for _, slot := range taken {
			base = max(base, slot.Index+1)
		}
		for _, issue := range []record.Issue{root(running, "in_progress", "a"), root(stale, "backlog", "b"), root(waiting, "todo", "0")} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for index, key := range []string{running, stale} {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: base + index, AdmittedAt: now}); err != nil {
				return err
			}
		}
		// Due a minute ago, so an outbox that claimed across projects would run it before any row
		// this daemon's admission enqueues.
		row, err := record.NewOutboxRow(running, record.OrdinaryStatusWrite("in_progress", "todo", ""), now.Add(-time.Minute))
		if err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, row)
	}); err != nil {
		t.Fatalf("seed another project's admission: %v", err)
	}
	t.Cleanup(func() {
		if _, err := st.Pool().Exec(context.Background(), `delete from outbox where issue like $1`, other+"-%"); err != nil {
			t.Errorf("remove the seeded outbox row: %v", err)
		}
	})

	// ownRun closes when the outbox first reaches Dispatch for this project's issue: its first batch
	// after admission has run, and the other project's older row would have run before it.
	ownRun := make(chan struct{})
	var ownOnce sync.Once
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servedBootCall(t, w, r, own[:strings.Index(own, "-")]) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/issues/"+own) {
			ownOnce.Do(func() { close(ownRun) })
		}
		if strings.Contains(r.URL.Path, other) {
			t.Errorf("the daemon asked Dispatch about another project's issue: %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
			// This project's own writes after admission are not what this test is about.
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]map[string]any{{"key": own, "title": "This project's root", "status": "todo", "parent": nil, "rank": "m", "labels": []string{dispatch.LegionLabel}, "resolved_repo": "acme/widgets"}}); err != nil {
			t.Errorf("write Dispatch issues: %v", err)
		}
	}))
	t.Cleanup(dispatchServer.Close)
	cfg.DispatchURL = dispatchServer.URL
	github := newBranchGitHub(t, nil, branchCreated)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- run(runCtx, cfg, quietLogger(), overrides{
			listen:  heldListen,
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
			githubAPI: github.url,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	awaitHealthz(t, cfg, done)

	var active []string
	deadline := time.Now().Add(60 * time.Second)
	for !slices.Equal(active, []string{own}) {
		if time.Now().After(deadline) {
			t.Fatalf("admission is %v, want %s alone: another project's slots and waiting root are not this daemon's", active, own)
		}
		time.Sleep(50 * time.Millisecond)
		var state struct {
			Admission struct {
				Active []string `json:"active"`
			} `json:"admission"`
		}
		response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/legion/v1/state")
		if err != nil {
			continue
		}
		if err := json.NewDecoder(response.Body).Decode(&state); err == nil && response.StatusCode == http.StatusOK {
			active = state.Admission.Active
		}
		response.Body.Close()
	}

	select {
	case <-ownRun:
	case <-time.After(60 * time.Second):
		t.Fatalf("the outbox never reached Dispatch for %s after admitting it", own)
	}

	var slots []string
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		all, err := records.Slots(ctx, tx)
		for _, slot := range all {
			if strings.HasPrefix(slot.Issue, other+"-") {
				slots = append(slots, fmt.Sprintf("%s@%d", slot.Issue, slot.Index))
			}
		}
		return err
	}); err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	if want := []string{fmt.Sprintf("%s@%d", running, base), fmt.Sprintf("%s@%d", stale, base+1)}; !reflect.DeepEqual(slots, want) {
		t.Errorf("another project's slots = %v, want %v, untouched", slots, want)
	}
	var foreign []branchCreate
	for _, create := range github.created() {
		if strings.HasPrefix(create.ref, "refs/heads/legion/"+other+"-") {
			foreign = append(foreign, create)
		}
	}
	if len(foreign) != 0 {
		t.Errorf("GitHub saw %d branch create(s) for another project's issues: %+v, want none", len(foreign), foreign)
	}
}

// The project's records (projectRecords) answer no record of another project's issue, whatever
// key or tree a read names: the issues table holds every project's records, each under its
// project, and a record of another project is no record of this daemon's. A read by key answers
// none for another project's issue, a read by tree answers no member of another project's tree,
// and the two lists answer this project's records alone; each answers this project's own records
// as the store holds them. The raw store beside it reads every project's, so each case shows the
// other project's record is there to be read.
func TestTheProjectsRecordsReadNoRecordOfAnotherProjectsIssue(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	raw := record.NewStore()
	records := projectRecords{Store: raw, project: "LEGION"}
	// Each project holds a running root, a child in its tree, and a queued root whose chain of
	// parents runs through LEGION-1.
	of := func(project, key, tree, status string, stands phase.Phase) record.Issue {
		issue := record.Issue{Key: key, Tree: tree, Project: project, Title: key, Phase: stands, Generation: 1, Status: status,
			DispatchStatus: status, Rank: key, HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
		if key != tree {
			parent := tree
			issue.Parent = &parent
		}
		return issue
	}
	queued := func(project, key string) record.Issue {
		issue := of(project, key, key, "todo", phase.Admitted)
		parent := "LEGION-1"
		issue.Parent, issue.Ancestry = &parent, []string{parent}
		return issue
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, issue := range []record.Issue{
			of("LEGION", "LEGION-1", "LEGION-1", "in_progress", phase.Implementing),
			of("LEGION", "LEGION-2", "LEGION-1", "in_progress", phase.Implementing),
			queued("LEGION", "LEGION-3"),
			of("OTHERPROJECT", "OTHERPROJECT-1", "OTHERPROJECT-1", "in_progress", phase.Implementing),
			of("OTHERPROJECT", "OTHERPROJECT-2", "OTHERPROJECT-1", "in_progress", phase.Implementing),
			queued("OTHERPROJECT", "OTHERPROJECT-3"),
		} {
			if err := raw.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record both projects' issues: %v", err)
	}
	read := func(t *testing.T, through func(pgx.Tx) error) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, through); err != nil {
			t.Fatal(err)
		}
	}
	keysOf := func(issues []record.Issue) []string {
		keys := []string{}
		for _, issue := range issues {
			keys = append(keys, issue.Key)
		}
		return keys
	}

	t.Run("Issue", func(t *testing.T) {
		read(t, func(tx pgx.Tx) error {
			theirs, err := raw.Issue(ctx, tx, "OTHERPROJECT-1")
			if err != nil || theirs == nil {
				t.Fatalf("the raw store's record of OTHERPROJECT-1 = %v, %v; want OTHERPROJECT's record", theirs, err)
			}
			got, err := records.Issue(ctx, tx, "OTHERPROJECT-1")
			if err != nil || got != nil {
				t.Errorf("the project's record of OTHERPROJECT-1 = %+v, %v; want none: the record is OTHERPROJECT's", got, err)
			}
			own, err := records.Issue(ctx, tx, "LEGION-1")
			if err != nil || own == nil || own.Key != "LEGION-1" || own.Project != "LEGION" || own.Phase != phase.Implementing {
				t.Errorf("the project's record of LEGION-1 = %+v, %v; want LEGION's record of it", own, err)
			}
			return nil
		})
	})
	t.Run("TreeIssues", func(t *testing.T) {
		read(t, func(tx pgx.Tx) error {
			theirs, err := raw.TreeIssues(ctx, tx, "OTHERPROJECT-1")
			if err != nil || !reflect.DeepEqual(keysOf(theirs), []string{"OTHERPROJECT-1", "OTHERPROJECT-2"}) {
				t.Fatalf("the raw store's tree of OTHERPROJECT-1 = %v, %v; want OTHERPROJECT's two records", keysOf(theirs), err)
			}
			got, err := records.TreeIssues(ctx, tx, "OTHERPROJECT-1")
			if err != nil || len(got) != 0 {
				t.Errorf("the project's tree of OTHERPROJECT-1 = %v, %v; want no member: the tree is OTHERPROJECT's", keysOf(got), err)
			}
			own, err := records.TreeIssues(ctx, tx, "LEGION-1")
			if want := []string{"LEGION-1", "LEGION-2"}; err != nil || !reflect.DeepEqual(keysOf(own), want) {
				t.Errorf("the project's tree of LEGION-1 = %v, %v; want %v", keysOf(own), err, want)
			}
			return nil
		})
	})
	t.Run("Issues", func(t *testing.T) {
		read(t, func(tx pgx.Tx) error {
			got, err := records.Issues(ctx, tx)
			if want := []string{"LEGION-1", "LEGION-2", "LEGION-3"}; err != nil || !reflect.DeepEqual(keysOf(got), want) {
				t.Errorf("the project's records = %v, %v; want %v, none of OTHERPROJECT's", keysOf(got), err, want)
			}
			return nil
		})
	})
	t.Run("QueuedRootsUnder", func(t *testing.T) {
		read(t, func(tx pgx.Tx) error {
			theirs, err := raw.QueuedRootsUnder(ctx, tx, "LEGION-1")
			if err != nil || !slices.Contains(keysOf(theirs), "OTHERPROJECT-3") {
				t.Fatalf("the raw store's queued roots under LEGION-1 = %v, %v; want OTHERPROJECT-3 among them", keysOf(theirs), err)
			}
			got, err := records.QueuedRootsUnder(ctx, tx, "LEGION-1")
			if want := []string{"LEGION-3"}; err != nil || !reflect.DeepEqual(keysOf(got), want) {
				t.Errorf("the project's queued roots under LEGION-1 = %v, %v; want %v, none of OTHERPROJECT's", keysOf(got), err, want)
			}
			return nil
		})
	})
}

// The boot's sweep closes every issue of the project that awaits a merge GitHub already reported
// (workflow's sweepMerged), and it reads the issues through the project's records, which hold no
// record of another project's (projectRecords): on a shared database another project's daemon
// records its own issue awaiting a merge that GitHub reported, and this daemon's boot leaves it
// where that daemon stands it — still merging, lingering nowhere, with no production check
// opened and no row queued keyed by it — while it closes its own.
func TestTheBootSweepLeavesAMergedIssueOfAnotherProjectWhereItStands(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	raw := record.NewStore()
	records := projectRecords{Store: raw, project: "LEGION"}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, ProductionCheckBound: time.Hour, ProductionCheckRole: claim.RoleTester}, quietLogger())
	merging := func(project, key, repo string, number int) (record.Issue, record.PullRequest) {
		return record.Issue{Key: key, Repo: repo, DefaultBranch: "main", GitHubRepo: repo, Tree: key, Project: project, Title: key,
				Phase: phase.Merging, Generation: 1, Status: "retro", Rank: key},
			record.PullRequest{State: record.PullRequestMerged, Issue: key, Repo: repo, Number: number, Branch: "legion/" + key,
				HeadSHA: fmt.Sprintf("h%d", number), Failing: []string{}}
	}
	ownIssue, ownPR := merging("LEGION", "LEGION-301", "sjawhar/legion", 1)
	theirIssue, theirPR := merging("OTHER", "OTHER-1", "acme/other", 6)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, seed := range []struct {
			issue record.Issue
			pr    record.PullRequest
		}{{ownIssue, ownPR}, {theirIssue, theirPR}} {
			if err := raw.PutIssue(ctx, tx, seed.issue); err != nil {
				return err
			}
			if err := raw.PutPullRequest(ctx, tx, seed.pr); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the merged issues of both projects: %v", err)
	}

	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot-1", intake.DaemonBooted{Boot: 1}, engine); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}
	var own, theirs *record.Issue
	var check *record.ProductionCheck
	var rows int
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if own, err = raw.Issue(ctx, tx, ownIssue.Key); err != nil {
			return err
		}
		if theirs, err = raw.Issue(ctx, tx, theirIssue.Key); err != nil {
			return err
		}
		if check, err = raw.ProductionCheck(ctx, tx, theirIssue.Key); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `select count(*) from outbox where issue = $1`, theirIssue.Key).Scan(&rows)
	}); err != nil {
		t.Fatalf("read the records after the boot: %v", err)
	}
	if own == nil || own.Phase != phase.Done {
		t.Fatalf("%s after the boot = %+v, want it closed: its merge was reported", ownIssue.Key, own)
	}
	if theirs == nil || theirs.Project != "OTHER" || theirs.Phase != phase.Merging || theirs.Status != "retro" || theirs.LingerUntil != nil {
		t.Errorf("%s after the boot = %+v, want OTHER's record merging and lingering nowhere, as OTHER's daemon left it", theirIssue.Key, theirs)
	}
	if check != nil {
		t.Errorf("%s's production check after the boot = %+v, want none: the issue is OTHER's", theirIssue.Key, check)
	}
	if rows != 0 {
		t.Errorf("the boot queued %d outbox rows keyed %s, want none: OTHER's daemon runs every row of its issues", rows, theirIssue.Key)
	}
}
