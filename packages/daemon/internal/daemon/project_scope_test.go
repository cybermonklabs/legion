package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
	t.Parallel()
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
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
	t.Parallel()
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
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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
		if err := store.Transact(ctx, pool, func(_ context.Context, tx pgx.Tx) error { return through(tx) }); err != nil {
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

// The project's records (projectRecords) write no outbox row keyed by another project's issue,
// whichever of their writes of a row by key it is (claim.KeyOfProject): such a row is run only by
// the daemon whose project its key names, against that daemon's own claims, so writing it is
// writing into that daemon's work. Each write — an insert, a start that takes out the start an
// issue owes, an expedite, an upsert that replaces a pending payload and its lease, a drop, and the
// clear of a generation, which queues the publication of the gates it empties — leaves the rows of
// the other project's key as they stand: it writes none, deletes none and changes none. It returns
// no error, since a row the daemon would never run must not fail the caller's own work, but for
// EnqueueID: every caller of that one names the row by the id it answers, and an id of no row
// names nothing, so it refuses with claim.ErrKeyOfAnotherProject and the caller's transaction
// rolls back. Each refusal is logged at Warn with the row's kind, its key and the project, so
// nothing is dropped in silence. The same write of a row of this project's own key goes through.
func TestTheProjectsRecordsWriteNoOutboxRowOfAnotherProjectsIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	raw := record.NewStore()
	var logs syncBuffer
	records := projectRecords{Store: raw, project: "LEGION", log: jsonLogger(&logs)}
	now := time.Now().UTC().Truncate(time.Microsecond)
	later := now.Add(time.Hour)
	write := func(t *testing.T, through func(pgx.Tx) error) {
		t.Helper()
		if err := store.Transact(ctx, pool, func(_ context.Context, tx pgx.Tx) error { return through(tx) }); err != nil {
			t.Fatal(err)
		}
	}
	// rowsOf is every outbox row keyed key as it stands — its id, kind, payload, due time and
	// lease — in id order.
	rowsOf := func(t *testing.T, key string) []string {
		t.Helper()
		found, err := pool.Query(ctx, `select concat_ws(' ', id, kind, payload, next_at, lease_token, lease_until)
			from outbox where issue = $1 order by id`, key)
		if err != nil {
			t.Fatalf("read the outbox rows keyed %s: %v", key, err)
		}
		rows, err := pgx.CollectRows(found, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the outbox rows keyed %s: %v", key, err)
		}
		return rows
	}
	// seed writes row through the raw store, as the daemon of its key's project writes its own
	// rows, and answers it with its id.
	seed := func(t *testing.T, row record.OutboxRow) record.OutboxRow {
		t.Helper()
		write(t, func(tx pgx.Tx) error {
			var err error
			row.ID, err = raw.EnqueueID(ctx, tx, row)
			return err
		})
		return row
	}
	// refused checks that the project's write of a row keyed key left the rows of that key as
	// they stood before it, and logged one Warn naming the row's kind, its key and the project.
	refused := func(t *testing.T, kind record.OutboxKind, key string, before []string) {
		t.Helper()
		if after := rowsOf(t, key); !slices.Equal(after, before) {
			t.Errorf("the rows keyed %s after the project's write = %q, want them as they stood, %q: the key is OTHERPROJECT's", key, after, before)
		}
		warned := 0
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["level"] == "WARN" && entry["kind"] == string(kind) &&
				entry["issue"] == key && entry["project"] == "LEGION" {
				warned++
			}
		}
		if warned != 1 {
			t.Errorf("the write logged %d Warn lines naming the %s row keyed %s and the project LEGION, want one; the log:\n%s",
				warned, kind, key, logs.String())
		}
		logs.Reset()
	}
	status := func(t *testing.T, key string, due time.Time) record.OutboxRow {
		return mustOutboxRow(t, key, record.OrdinaryStatusWrite("in_progress", "todo", ""), due)
	}

	t.Run("Enqueue", func(t *testing.T) {
		write(t, func(tx pgx.Tx) error { return records.Enqueue(ctx, tx, status(t, "OTHERPROJECT-1", now)) })
		refused(t, record.OutboxKindDispatchStatus, "OTHERPROJECT-1", nil)
		write(t, func(tx pgx.Tx) error { return records.Enqueue(ctx, tx, status(t, "LEGION-1", now)) })
		if got := rowsOf(t, "LEGION-1"); len(got) != 1 {
			t.Errorf("the project's own rows keyed LEGION-1 = %q, want the one row written", got)
		}
	})
	t.Run("EnqueueStart", func(t *testing.T) {
		start := func(key, task string) record.OutboxRow {
			return mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleImplementer, Generation: 1,
				Phase: phase.Implementing, Task: task}, now)
		}
		// The other project's daemon owes its issue a start, which a start written through
		// EnqueueStart takes out and writes over.
		seed(t, start("OTHERPROJECT-2", "Implement it."))
		before := rowsOf(t, "OTHERPROJECT-2")
		write(t, func(tx pgx.Tx) error {
			return records.EnqueueStart(ctx, tx, start("OTHERPROJECT-2", "Implement it again."))
		})
		refused(t, record.OutboxKindSupervise, "OTHERPROJECT-2", before)
		seed(t, start("LEGION-2", "Implement it."))
		write(t, func(tx pgx.Tx) error { return records.EnqueueStart(ctx, tx, start("LEGION-2", "Implement it again.")) })
		if got := rowsOf(t, "LEGION-2"); len(got) != 1 || !strings.Contains(got[0], "Implement it again.") {
			t.Errorf("the project's own rows keyed LEGION-2 = %q, want the one start, carrying the new task", got)
		}
	})
	t.Run("EnqueueID", func(t *testing.T) {
		enqueue := func(key string) (int64, error) {
			var id int64
			err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				id, err = records.EnqueueID(ctx, tx, status(t, key, now))
				return err
			})
			return id, err
		}
		if id, err := enqueue("OTHERPROJECT-3"); id != 0 || !errors.Is(err, claim.ErrKeyOfAnotherProject) {
			t.Errorf("EnqueueID of a row keyed OTHERPROJECT-3 = %d, %v; want 0 and claim.ErrKeyOfAnotherProject: its caller would name a row by that id", id, err)
		}
		refused(t, record.OutboxKindDispatchStatus, "OTHERPROJECT-3", nil)
		if id, err := enqueue("LEGION-3"); err != nil || id == 0 || len(rowsOf(t, "LEGION-3")) != 1 {
			t.Errorf("EnqueueID of the project's own row keyed LEGION-3 = %d, %v with the rows %q, want the id of the one row written",
				id, err, rowsOf(t, "LEGION-3"))
		}
	})
	t.Run("ExpediteOutbox", func(t *testing.T) {
		row := seed(t, status(t, "OTHERPROJECT-4", later))
		before := rowsOf(t, "OTHERPROJECT-4")
		write(t, func(tx pgx.Tx) error {
			_, err := records.ExpediteOutbox(ctx, tx, row, now)
			return err
		})
		refused(t, record.OutboxKindDispatchStatus, "OTHERPROJECT-4", before)
		own := seed(t, status(t, "LEGION-4", later))
		write(t, func(tx pgx.Tx) error {
			_, err := records.ExpediteOutbox(ctx, tx, own, now)
			return err
		})
		var due time.Time
		if err := pool.QueryRow(ctx, `select next_at from outbox where id = $1`, own.ID).Scan(&due); err != nil || !due.Equal(now) {
			t.Errorf("the project's own row %d keyed LEGION-4 is due at %s (%v), want it expedited to %s", own.ID, due, err, now)
		}
	})
	t.Run("EnqueueDesignGates", func(t *testing.T) {
		gates := func(key, artifact string) record.DesignGatesPublish {
			return record.DesignGatesPublish{Issue: key, Gates: []record.DesignGateDocument{{Step: record.DesignStepSpec, ArtifactID: artifact}}}
		}
		// The other project's daemon is sending its issue's set: it holds the row's lease, which a
		// replacement of the payload rotates.
		write(t, func(tx pgx.Tx) error {
			if err := raw.EnqueueDesignGates(ctx, tx, gates("OTHERPROJECT-5", "spec-1"), now); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `update outbox set lease_token = 'otherproject-lease', lease_until = $2 where issue = $1`, "OTHERPROJECT-5", later)
			return err
		})
		before := rowsOf(t, "OTHERPROJECT-5")
		write(t, func(tx pgx.Tx) error {
			return records.EnqueueDesignGates(ctx, tx, gates("OTHERPROJECT-5", "spec-2"), now)
		})
		refused(t, record.OutboxKindDesignGatesPublish, "OTHERPROJECT-5", before)
		write(t, func(tx pgx.Tx) error { return records.EnqueueDesignGates(ctx, tx, gates("LEGION-5", "spec-2"), now) })
		if got := rowsOf(t, "LEGION-5"); len(got) != 1 {
			t.Errorf("the project's own rows keyed LEGION-5 = %q, want the one publication written", got)
		}
	})
	t.Run("EnqueueGatedSteps", func(t *testing.T) {
		// The publish is keyed by the bare project key, under which the other project's daemon
		// has queued its own list.
		write(t, func(tx pgx.Tx) error {
			return raw.EnqueueGatedSteps(ctx, tx, record.GatedStepsPublish{Project: "OTHERPROJECT", Steps: []record.DesignStep{}}, now)
		})
		before := rowsOf(t, "OTHERPROJECT")
		steps := []record.DesignStep{record.DesignStepSpec}
		write(t, func(tx pgx.Tx) error {
			return records.EnqueueGatedSteps(ctx, tx, record.GatedStepsPublish{Project: "OTHERPROJECT", Steps: steps}, now)
		})
		refused(t, record.OutboxKindGatedStepsPublish, "OTHERPROJECT", before)
		write(t, func(tx pgx.Tx) error {
			return records.EnqueueGatedSteps(ctx, tx, record.GatedStepsPublish{Project: "LEGION", Steps: steps}, now)
		})
		if got := rowsOf(t, "LEGION"); len(got) != 1 {
			t.Errorf("the project's own rows keyed LEGION = %q, want the one publish written", got)
		}
	})
	t.Run("DropCatchUps", func(t *testing.T) {
		catchUp := func(key string) record.OutboxRow {
			return mustOutboxRow(t, key, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, CatchUp: &record.CatchUp{Generation: 1, Launch: 1}}, now)
		}
		seed(t, catchUp("OTHERPROJECT-6"))
		before := rowsOf(t, "OTHERPROJECT-6")
		write(t, func(tx pgx.Tx) error {
			_, err := records.DropCatchUps(ctx, tx, "OTHERPROJECT-6", now)
			return err
		})
		refused(t, record.OutboxKindNotice, "OTHERPROJECT-6", before)
		seed(t, catchUp("LEGION-6"))
		write(t, func(tx pgx.Tx) error {
			_, err := records.DropCatchUps(ctx, tx, "LEGION-6", now)
			return err
		})
		if got := rowsOf(t, "LEGION-6"); len(got) != 0 {
			t.Errorf("the project's own rows keyed LEGION-6 after the drop = %q, want its catch-up dropped", got)
		}
	})
	t.Run("DropStatusWrites", func(t *testing.T) {
		seed(t, status(t, "OTHERPROJECT-7", now))
		before := rowsOf(t, "OTHERPROJECT-7")
		write(t, func(tx pgx.Tx) error { return records.DropStatusWrites(ctx, tx, "OTHERPROJECT-7") })
		refused(t, record.OutboxKindDispatchStatus, "OTHERPROJECT-7", before)
		seed(t, status(t, "LEGION-7", now))
		write(t, func(tx pgx.Tx) error { return records.DropStatusWrites(ctx, tx, "LEGION-7") })
		if got := rowsOf(t, "LEGION-7"); len(got) != 0 {
			t.Errorf("the project's own rows keyed LEGION-7 after the drop = %q, want its status write dropped", got)
		}
	})
	t.Run("DropUnfencedCheckWakes", func(t *testing.T) {
		unfenced := func(key string) record.OutboxRow {
			return mustOutboxRow(t, key, record.CheckWakeDue{Look: 1, Reason: "an earlier release's wake"}, now)
		}
		seed(t, unfenced("OTHERPROJECT-10"))
		before := rowsOf(t, "OTHERPROJECT-10")
		write(t, func(tx pgx.Tx) error { return records.DropUnfencedCheckWakes(ctx, tx, "OTHERPROJECT-10") })
		refused(t, record.OutboxKindCheckWake, "OTHERPROJECT-10", before)
		seed(t, unfenced("LEGION-10"))
		seed(t, mustOutboxRow(t, "LEGION-10", record.CheckWakeDue{Look: 1, WakeAt: now.Add(time.Hour), Reason: "this release's wake"}, now))
		write(t, func(tx pgx.Tx) error { return records.DropUnfencedCheckWakes(ctx, tx, "LEGION-10") })
		if got := rowsOf(t, "LEGION-10"); len(got) != 1 {
			t.Errorf("the project's own rows keyed LEGION-10 after the drop = %q, want the wake that names its time alone", got)
		}
	})
	// A clear of a generation deletes the design gates it covers and queues, keyed by each issue
	// it emptied, the publication of the empty set (record.Postgres.clearGates).
	gated := func(t *testing.T, project, key string) {
		t.Helper()
		write(t, func(tx pgx.Tx) error {
			if err := raw.PutIssue(ctx, tx, record.Issue{Key: key, Tree: key, Project: project, Title: key, Phase: phase.Admitted, Generation: 1,
				Status: "todo", DispatchStatus: "todo", Rank: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
				return err
			}
			return raw.PutGate(ctx, tx, record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "spec-1", LatestVersion: 1})
		})
	}
	for _, generation := range []struct {
		name        string
		theirs, own string
		clear       func(tx pgx.Tx, key string) error
	}{
		{"ClearGeneration", "OTHERPROJECT-8", "LEGION-8", func(tx pgx.Tx, key string) error { return records.ClearGeneration(ctx, tx, key, now) }},
		{"ClearTreeGeneration", "OTHERPROJECT-9", "LEGION-9", func(tx pgx.Tx, key string) error { return records.ClearTreeGeneration(ctx, tx, key, now) }},
	} {
		t.Run(generation.name, func(t *testing.T) {
			gated(t, "OTHERPROJECT", generation.theirs)
			write(t, func(tx pgx.Tx) error { return generation.clear(tx, generation.theirs) })
			refused(t, record.OutboxKindDesignGatesPublish, generation.theirs, nil)
			write(t, func(tx pgx.Tx) error {
				gates, err := raw.GatesOf(ctx, tx, generation.theirs)
				if err == nil && record.SpecGate(gates) == nil {
					t.Errorf("the gate of %s after the project's clear = %v, want OTHERPROJECT's gate standing", generation.theirs, gates)
				}
				return err
			})
			gated(t, "LEGION", generation.own)
			write(t, func(tx pgx.Tx) error { return generation.clear(tx, generation.own) })
			if got := rowsOf(t, generation.own); len(got) != 1 {
				t.Errorf("the project's own rows keyed %s after the clear = %q, want the one publication of its emptied gates", generation.own, got)
			}
		})
	}
}

// The boot's sweep closes every issue of the project that awaits a merge GitHub already reported
// (workflow's sweepMerged), and it reads the issues through the project's records, which hold no
// record of another project's (projectRecords): on a shared database another project's daemon
// records its own issue awaiting a merge that GitHub reported, and this daemon's boot leaves it
// where that daemon stands it — still merging, lingering nowhere, with no production check
// opened and no row queued keyed by it — while it closes its own.
func TestTheBootSweepLeavesAMergedIssueOfAnotherProjectWhereItStands(t *testing.T) {
	t.Parallel()
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
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if own, err = raw.Issue(ctx, tx, ownIssue.Key); err != nil {
			return err
		}
		if theirs, err = raw.Issue(ctx, tx, theirIssue.Key); err != nil {
			return err
		}
		if check, err = raw.ProductionCheckOf(ctx, tx, theirIssue.Key, time.Time{}); err != nil {
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
