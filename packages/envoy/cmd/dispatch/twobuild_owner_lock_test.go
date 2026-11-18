//go:build twobuild

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// ───── one owner's writers across the two builds ───────────────────────────

// The previous build serialises an owner's writers on the owner's row: each takes the row `for no
// key update` first and holds it to its commit. This tree serialises them on the owner's advisory
// lock, which the previous build never takes, and takes the row too while a process of a build
// before the lock protocol is live (store.LockOwner, server_processes.owner_lock_protocol 0). This
// row runs a process of each build on one database and puts one write of each on one issue at
// once, the previous build's first: the test holds the issue's row on its own connection, sends
// the previous build's write, which queues on that row, then this build's, and lets the row go.
// The previous build's write then commits first, and this build's must decide on what it wrote.
// Each direction is one way a write of this build that read the issue before that commit goes
// wrong: this build's write accepted where the previous build's made it a refusal, and the
// previous build's write lost under this build's.

// The application_name each build's connections carry, so the test tells their backends apart in
// pg_stat_activity.
const (
	previousApplication = "dispatch-previous-build"
	currentApplication  = "dispatch-current-build"
)

// previousRowLock is the statement the previous build's PATCH takes an issue's row with, first,
// before it reads the issue (its api.lockIssueRows).
const previousRowLock = `select parent_key from issues where key = $1 for no key update`

// currentIssueStatement matches a statement of this build that names the issue's row: the row taken
// beside the advisory lock in the read phase, the advisory lock itself, or a write phase's row lock
// or update.
var currentIssueStatement = regexp.MustCompile(`issues|owner_lock`)

// startNamed starts a process of the previous build (previous) or of this tree, its connections
// named application, and returns once it answers /healthz.
func (b *twoBuilds) startNamed(t *testing.T, previous bool, application string) *dispatchProcess {
	t.Helper()
	options := b.options(nil)
	url := b.databaseURL(t) + "&application_name=" + application
	if previous {
		options.Binary, options.WebDist = b.build.path("envoy-dispatch"), b.build.path("web")
		b.a = startDispatchProcessWith(t, url, options)
		b.a.waitHealthy(t)
		return b.a
	}
	options.Binary = b.binary
	b.b = startDispatchProcessWith(t, url, options)
	b.b.waitHealthy(t)
	return b.b
}

// queuedBackend waits up to within for a backend whose connections carry application, and whose
// current statement matches statement, to queue behind a lock holder holds (storetest.QueuedBehind),
// and answers its process id and statement.
func queuedBackend(t *testing.T, database *store.Store, holder pgx.Tx, application string, statement *regexp.Regexp, within time.Duration) (uint32, string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for {
		rows, err := database.Pool.Query(ctx, `
			select pid, query from pg_stat_activity
			where datname = current_database() and application_name = $1 and wait_event_type = 'Lock'`, application)
		if err != nil {
			t.Fatal(err)
		}
		type backend struct {
			PID   uint32
			Query string
		}
		backends, err := pgx.CollectRows(rows, pgx.RowToStructByPos[backend])
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range backends {
			if !statement.MatchString(candidate.Query) {
				continue
			}
			if queued, err := storetest.QueuedBehind(ctx, holder, candidate.PID); err != nil {
				t.Fatal(err)
			} else if queued {
				return candidate.PID, candidate.Query
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend of %s running a statement matching %s queued behind the issue's row within %s; its backends waiting on a lock: %+v",
				application, statement, within, backends)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// crossBuildWrite is one HTTP request a race sends.
type crossBuildWrite struct {
	process      *dispatchProcess
	method, path string
	body         string
}

// send sends w on another goroutine and delivers its answer.
func (w crossBuildWrite) send() <-chan httpAnswer {
	answered := make(chan httpAnswer, 1)
	go func() {
		answer, err := w.process.trySend(w.method, w.path, w.body, asUser(dispatchTestLogin))
		if err != nil {
			answer = httpAnswer{status: -1, body: []byte(err.Error())}
		}
		answered <- answer
	}()
	return answered
}

// raceAcrossBuilds holds issue's row on the test's own connection, sends first (the previous
// build's write) and waits for it to queue on that row in its first statement, sends second (this
// build's) and waits for it to queue behind the row too, then lets the row go, and answers both
// answers and the statement this build's write waited in.
func raceAcrossBuilds(t *testing.T, builds *twoBuilds, issue string, first, second crossBuildWrite) (httpAnswer, httpAnswer, string) {
	t.Helper()
	ctx := context.Background()
	holder, err := builds.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue); err != nil {
		t.Fatalf("hold the issue's row: %v", err)
	}
	firstAnswered := first.send()
	queuedBackend(t, builds.database, holder, previousApplication, regexp.MustCompile(`^`+regexp.QuoteMeta(previousRowLock)+`$`), 20*time.Second)
	secondAnswered := second.send()
	_, waitingOn := queuedBackend(t, builds.database, holder, currentApplication, currentIssueStatement, 20*time.Second)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var answers [2]httpAnswer
	for index, answered := range []<-chan httpAnswer{firstAnswered, secondAnswered} {
		select {
		case answers[index] = <-answered:
		case <-time.After(60 * time.Second):
			t.Fatalf("a write of the race never answered within 60s of the issue's row being let go")
		}
	}
	return answers[0], answers[1], waitingOn
}

// A write of each build on one issue at once, the previous build's committing first, decides as if
// the two had run one after the other: this build's PATCH conditional on the issue's sequence
// (expected_sequence) is refused 409 ISSUE_CHANGED once the previous build's PATCH has moved that
// sequence, and the previous build's label survives this build's labels_add. This build's process
// takes the issue's row in its read phase while the previous build's process is live, and logs
// that its owner locks now do. (A close would be the plainer refusal, but the previous build's
// close settles the issue's documents first, on the issue's row, before its own transaction, so
// the row the test holds would queue that settlement rather than the close.)
func TestTwoBuildsSerialiseOneOwnersWritersBothWays(t *testing.T) {
	builds := newTwoBuilds(t)
	builds.startNamed(t, true, previousApplication)
	current := builds.startNamed(t, false, currentApplication)
	previous := builds.previous()
	project := uniqueProjectKey(t, "OWN")
	current.createProject(t, project, "Owner locks")

	t.Run("this build's conditional PATCH beside the previous build's PATCH", func(t *testing.T) {
		issue, _ := current.createIssueIn(t, project, "Retitled by both builds", "A spec.\n")
		var sequence int64
		if err := builds.database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		const previousTitle, currentTitle = "Retitled by the previous build", "Retitled by this build"
		first, second, waitingOn := raceAcrossBuilds(t, builds, issue,
			crossBuildWrite{process: previous, method: http.MethodPatch, path: "/api/v1/issues/" + issue, body: `{"title":"` + previousTitle + `"}`},
			crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
				body: fmt.Sprintf(`{"title":%q,"expected_sequence":%d}`, currentTitle, sequence)})
		t.Logf("this build's PATCH waited in %q", waitingOn)
		if first.status != http.StatusOK {
			t.Fatalf("the %s's PATCH answered %s", builds.previousName(), first)
		}
		if second.status != http.StatusConflict || second.code() != "ISSUE_CHANGED" {
			t.Errorf("this build's PATCH conditional on sequence %d, sent while the %s's PATCH that moved it was under way, answered %s; want 409 ISSUE_CHANGED, as it answers once that PATCH has committed",
				sequence, builds.previousName(), second)
		}
		var title string
		if err := builds.database.Pool.QueryRow(context.Background(), `select title from issues where key = $1`, issue).Scan(&title); err != nil {
			t.Fatal(err)
		}
		if title != previousTitle {
			t.Errorf("the issue's title is %q, want the %s's %q: this build read the sequence before that PATCH committed and wrote over it",
				title, builds.previousName(), previousTitle)
		}
	})

	t.Run("the previous build's label beside this build's labels_add", func(t *testing.T) {
		issue, _ := current.createIssueIn(t, project, "Labelled by both builds", "A spec.\n")
		first, second, waitingOn := raceAcrossBuilds(t, builds, issue,
			crossBuildWrite{process: previous, method: http.MethodPatch, path: "/api/v1/issues/" + issue, body: `{"labels_add":["previous-build"]}`},
			crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue, body: `{"labels_add":["current-build"]}`})
		t.Logf("this build's labels_add waited in %q", waitingOn)
		if first.status != http.StatusOK || second.status != http.StatusOK {
			t.Fatalf("the %s's PATCH answered %s and this build's %s", builds.previousName(), first, second)
		}
		var labels []string
		if err := builds.database.Pool.QueryRow(context.Background(), `select labels from issues where key = $1`, issue).Scan(&labels); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(labels, "previous-build") || !slices.Contains(labels, "current-build") {
			t.Errorf("the issue's labels are %v; want the %s's and this build's both: this build read the labels before the %s's commit and wrote over them",
				labels, builds.previousName(), builds.previousName())
		}
	})

	current.WaitForOutput(t, "owner locks now take the owner's row too")
}

// ───── the previous build's boot beside a write of this build in flight ─────

// gateWaiter is the backend of the previous build that waits for the owner lock gate behind the
// backend blocker, exclusively, as an earlier build's arrival takes it (store.OwnerLockGateNamespace,
// migration 20261115200014_owner_lock_legacy_gate), with the statement it waits in; 0 when none
// does.
func gateWaiter(t *testing.T, database *store.Store, blocker uint32) (uint32, string) {
	t.Helper()
	var pid uint32
	var query string
	err := database.Pool.QueryRow(context.Background(), `
		select activity.pid, activity.query
		from pg_stat_activity activity join pg_locks lock on lock.pid = activity.pid
		where activity.datname = current_database() and activity.application_name = $1
		  and lock.locktype = 'advisory' and not lock.granted and lock.mode = 'ExclusiveLock'
		  and lock.classid::bigint = $2 and lock.objid::bigint = $3 and lock.objsubid = 2
		  and $4::integer = any(pg_blocking_pids(activity.pid))`,
		previousApplication, store.OwnerLockGateNamespace, store.OwnerLockGateKey, blocker).Scan(&pid, &query)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ""
	}
	if err != nil {
		t.Fatalf("read the backends waiting for the owner lock gate: %v", err)
	}
	return pid, query
}

// pauseLabelWrites holds each write of an issue's labels by a backend whose connections carry
// application as that statement opens, before it reaches a row, until the transaction it returns
// ends: a trigger has each UPDATE naming issues.labels on such a backend take a shared hold on a
// key the returned transaction holds. A writer of the issue's row on a backend of the other build,
// or of the test, passes through.
func pauseLabelWrites(t *testing.T, database *store.Store, application string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	const holdLabels = int64(4815162346)
	for _, statement := range []string{
		`create function dispatch_test_hold_labels() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(tg_argv[0]::bigint);
			return null;
		end;
		$$`,
		fmt.Sprintf(`create trigger dispatch_test_hold_labels before update of labels on issues for each statement
			when (current_setting('application_name') = '%s') execute function dispatch_test_hold_labels('%d')`, application, holdLabels),
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the label writes of %s: %v", application, err)
		}
	}
	pause, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pause.Exec(ctx, `select pg_advisory_xact_lock($1)`, holdLabels); err != nil {
		_ = pause.Rollback(ctx)
		t.Fatalf("take the hold the label writes wait on: %v", err)
	}
	return pause
}

// currentLabelWrite matches this build's update of an issue's labels, the statement
// pauseLabelWrites holds.
var currentLabelWrite = regexp.MustCompile(`update issues set labels`)

// The previous build boots, with its web app as the image ships it, while a write of this build
// is between its owner lock and its write phase: a PATCH adding a label, sent while no earlier
// build was in the fleet, so it read the labels under the advisory lock alone and holds the owner
// lock gate shared. The test pauses it as its write phase opens, at its update of the labels and
// before that update reaches the issue's row (pauseLabelWrites), so no writer of the issue's row
// waits on the pause. The previous build's boot publishes its dashboard before it writes anything
// else, and that publish's insert takes the gate exclusively, so the boot waits there until the
// PATCH commits; once it serves, a label it adds lands after the PATCH's and both are kept.
// Without the gate the previous build boots at once, its label is written while the PATCH waits,
// and the PATCH then writes the labels it read over it.
func TestThePreviousBuildsBootWaitsForAWriteInFlightAndItsWriteIsKept(t *testing.T) {
	builds := newTwoBuilds(t)
	current := builds.startNamed(t, false, currentApplication)
	project := uniqueProjectKey(t, "GATE")
	current.createProject(t, project, "Owner lock gate")
	issue, _ := current.createIssueIn(t, project, "Labelled across a boot", "A spec.\n")
	ctx := context.Background()

	pause := pauseLabelWrites(t, builds.database, currentApplication)
	defer pause.Rollback(ctx)
	patched := crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["current-build"]}`}.send()
	patchPID, waitingOn := queuedBackend(t, builds.database, pause, currentApplication, currentLabelWrite, 20*time.Second)
	t.Logf("this build's PATCH waits in %q", waitingOn)

	options := builds.options(nil)
	options.Binary, options.WebDist = builds.build.path("envoy-dispatch"), builds.build.path("web")
	started := time.Now()
	builds.a = startDispatchProcessWith(t, builds.databaseURL(t)+"&application_name="+previousApplication, options)
	previous := builds.a

	var gatePID uint32
	var gateQuery string
	booted := false
	for deadline := time.Now().Add(60 * time.Second); gatePID == 0 && !booted; {
		if gatePID, gateQuery = gateWaiter(t, builds.database, patchPID); gatePID != 0 {
			break
		}
		booted = previous.healthyWithin(50*time.Millisecond) == nil
		if !booted && time.Now().After(deadline) {
			t.Fatalf("the %s neither served nor waited for the owner lock gate within 60s:\n%s", builds.previousName(), previous.Output.String())
		}
	}
	labelPrevious := crossBuildWrite{process: previous, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["previous-build"]}`}
	if booted {
		t.Errorf("the %s served %s after it started while this build's PATCH, which read no earlier build in the fleet, was between its owner lock and its write; want its boot to wait at its dashboard publish for that PATCH (the owner lock gate)",
			builds.previousName(), time.Since(started).Round(time.Millisecond))
		if answer := <-labelPrevious.send(); answer.status != http.StatusOK {
			t.Fatalf("the %s's PATCH answered %s", builds.previousName(), answer)
		}
		if err := pause.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("the %s's boot waits for the owner lock gate %s after it started, in %q", builds.previousName(),
			time.Since(started).Round(time.Millisecond), gateQuery)
		if !strings.Contains(gateQuery, "insert into dashboard_builds") {
			t.Errorf("the %s waits for the gate in %q, want its dashboard publish's insert", builds.previousName(), gateQuery)
		}
		const held = 3 * time.Second
		if previous.healthyWithin(held) == nil {
			t.Fatalf("the %s served while this build's PATCH it waits for was still under way", builds.previousName())
		}
		if pid, _ := gateWaiter(t, builds.database, patchPID); pid != gatePID {
			t.Fatalf("after %s the %s's backend waiting for the gate is %d, want %d still waiting", held, builds.previousName(), pid, gatePID)
		}
		released := time.Now()
		if err := pause.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		previous.waitHealthy(t)
		t.Logf("the %s served %s after this build's PATCH was let go, %s after it started", builds.previousName(),
			time.Since(released).Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
		if answer := <-labelPrevious.send(); answer.status != http.StatusOK {
			t.Fatalf("the %s's PATCH answered %s", builds.previousName(), answer)
		}
	}
	select {
	case answer := <-patched:
		if answer.status != http.StatusOK {
			t.Fatalf("this build's PATCH answered %s", answer)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("this build's PATCH never answered within 60s of its update of the labels being let go")
	}
	var labels []string
	if err := builds.database.Pool.QueryRow(ctx, `select labels from issues where key = $1`, issue).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(labels, "previous-build") || !slices.Contains(labels, "current-build") {
		t.Errorf("the issue's labels are %v; want this build's and the %s's both: the label the %s added once it served was lost under this build's PATCH, which read the labels before it",
			labels, builds.previousName(), builds.previousName())
	}
	if answer := <-(crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["current-build-again"]}`}.send()); answer.status != http.StatusOK {
		t.Fatalf("this build's second PATCH answered %s", answer)
	}
	current.WaitForOutput(t, "owner locks now take the owner's row too")
}

// ───── the previous build's registration beside a write of this build held past its bound ─────

// registrationGateBound bounds an earlier build's registration's wait for the owner lock gate:
// owner_lock_legacy_registration's lock_timeout (migration 20261115200014_owner_lock_legacy_gate),
// spelled again here. registrationGateHold is how long the row below holds a write of this build
// in flight once that registration waits: longer than the bound.
const (
	registrationGateBound = 5 * time.Second
	registrationGateHold  = 8 * time.Second
)

// heartbeatWatch is what watchHeartbeat saw of one server process's row: each heartbeat_at it read,
// in order, the oldest the row was at a read by the database's clock, whether a read found no row,
// and the last read that failed.
type heartbeatWatch struct {
	beats  []time.Time
	oldest time.Duration
	gone   bool
	err    error
}

// gaps is the time between each two heartbeats w saw.
func (w heartbeatWatch) gaps() []time.Duration {
	gaps := make([]time.Duration, 0, len(w.beats))
	for i := 1; i < len(w.beats); i++ {
		gaps = append(gaps, w.beats[i].Sub(w.beats[i-1]).Round(time.Millisecond))
	}
	return gaps
}

// watchHeartbeat reads the server_processes row of instance every 100 ms until the stop it returns
// is called, which answers what it saw, or the test ends.
func watchHeartbeat(t *testing.T, database *store.Store, instance string) (stop func() heartbeatWatch) {
	done := make(chan struct{})
	seen := make(chan heartbeatWatch, 1)
	go func() {
		var watch heartbeatWatch
		for {
			var beat time.Time
			var age float64
			err := database.Pool.QueryRow(context.Background(), `
				select heartbeat_at, extract(epoch from clock_timestamp() - heartbeat_at)::float8
				from server_processes where instance = $1::uuid`, instance).Scan(&beat, &age)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				watch.gone = true
			case err != nil:
				watch.err = err
			default:
				if n := len(watch.beats); n == 0 || !watch.beats[n-1].Equal(beat) {
					watch.beats = append(watch.beats, beat)
				}
				watch.oldest = max(watch.oldest, time.Duration(age*float64(time.Second)))
			}
			select {
			case <-done:
				seen <- watch
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	end := func() { once.Do(func() { close(done) }) }
	t.Cleanup(end)
	return func() heartbeatWatch {
		end()
		return <-seen
	}
}

// The previous build boots with no web app (DISPATCH_WEB_DIST an empty directory), so it publishes
// no dashboard and its registration is its arrival, while a write of this build is between its
// owner lock and its write phase for longer than the registration's bound: a PATCH adding a label,
// held as the boot row above holds it (pauseLabelWrites). The registration waits for the gate
// holding the registry's lock, which every heartbeat takes too, and is refused with 55P03 once
// registrationGateBound has passed; the previous build then exits, as its main does when its
// registration fails. This build's process does not lapse meanwhile: its row is never older than
// the live window (processes.LiveWindow), its heartbeat gaps logged. Once the write has committed
// with its label, the previous build, started again as its supervisor would, registers and
// serves, and the label it adds is kept beside this build's. Without the bound the registration
// waits for the write for as long as it is held, with every heartbeat of this build behind it.
func TestThePreviousBuildsRegistrationWaitsForTheGateNoLongerThanItsBound(t *testing.T) {
	builds := newTwoBuilds(t)
	current := builds.startNamed(t, false, currentApplication)
	project := uniqueProjectKey(t, "BND")
	current.createProject(t, project, "Registration bound")
	issue, _ := current.createIssueIn(t, project, "Labelled across a refused registration", "A spec.\n")
	ctx := context.Background()
	var instance string
	if err := builds.database.Pool.QueryRow(ctx, `select instance::text from server_processes where owner_lock_protocol = $1`,
		store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("read this build's registration: %v", err)
	}

	pause := pauseLabelWrites(t, builds.database, currentApplication)
	defer pause.Rollback(ctx)
	patched := crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["current-build"]}`}.send()
	patchPID, waitingOn := queuedBackend(t, builds.database, pause, currentApplication, currentLabelWrite, 20*time.Second)
	t.Logf("this build's PATCH waits in %q", waitingOn)
	stopWatch := watchHeartbeat(t, builds.database, instance)

	noDashboard := t.TempDir()
	startPrevious := func() *dispatchProcess {
		options := builds.options(nil)
		options.Binary, options.WebDist = builds.build.path("envoy-dispatch"), noDashboard
		builds.a = startDispatchProcessWith(t, builds.databaseURL(t)+"&application_name="+previousApplication, options)
		return builds.a
	}
	started := time.Now()
	previous := startPrevious()
	var gateQuery string
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var gatePID uint32
		if gatePID, gateQuery = gateWaiter(t, builds.database, patchPID); gatePID != 0 {
			break
		}
		select {
		case <-previous.Exited:
			t.Fatalf("the %s exited before its registration waited for the owner lock gate:\n%s", builds.previousName(), previous.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s's registration did not wait for the owner lock gate within 60s:\n%s", builds.previousName(), previous.Output.String())
		}
	}
	waiting := time.Now()
	t.Logf("the %s waits for the owner lock gate %s after it started, in %q", builds.previousName(),
		waiting.Sub(started).Round(time.Millisecond), gateQuery)
	if !strings.Contains(gateQuery, "insert into server_processes") {
		t.Errorf("the %s waits for the gate in %q, want its registration's insert: with no web app it publishes no dashboard", builds.previousName(), gateQuery)
	}
	refused := false
	select {
	case <-previous.Exited:
		refused = true
		took := time.Since(waiting)
		t.Logf("the %s exited %s after its registration began to wait for the gate", builds.previousName(), took.Round(time.Millisecond))
		if took > registrationGateBound+3*time.Second {
			t.Errorf("the %s exited %s after its registration began to wait for the gate, want within its bound of %s and the exit", builds.previousName(), took.Round(time.Millisecond), registrationGateBound)
		}
		if output := previous.Output.String(); !strings.Contains(output, `msg="dispatch: register this server process"`) || !strings.Contains(output, "SQLSTATE 55P03") {
			t.Errorf("the %s exited without logging its registration refused for the lock timeout (55P03):\n%s", builds.previousName(), output)
		}
	case <-time.After(registrationGateBound + 3*time.Second):
		t.Errorf("the %s's registration still waited for the gate %s after it began, past its bound of %s",
			builds.previousName(), time.Since(waiting).Round(time.Millisecond), registrationGateBound)
	}
	if rest := registrationGateHold - time.Since(waiting); rest > 0 {
		time.Sleep(rest)
	}
	if err := pause.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-patched:
		if answer.status != http.StatusOK {
			t.Fatalf("this build's PATCH answered %s", answer)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("this build's PATCH never answered within 60s of its update of the labels being let go")
	}
	watch := stopWatch()
	t.Logf("this build's heartbeats from the PATCH's hold to its commit, %s after the %s's registration began to wait: %d read, gaps %v, the row at its oldest %s",
		time.Since(waiting).Round(time.Millisecond), builds.previousName(), len(watch.beats), watch.gaps(), watch.oldest.Round(time.Millisecond))
	if watch.err != nil {
		t.Errorf("read this build's heartbeat: %v", watch.err)
	}
	if watch.gone || watch.oldest >= processes.LiveWindow {
		t.Errorf("this build's registration lapsed while the %s's registration waited: its row was %s old at its oldest (gone: %t), want under the live window %s",
			builds.previousName(), watch.oldest.Round(time.Millisecond), watch.gone, processes.LiveWindow)
	}
	if refused {
		var earlier int
		if err := builds.database.Pool.QueryRow(ctx, `select count(*) from server_processes where owner_lock_protocol = 0`).Scan(&earlier); err != nil {
			t.Fatal(err)
		}
		if earlier != 0 {
			t.Errorf("%d rows of an earlier build are registered after the %s's registration was refused, want none", earlier, builds.previousName())
		}
		previous = startPrevious()
	}
	previous.waitHealthy(t)
	if answer := <-(crossBuildWrite{process: previous, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["previous-build"]}`}.send()); answer.status != http.StatusOK {
		t.Fatalf("the %s's PATCH answered %s", builds.previousName(), answer)
	}
	var labels []string
	if err := builds.database.Pool.QueryRow(ctx, `select labels from issues where key = $1`, issue).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(labels, "previous-build") || !slices.Contains(labels, "current-build") {
		t.Errorf("the issue's labels are %v; want this build's and the %s's both", labels, builds.previousName())
	}
	if answer := <-(crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["current-build-again"]}`}.send()); answer.status != http.StatusOK {
		t.Fatalf("this build's second PATCH answered %s", answer)
	}
	current.WaitForOutput(t, "owner locks now take the owner's row too")
}
