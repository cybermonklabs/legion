package legion

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m.Run)) }

// fakeDaemon is a Legion daemon stand-in whose state document and status the test moves between
// passes, counting the reads it served.
type fakeDaemon struct {
	*httptest.Server
	mu     sync.Mutex
	body   string
	status int
	reads  int
}

func newFakeDaemon(t *testing.T, body string) *fakeDaemon {
	t.Helper()
	daemon := &fakeDaemon{body: body, status: http.StatusOK}
	daemon.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != StatePath {
			http.NotFound(w, r)
			return
		}
		daemon.mu.Lock()
		body, status := daemon.body, daemon.status
		daemon.reads++
		daemon.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(daemon.Close)
	return daemon
}

func (d *fakeDaemon) serves(body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.body, d.status = body, http.StatusOK
}

func (d *fakeDaemon) refuses(status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.body, d.status = `{"error":"the daemon is restarting"}`, status
}

func (d *fakeDaemon) served() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// stateOf is a one-issue state document: the issue in phase, with planner or implementer holding
// the only claim.
func stateOf(phase, role string) string {
	return fmt.Sprintf(`{"issues":{"INFRA-1":{"key":"INFRA-1","phase":%q,"status":"doing",
		"slot":{"index":0,"admittedAt":"2026-11-03T10:00:00Z"},
		"workers":{%q:{"claim":{"session":"s-1","state":"working"},"rounds":1}}}}}`, phase, role)
}

// configured is a store with project INFRA naming daemon, and a sampler on it.
func configured(t *testing.T, daemonURL string) (*store.Store, *Sampler) {
	t.Helper()
	database := storetest.Open(t)
	seedLegionProject(t, database, "INFRA", daemonURL)
	return database, NewSampler(database.Pool)
}

func seedLegionProject(t *testing.T, database *store.Store, project, daemonURL string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict do nothing`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	if _, err := PutSettings(context.Background(), database.Pool, project, daemonURL,
		model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("set the Legion setting of %s: %v", project, err)
	}
}

// observations is every observation of project, oldest first, as "<issue>/<field>=<value>".
func observations(t *testing.T, database *store.Store, project string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select issue_key, field, value::text from legion_observations where project = $1 order by observed_at, field
	`, project)
	if err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	defer rows.Close()
	var listed []string
	for rows.Next() {
		var issue, field, value string
		if err := rows.Scan(&issue, &field, &value); err != nil {
			t.Fatalf("scan an observation: %v", err)
		}
		listed = append(listed, issue+"/"+field+"="+value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	return listed
}

// readRow is one legion_reads row.
type readRow struct {
	ok    bool
	error *string
	bytes int
}

func reads(t *testing.T, database *store.Store, project string) []readRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select ok, error, bytes from legion_reads where project = $1 order by id
	`, project)
	if err != nil {
		t.Fatalf("read the reads: %v", err)
	}
	defer rows.Close()
	var listed []readRow
	for rows.Next() {
		var row readRow
		if err := rows.Scan(&row.ok, &row.error, &row.bytes); err != nil {
			t.Fatalf("scan a read: %v", err)
		}
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the reads: %v", err)
	}
	return listed
}

// TestAChangedValueWritesOneRowAndAnUnchangedValueWritesNone is the sampler's whole rule: a pass
// writes a row for each value that moved and nothing for the rest, a field the issue carries none
// of writes no first null, and a role the read no longer names is written null, since its claim is
// gone from the daemon's state.
func TestAChangedValueWritesOneRowAndAnUnchangedValueWritesNone(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)

	sampler.Pass(t.Context())
	first := observations(t, database, "INFRA")
	want := []string{
		`INFRA-1/claim.planner="working"`,
		`INFRA-1/phase="planning"`,
		`INFRA-1/slot=0`,
	}
	if len(first) != len(want) {
		t.Fatalf("the first pass wrote %v, want %v: a field the issue carries none of writes no first row", first, want)
	}
	for index, row := range want {
		if first[index] != row {
			t.Errorf("observation %d = %s, want %s", index, first[index], row)
		}
	}

	// Nothing moved, so nothing is written.
	sampler.Pass(t.Context())
	if again := observations(t, database, "INFRA"); len(again) != len(first) {
		t.Fatalf("a pass over an unchanged state wrote %d more rows: %v", len(again)-len(first), again)
	}

	// The phase moves and the planner hands over to the implementer: the phase and the new
	// claim are written, the status and the slot are not, and the planner's claim, which this
	// read no longer carries, is written null.
	daemon.serves(stateOf("implementing", "implementer"))
	sampler.Pass(t.Context())
	moved := observations(t, database, "INFRA")[len(first):]
	wantMoved := []string{
		`INFRA-1/claim.implementer="working"`,
		`INFRA-1/claim.planner=null`,
		`INFRA-1/phase="implementing"`,
	}
	if !slices.Equal(moved, wantMoved) {
		t.Errorf("the third pass wrote %v, want %v", moved, wantMoved)
	}

	if served := reads(t, database, "INFRA"); len(served) != 3 {
		t.Fatalf("legion_reads = %v, want one row per pass", served)
	} else {
		for index, row := range served {
			if !row.ok || row.error != nil || row.bytes == 0 {
				t.Errorf("read %d = %+v, want a successful read with its bytes", index, row)
			}
		}
	}
}

// TestAFailedReadRecordsTheFailureAndLeavesTheObservationsAlone: a daemon that refuses, and one
// that answers something other than the route's JSON, each leave the last good observations where
// they are and show up as the last failure.
func TestAFailedReadRecordsTheFailureAndLeavesTheObservationsAlone(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)

	sampler.Pass(t.Context())
	good := observations(t, database, "INFRA")
	if len(good) == 0 {
		t.Fatal("the first pass wrote nothing")
	}

	daemon.refuses(http.StatusServiceUnavailable)
	sampler.Pass(t.Context())
	daemon.serves(`{"issues": {`)
	sampler.Pass(t.Context())

	if after := observations(t, database, "INFRA"); len(after) != len(good) {
		t.Fatalf("a failed read changed the observations: %v, want the %d the good read left", after, len(good))
	}
	served := reads(t, database, "INFRA")
	if len(served) != 3 {
		t.Fatalf("legion_reads = %v, want one row per pass, failures included", served)
	}
	for index, row := range served[1:] {
		if row.ok || row.error == nil || *row.error == "" {
			t.Errorf("failed read %d = %+v, want ok false and the failure named", index+1, row)
		}
	}
	if served[2].bytes == 0 {
		t.Errorf("the unparseable read's bytes = 0, want what the daemon sent")
	}
}

// TestANULInAStringDoesNotLoseTheRead: jsonb refuses the NUL escape ("unsupported Unicode escape
// sequence", SQLSTATE 22P05), and a read's observations go in one statement, so one wait whose
// text carries a NUL failed every observation of the read, at every read. The NUL is stored as
// U+FFFD and the rest of the read with it, while an escaped backslash before `u0000` is six
// characters of text and no NUL, and is stored as the daemon sent it.
func TestANULInAStringDoesNotLoseTheRead(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)
	sampler.Pass(t.Context())

	daemon.serves(`{"issues":{"INFRA-1":{"key":"INFRA-1","phase":"awaiting_person",
		"slot":{"index":0},
		"waitingOn":{"kind":"ask","detail":"a\u0000b","path":"c\\u0000d"},
		"workers":{"planner":{"claim":{"state":"working"}}}}}}`)
	sampler.Pass(t.Context())

	served := reads(t, database, "INFRA")
	if len(served) != 2 || !served[1].ok || served[1].error != nil {
		t.Fatalf("legion_reads = %v, want the second read stored as a good read", served)
	}
	// newest is the text at path of the field's newest value, the value itself for no path.
	newest := func(field string, path ...string) string {
		t.Helper()
		var value string
		if err := database.Pool.QueryRow(t.Context(), `
			select value #>> $2::text[]
			from legion_observations
			where project = 'INFRA' and issue_key = 'INFRA-1' and field = $1
			order by observed_at desc
			limit 1
		`, field, append([]string{}, path...)).Scan(&value); err != nil {
			t.Fatalf("read the newest %s: %v", field, err)
		}
		return value
	}
	if phase := newest(FieldPhase); phase != PhaseAwaitingPerson {
		t.Errorf("the newest phase = %q, want %q: the read's other observations are stored beside the NUL", phase, PhaseAwaitingPerson)
	}
	if detail := newest(FieldWaitingOn, "detail"); detail != "a\uFFFDb" {
		t.Errorf("the wait's detail = %q, want the NUL stored as U+FFFD", detail)
	}
	if path := newest(FieldWaitingOn, "path"); path != `c\u0000d` {
		t.Errorf("the wait's path = %q, want the escaped backslash's text as the daemon sent it", path)
	}
}

// TestTwoProcessesStoreEachChangeOnce is acceptance A4: production runs two envoy-dispatch
// processes against one Postgres, each with a pool of its own (storetest.SecondProcess), and the
// `legion-state` lease is what makes one of them serve a period. Both tick for the same moment;
// one pass runs, one daemon read is made, and each change is stored once.
func TestTwoProcessesStoreEachChangeOnce(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, first := configured(t, daemon.URL)
	second := storetest.SecondProcess(t, database)
	other := NewSampler(second.Pool)

	firstLeases := storetest.Leases(t, database.Pool)
	secondLeases := storetest.Leases(t, second.Pool)

	// The two ticks a RunLeased loop makes for one period, with the moment each was due: the
	// value the ticker delivered, which is the same on both processes.
	due := time.Now()
	ranFirst := duty.Exclusive(t.Context(), firstLeases, SampleLease, due, SampleInterval, first.Pass)
	ranSecond := duty.Exclusive(t.Context(), secondLeases, SampleLease, due, SampleInterval, other.Pass)

	if ranFirst == ranSecond {
		t.Fatalf("first ran = %v, second ran = %v: exactly one process serves a period", ranFirst, ranSecond)
	}
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads for one period, want one", served)
	}
	if rows := reads(t, database, "INFRA"); len(rows) != 1 {
		t.Errorf("legion_reads = %v, want one row for one period", rows)
	}
	stored := observations(t, database, "INFRA")
	if len(stored) != 3 {
		t.Fatalf("observations = %v, want each change stored once", stored)
	}

	// The next period, with the phase moved: whichever process serves it stores that one change
	// once, and the other stores nothing.
	daemon.serves(stateOf("implementing", "planner"))
	next := due.Add(SampleInterval)
	duty.Exclusive(t.Context(), secondLeases, SampleLease, next, SampleInterval, other.Pass)
	duty.Exclusive(t.Context(), firstLeases, SampleLease, next, SampleInterval, first.Pass)
	if moved := observations(t, database, "INFRA")[len(stored):]; len(moved) != 1 || moved[0] != `INFRA-1/phase="implementing"` {
		t.Fatalf("the second period wrote %v, want the one change once", moved)
	}
}

// TestAProjectWithNoSettingIsNeverRead: the sampler reads what legion_settings names and nothing
// else, and a setting removed stops its reads while what was observed stays.
func TestAProjectWithNoSettingIsNeverRead(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)

	sampler.Pass(t.Context())
	before := observations(t, database, "INFRA")

	deleted, err := DeleteSettings(t.Context(), database.Pool, "INFRA")
	if err != nil || !deleted {
		t.Fatalf("delete the setting: deleted=%v err=%v", deleted, err)
	}
	sampler.Pass(t.Context())

	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want only the one made while it was configured", served)
	}
	if after := observations(t, database, "INFRA"); len(after) != len(before) {
		t.Errorf("observations = %v, want the %d the configured read left", after, len(before))
	}
	if rows := reads(t, database, "INFRA"); len(rows) != 1 {
		t.Errorf("legion_reads = %v, want the one read", rows)
	}
}

// TestAPassPrunesTheReadsPastTheirRetention: legion_reads gains a row per project per minute and
// nothing but this deletes them, so a pass drops every read older than ReadRetention — and keeps
// each project's newest whatever its age, because that row is the freshness the board's header
// shows.
func TestAPassPrunesTheReadsPastTheirRetention(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)
	// A project whose daemon nothing reads any more: its setting was removed, and the reads it
	// left stay (DeleteSettings). It is here to prove the prune keeps a project's newest read
	// however old it is, since that row is the freshness the board's header shows.
	if _, err := database.Pool.Exec(t.Context(),
		`insert into projects (key, name, default_assignee) values ('LINUX', 'LINUX', 'alice')`); err != nil {
		t.Fatalf("seed project LINUX: %v", err)
	}
	// Each aged read is stamped from the database's own now(), the clock PruneReads cuts off by,
	// so the row a minute past the retention is past it by that clock whatever this process's
	// clock says.
	aged := func(project string, age time.Duration) {
		t.Helper()
		var now time.Time
		if err := database.Pool.QueryRow(t.Context(), `select now()`).Scan(&now); err != nil {
			t.Fatalf("read the database's clock: %v", err)
		}
		if _, err := RecordRead(t.Context(), database.Pool, project, now.UTC().Add(-age), 2048, nil); err != nil {
			t.Fatalf("record a read of %s: %v", project, err)
		}
	}
	for _, age := range []time.Duration{ReadRetention + time.Hour, ReadRetention + time.Minute, ReadRetention - time.Hour} {
		aged("INFRA", age)
	}
	aged("LINUX", 30*24*time.Hour)

	sampler.Pass(t.Context())

	// INFRA: the read this pass made, and the one inside the retention; its two older rows are
	// gone. LINUX's one ancient row stays, because it is that project's newest.
	if rows := reads(t, database, "INFRA"); len(rows) != 2 {
		t.Errorf("INFRA's reads = %v, want the pass's own and the one inside the retention", rows)
	}
	if rows := reads(t, database, "LINUX"); len(rows) != 1 {
		t.Errorf("LINUX's reads = %v, want the one it had, old as it is", rows)
	}

	// A second pass with nothing to delete leaves the rows where they are.
	if pruned, err := PruneReads(t.Context(), database.Pool); err != nil || pruned != 0 {
		t.Errorf("a second prune deleted %d (err %v), want none", pruned, err)
	}
}

// Every read a pass makes is announced on StateChannel, with the project as the payload, a read
// that failed as much as one that moved something: that announcement is the one thing that wakes
// an open Legion board, since nothing commits in Dispatch when a remote daemon's state moves, and
// the board's header carries the read's own time and failure (spec R2, the `/ws/live` view
// `legion` pushing the change with no new request). The live view's own tests announce on the
// channel themselves, so a sampler that stopped announcing would leave every open board frozen at
// its first answer while every one of them still passed.
func TestEachReadOfAPassIsAnnouncedToTheOpenBoards(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)
	listener, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("take a connection to listen on: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(t.Context(), "listen "+StateChannel); err != nil {
		t.Fatalf("listen on %s: %v", StateChannel, err)
	}
	announced := func(what string) {
		t.Helper()
		notification, err := listener.Conn().WaitForNotification(testwait.Context(t))
		if err != nil {
			t.Fatalf("%s: no announcement on %s (%v): an open board would never hear this read", what, StateChannel, err)
		}
		if notification.Payload != "INFRA" {
			t.Errorf("%s: announced %q, want the project INFRA", what, notification.Payload)
		}
	}

	sampler.Pass(t.Context())
	announced("a read that moved the observations")

	daemon.refuses(http.StatusServiceUnavailable)
	sampler.Pass(t.Context())
	announced("a read that failed")
}

// TestAReadWhoseObservationsAreRefusedLeavesNoReadSayingItSucceeded: RecordRead stores a read's
// observations and its legion_reads row together or not at all. When Postgres refuses an
// observation, no row may say the read succeeded, since the board's header would then show a good
// read over data Dispatch never stored, and the last good read must stand.
func TestAReadWhoseObservationsAreRefusedLeavesNoReadSayingItSucceeded(t *testing.T) {
	database := storetest.Open(t)
	seedLegionProject(t, database, "INFRA", "http://127.0.0.1:1")
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	refused := []Observation{{IssueKey: "INFRA-1", Field: "phase", Value: []byte(`not json`)}}
	if _, err := RecordRead(t.Context(), database.Pool, "INFRA", at, 42, refused); err == nil {
		t.Fatal("RecordRead stored an observation that is not JSON, want Postgres to refuse it")
	}
	var reads, observations int
	if err := database.Pool.QueryRow(t.Context(),
		`select count(*) from legion_reads where project = 'INFRA'`).Scan(&reads); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(t.Context(),
		`select count(*) from legion_observations where project = 'INFRA'`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if reads != 0 || observations != 0 {
		t.Errorf("after a refused read: %d legion_reads rows and %d observations, want none of either", reads, observations)
	}
}

// TestTheNewestReadsAnswerTheNewestGoodReadAndKeepAnIssueItDropped: ReadCurrent answers the newest
// good read. Its Newest, the board's cards, holds no field that read no longer carries, and the
// project's own admission block under AdmissionKey; its Reported, by which the board and the
// metrics place an issue in a tree, keeps an issue that left the daemon's state after Dispatch
// observed a phase of it, with no tree, since its `tree` left with it, and names no issue for the
// admission block. It reads legion_current, which the trigger keeps equal to the newest row of
// each field the history holds, `null` rows included.
func TestTheNewestReadsAnswerTheNewestGoodReadAndKeepAnIssueItDropped(t *testing.T) {
	daemon := newFakeDaemon(t, `{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"implementing","tree":"INFRA-1",
			"workers":{"implementer":{"claim":{"state":"working"}}}},
		"INFRA-2":{"key":"INFRA-2","phase":"testing","tree":"INFRA-1"}}}`)
	database, sampler := configured(t, daemon.URL)
	sampler.Pass(t.Context())
	// The second read moves INFRA-1 on, drops its implementer's claim, no longer carries INFRA-2,
	// and carries an admission block.
	daemon.serves(`{"issues":{"INFRA-1":{"key":"INFRA-1","phase":"reviewing","tree":"INFRA-1"}},
		"admission":{"cap":2}}`)
	sampler.Pass(t.Context())

	state, err := ReadCurrent(t.Context(), database.Pool, "INFRA")
	if err != nil {
		t.Fatalf("read the current state: %v", err)
	}
	var cards []string
	for _, one := range state.Newest {
		cards = append(cards, one.IssueKey+"/"+one.Field+"="+string(one.Value))
	}
	slices.Sort(cards)
	if want := []string{`/admission={"cap": 2}`, `INFRA-1/phase="reviewing"`, `INFRA-1/tree="INFRA-1"`}; !slices.Equal(cards, want) {
		t.Errorf("the newest observations are %v, want %v: the newest read carries nothing else", cards, want)
	}
	if want := map[string]string{"INFRA-1": "INFRA-1", "INFRA-2": ""}; !maps.Equal(state.Reported, want) {
		t.Errorf("the reported issues are %v, want %v: INFRA-2 left the daemon's state after Dispatch observed its phase, and its tree left with it", state.Reported, want)
	}

	rowsOf := func(query string) []string {
		t.Helper()
		rows, err := database.Pool.Query(t.Context(), query)
		if err != nil {
			t.Fatalf("read %s: %v", query, err)
		}
		listed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read %s: %v", query, err)
		}
		return listed
	}
	current := rowsOf(`
		select issue_key || '/' || field || '=' || value::text || '@' || observed_at::text
		from legion_current where project = 'INFRA' order by issue_key, field`)
	history := rowsOf(`
		select issue_key || '/' || field || '=' || value::text || '@' || observed_at::text
		from (
			select distinct on (issue_key, field) issue_key, field, value, observed_at
			from legion_observations where project = 'INFRA'
			order by issue_key, field, observed_at desc
		) newest order by issue_key, field`)
	if !slices.Equal(current, history) {
		t.Errorf("legion_current holds %v, want the newest row of each field of the history, %v", current, history)
	}
}

// TestReportedNamesAnIssuesTreeWhateverOrderItsRowsComeBackIn: ReadCurrent's statement sets no
// order, and Postgres gives none without one, so Reported must name an issue's tree whichever of
// the issue's rows the scan meets last. The issue's tree is written by a read before the one that
// writes its phase and its slot, and ReadCurrent reads the rows in two orders the test sets rather
// than the one Postgres picks: by field, which puts the `tree` row after the `phase` and `slot`
// rows, and the reverse, which puts it before them. A reader that let a later row of the issue
// overwrite the tree an earlier one named fails the second, whatever order Postgres would return.
func TestReportedNamesAnIssuesTreeWhateverOrderItsRowsComeBackIn(t *testing.T) {
	database := storetest.Open(t)
	seedLegionProject(t, database, "INFRA", "http://127.0.0.1:1")
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	tree := Observation{IssueKey: "INFRA-1", Field: FieldTree, Value: []byte(`"INFRA-1"`)}
	if _, err := RecordRead(t.Context(), database.Pool, "INFRA", at, 42, []Observation{tree}); err != nil {
		t.Fatalf("record the read that names the tree: %v", err)
	}
	if _, err := RecordRead(t.Context(), database.Pool, "INFRA", at.Add(time.Minute), 42, []Observation{
		{IssueKey: "INFRA-1", Field: FieldPhase, Value: []byte(`"planning"`)},
		tree,
		{IssueKey: "INFRA-1", Field: FieldSlot, Value: []byte(`0`)},
	}); err != nil {
		t.Fatalf("record the read that adds the phase and the slot: %v", err)
	}

	for _, order := range []string{"field", "field desc"} {
		current, err := ReadCurrent(t.Context(), inOrder{db: database.Pool, order: order}, "INFRA")
		if err != nil {
			t.Fatalf("read the current state ordered by %s: %v", order, err)
		}
		if want := map[string]string{"INFRA-1": "INFRA-1"}; !maps.Equal(current.Reported, want) {
			t.Errorf("with the rows ordered by %s, the reported issues are %v, want %v: the tree INFRA-1's own row names",
				order, current.Reported, want)
		}
	}
}

// inOrder is a Queryer that answers a statement's rows in the order its clause sets, around a
// statement that sets none.
type inOrder struct {
	db    Queryer
	order string
}

func (q inOrder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.db.Query(ctx, "select * from ("+sql+") as unordered order by "+q.order, args...)
}
