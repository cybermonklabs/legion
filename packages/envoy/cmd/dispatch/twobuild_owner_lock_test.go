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

// A build before the owner lock (server_processes.owner_lock_protocol 0) serialises an owner's
// writers on the owner's row: each takes the row `for no key update` first and holds it to its
// commit. This tree (store.OwnerLockProtocol) serialises them on the owner's advisory lock, which
// such a build never takes, and takes the row too while a process of such a build is live
// (store.LockOwner). The previous build is either kind: dispatch-two-builds pairs this tree with
// the newest release, a build before the owner lock until a release carries it, and
// dispatch-two-builds-same pairs this tree with its own build. Each row below reads which kind from
// the protocol the previous build registers (ownerLockPairingOf), never from a job's name, a
// variable or the build's commit, and runs the case it states for that pairing.
//
// The first row runs a process of each build on one database and puts one write of each on one
// issue at once, the previous build's first: the test holds, on its own connection, the lock the
// previous build's writers take first (the issue's row beside a build before the owner lock, the
// issue's owner lock beside a build at this tree's protocol), sends the previous build's write,
// which queues on that lock, then this build's, and lets the lock go. The previous build's write
// then commits first, and this build's must decide on what it wrote. Each direction is one way a
// write of this build that read the issue before that commit goes wrong: this build's write
// accepted where the previous build's made it a refusal, and the previous build's write lost under
// this build's. The two rows after it test the owner lock gate, which only a build before the
// owner lock takes, as it arrives: they run beside such a build and skip beside a build at this
// tree's protocol (gateRowSkip).

// ownerLockPairing is the case the owner lock rows run, by the owner lock protocol the previous
// build registers (ownerLockPairingOf).
type ownerLockPairing int

const (
	// earlierBuildPairing is a previous build before the owner lock, at protocol 0: its writers take
	// the owner's row first and never the owner lock, its arrival takes the owner lock gate, and
	// this build's writes take the owner's row too while it is live.
	earlierBuildPairing ownerLockPairing = iota + 1
	// sameProtocolPairing is a previous build at this tree's protocol (store.OwnerLockProtocol): its
	// writers take the owner lock first, as this build's do, and it is no earlier build to this one,
	// so neither build takes the owner's row before its write phase.
	sameProtocolPairing
)

func (p ownerLockPairing) String() string {
	switch p {
	case earlierBuildPairing:
		return "a build before the owner lock"
	case sameProtocolPairing:
		return "a build at this tree's owner lock protocol"
	}
	return fmt.Sprintf("ownerLockPairing(%d)", int(p))
}

// ownerLockPairingOf is the pairing of a previous build that registers owner lock protocol
// protocol: 0 is a build before the owner lock (earlierBuildPairing), store.OwnerLockProtocol a
// build at this tree's (sameProtocolPairing). No row states a case for any other protocol, so for
// one it answers an error, which fails the row rather than skipping it.
func ownerLockPairingOf(protocol int) (ownerLockPairing, error) {
	switch protocol {
	case 0:
		return earlierBuildPairing, nil
	case store.OwnerLockProtocol:
		return sameProtocolPairing, nil
	}
	return 0, fmt.Errorf("the previous build registers owner lock protocol %d, and the owner lock rows state a case only for 0, a build before the owner lock, and %d, this tree's (store.OwnerLockProtocol)",
		protocol, store.OwnerLockProtocol)
}

// gateRowSkip answers whether a row of the owner lock gate skips in pairing, and the reason it
// skips with. Only an earlier build's arrival takes the gate (migration
// 20261115200014_owner_lock_legacy_gate), so the gate rows run beside a build before the owner lock
// and skip beside a build at this tree's protocol.
func gateRowSkip(pairing ownerLockPairing) (reason string, skip bool) {
	if pairing != sameProtocolPairing {
		return "", false
	}
	return fmt.Sprintf("The previous build registers owner lock protocol %d, this tree's (store.OwnerLockProtocol). It takes the owner lock as this build does, so its arrival never takes the owner lock gate this row tests. TestTwoBuildsSerialiseOneOwnersWritersBothWays is the row that covers this pairing.",
		store.OwnerLockProtocol), true
}

// skipGateRowBeside skips a row of the owner lock gate when gateRowSkip skips it beside build, by
// the protocol build registers when it boots alone (previousBuildOwnerLockProtocol), and fails the
// row when no row states a case for that protocol (ownerLockPairingOf).
func skipGateRowBeside(t *testing.T, build previousBuild) {
	t.Helper()
	pairing, err := ownerLockPairingOf(previousBuildOwnerLockProtocol(t, build))
	if err != nil {
		t.Fatal(err)
	}
	if reason, skip := gateRowSkip(pairing); skip {
		t.Skip(reason)
	}
}

// The owner lock rows decide their case from the protocol the previous build reports as it
// registers, and from nothing else: a report of 0 is a build before the owner lock, and the gate
// rows run; a report of this tree's protocol is a build at it, and the gate rows skip, naming the
// row that covers that pairing; any other report fails the row that read it. A decision that
// skipped the gate rows at 0 would leave dispatch-two-builds, the job that pairs this tree with a
// build before the owner lock, passing with the gate untested. The row runs the decision alone,
// with no build and no database.
func TestTheOwnerLockRowsDecideTheirCaseByThePreviousBuildsProtocol(t *testing.T) {
	for _, report := range []struct {
		protocol int
		pairing  ownerLockPairing
		skip     bool
	}{
		{protocol: 0, pairing: earlierBuildPairing, skip: false},
		{protocol: store.OwnerLockProtocol, pairing: sameProtocolPairing, skip: true},
	} {
		pairing, err := ownerLockPairingOf(report.protocol)
		if err != nil {
			t.Errorf("a previous build at owner lock protocol %d fails its row: %v; want %s", report.protocol, err, report.pairing)
			continue
		}
		if pairing != report.pairing {
			t.Errorf("a previous build at owner lock protocol %d is %s, want %s", report.protocol, pairing, report.pairing)
		}
		switch reason, skip := gateRowSkip(pairing); {
		case skip && !report.skip:
			t.Errorf("the gate rows skip beside a previous build at owner lock protocol %d, whose arrival takes the gate; want them run. They skip saying: %s", report.protocol, reason)
		case !skip && report.skip:
			t.Errorf("the gate rows run beside a previous build at owner lock protocol %d, whose arrival never takes the gate; want them skipped", report.protocol)
		case skip && !strings.Contains(reason, "TestTwoBuildsSerialiseOneOwnersWritersBothWays"):
			t.Errorf("the gate rows skip beside a previous build at owner lock protocol %d saying %q, which names no row that covers the pairing", report.protocol, reason)
		}
	}
	if pairing, err := ownerLockPairingOf(store.OwnerLockProtocol + 1); err == nil {
		t.Errorf("a previous build at owner lock protocol %d, for which no row states a case, is %s; want an error that fails the row", store.OwnerLockProtocol+1, pairing)
	}
}

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

// ownerLockStatement matches a statement of a build at this tree's protocol that takes an owner
// lock: a read phase's call of public.owner_lock, or a write phase's pg_advisory_xact_lock (store's
// ownersLock and ownerLock).
var ownerLockStatement = regexp.MustCompile(`pg_advisory_xact_lock|public\.owner_lock\(`)

// rowModeLine is what a process of this tree logs once its owner locks take the owner's row too,
// beside an earlier build in the fleet (store's noteOwnerLockMode).
const rowModeLine = "owner locks now take the owner's row too"

// startNamed starts a process of the previous build (previous) or of this tree, its connections
// named application, and returns once it answers /healthz.
func (b *twoBuilds) startNamed(t *testing.T, previous bool, application string) *dispatchProcess {
	t.Helper()
	options := b.options(nil, nil)
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
			t.Fatalf("no backend of %s running a statement matching %s queued behind the lock the test holds within %s; its backends waiting on a lock: %+v",
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

// send sends w on another goroutine and delivers its answer. Nothing bounds the request
// (trySend), so a row receives the answer with answerWithin.
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

// answerBound is how long a row waits for a write's answer: far longer than any write takes once
// nothing the test holds keeps it waiting, and far shorter than the suite's -timeout of 30
// minutes, so a write that never answers fails its row rather than hanging the test binary.
const answerBound = 60 * time.Second

// answerWithin waits up to within for the answer on answered, and fails the test naming what, the
// write that never answered, when within passes first. The failure ends the test, so its deferred
// rollbacks let go of what the test holds and its cleanups stop each process.
func answerWithin(t *testing.T, answered <-chan httpAnswer, within time.Duration, what string) httpAnswer {
	t.Helper()
	select {
	case answer := <-answered:
		return answer
	case <-time.After(within):
		t.Fatalf("%s never answered within %s", what, within)
		return httpAnswer{}
	}
}

// awaitedLockType is the type of the lock the backend pid waits on (pg_locks.locktype): advisory
// for an owner lock, tuple or transactionid for a row another transaction holds.
func awaitedLockType(t *testing.T, database *store.Store, pid uint32) string {
	t.Helper()
	var lockType string
	if err := database.Pool.QueryRow(context.Background(), `select locktype from pg_locks where pid = $1 and not granted`, pid).Scan(&lockType); err != nil {
		t.Fatalf("read the lock backend %d waits on: %v", pid, err)
	}
	return lockType
}

// raceAcrossBuilds holds, on the test's own connection, the lock on issue that the previous
// build's writers take first in pairing: the issue's row beside a build before the owner lock, the
// issue's owner lock beside a build at this tree's protocol. It sends first (the previous build's
// write) and waits for it to queue on that lock in its first statement, sends second (this
// build's) and waits for it to queue behind the lock too, then lets the lock go, and answers both
// answers and the statement this build's write waited in. Beside a build at this tree's protocol
// it fails the test when either write waits on a lock other than an advisory one: with no earlier
// build in the fleet, neither build takes the issue's row before its write phase.
func raceAcrossBuilds(t *testing.T, builds *twoBuilds, pairing ownerLockPairing, issue string, first, second crossBuildWrite) (httpAnswer, httpAnswer, string) {
	t.Helper()
	ctx := context.Background()
	holder, err := builds.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	firstStatement, secondStatement := regexp.MustCompile(`^`+regexp.QuoteMeta(previousRowLock)+`$`), currentIssueStatement
	if pairing == sameProtocolPairing {
		if err := store.LockOwner(ctx, holder, store.IssueOwner(issue)); err != nil {
			t.Fatalf("hold the issue's owner lock: %v", err)
		}
		firstStatement, secondStatement = ownerLockStatement, ownerLockStatement
	} else if _, err := holder.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue); err != nil {
		t.Fatalf("hold the issue's row: %v", err)
	}
	firstAnswered := first.send()
	firstPID, _ := queuedBackend(t, builds.database, holder, previousApplication, firstStatement, 20*time.Second)
	secondAnswered := second.send()
	secondPID, waitingOn := queuedBackend(t, builds.database, holder, currentApplication, secondStatement, 20*time.Second)
	if pairing == sameProtocolPairing {
		for _, queued := range []struct {
			pid  uint32
			name string
		}{{firstPID, builds.previousName()}, {secondPID, builds.currentName()}} {
			if lockType := awaitedLockType(t, builds.database, queued.pid); lockType != "advisory" {
				t.Errorf("the %s's write waits on a %s lock behind the issue's owner lock the test holds, want an advisory lock: beside a build at this tree's protocol no earlier build is in the fleet, and neither build takes the issue's row before its write phase",
					queued.name, lockType)
			}
		}
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	return answerWithin(t, firstAnswered, answerBound, "the "+builds.previousName()+"'s write of the race, once the lock on the issue was let go,"),
		answerWithin(t, secondAnswered, answerBound, "this build's write of the race, once the lock on the issue was let go,"),
		waitingOn
}

// A write of each build on one issue at once, the previous build's committing first, decides as if
// the two had run one after the other: this build's PATCH conditional on the issue's sequence
// (expected_sequence) is refused 409 ISSUE_CHANGED once the previous build's PATCH has moved that
// sequence, and the previous build's label survives this build's labels_add. (A close would be
// the plainer refusal, but the previous build's close settles the issue's documents first, on the
// issue's row, before its own transaction, so the row the test holds would queue that settlement
// rather than the close.)
//
// The row runs in both pairings, and reads which one from the protocol the previous build's process
// registers once it serves, before this build's process starts, while its row is the registry's
// only one. Beside a build before the owner lock, which dispatch-two-builds runs until a release
// carries the owner lock, the test holds the issue's row, which that build's PATCH takes first;
// this build's process takes the row in its read phase while that build's process is live, and
// logs that its owner locks now do. Beside a build at this tree's protocol, which
// dispatch-two-builds-same runs, both builds' PATCHes take the issue's owner lock first, so the
// test holds that lock instead: each PATCH waits on it, an advisory lock, and neither on a row
// lock, and neither process logs that its owner locks take the row, since no earlier build is in
// the fleet.
func TestTwoBuildsSerialiseOneOwnersWritersBothWays(t *testing.T) {
	builds := newTwoBuilds(t)
	previous := builds.startNamed(t, true, previousApplication)
	pairing, err := ownerLockPairingOf(registeredOwnerLockProtocol(t, builds.database, previous, builds.previousName(), 30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the %s is %s", builds.previousName(), pairing)
	current := builds.startNamed(t, false, currentApplication)
	project := uniqueProjectKey(t, "OWN")
	current.createProject(t, project, "Owner locks")

	t.Run("this build's conditional PATCH beside the previous build's PATCH", func(t *testing.T) {
		issue, _ := current.createIssueIn(t, project, "Retitled by both builds", "A spec.\n")
		var sequence int64
		if err := builds.database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		const previousTitle, currentTitle = "Retitled by the previous build", "Retitled by this build"
		first, second, waitingOn := raceAcrossBuilds(t, builds, pairing, issue,
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
		first, second, waitingOn := raceAcrossBuilds(t, builds, pairing, issue,
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

	if pairing == earlierBuildPairing {
		current.WaitForOutput(t, rowModeLine)
		return
	}
	for _, process := range []*dispatchProcess{previous, current} {
		if output := process.Output.String(); strings.Contains(output, rowModeLine) {
			t.Errorf("the %s logged that its %s beside %s, which takes the owner lock as this build does and is no earlier build:\n%s",
				builds.buildName(process), rowModeLine, pairing, output)
		}
	}
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
//
// The row runs beside a build before the owner lock alone, which dispatch-two-builds pairs this
// tree with until a release carries the owner lock. It reads the pairing before the previous build
// boots on the row's database, from a boot of that build alone on a database of its own
// (previousBuildOwnerLockProtocol), since a boot on the row's database would be the arrival the row
// tests. Beside a build at this tree's protocol, which dispatch-two-builds-same pairs this tree
// with, it skips (gateRowSkip): that build takes the owner lock as this build does, so its arrival
// never takes the gate, and TestTwoBuildsSerialiseOneOwnersWritersBothWays covers that pairing.
func TestThePreviousBuildsBootWaitsForAWriteInFlightAndItsWriteIsKept(t *testing.T) {
	build := openPreviousBuild(t)
	skipGateRowBeside(t, build)
	builds := newTwoBuildsOf(t, build)
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

	options := builds.options(nil, nil)
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
		if answer := answerWithin(t, labelPrevious.send(), answerBound, "the "+builds.previousName()+"'s PATCH, sent while this build's PATCH was still paused,"); answer.status != http.StatusOK {
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
		if answer := answerWithin(t, labelPrevious.send(), answerBound, "the "+builds.previousName()+"'s PATCH"); answer.status != http.StatusOK {
			t.Fatalf("the %s's PATCH answered %s", builds.previousName(), answer)
		}
	}
	if answer := answerWithin(t, patched, answerBound, "this build's PATCH, once its update of the labels was let go,"); answer.status != http.StatusOK {
		t.Fatalf("this build's PATCH answered %s", answer)
	}
	var labels []string
	if err := builds.database.Pool.QueryRow(ctx, `select labels from issues where key = $1`, issue).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(labels, "previous-build") || !slices.Contains(labels, "current-build") {
		t.Errorf("the issue's labels are %v; want this build's and the %s's both: the label the %s added once it served was lost under this build's PATCH, which read the labels before it",
			labels, builds.previousName(), builds.previousName())
	}
	if answer := answerWithin(t, crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["current-build-again"]}`}.send(), answerBound, "this build's second PATCH"); answer.status != http.StatusOK {
		t.Fatalf("this build's second PATCH answered %s", answer)
	}
	current.WaitForOutput(t, rowModeLine)
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
// is called, which answers what it saw, or the test ends. The stop fails the test when the watch's
// read in flight has not returned within answerBound.
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
		select {
		case watch := <-seen:
			return watch
		case <-time.After(answerBound):
			t.Fatalf("the watch of server process %s's heartbeat had not stopped %s after it was told to", instance, answerBound)
			return heartbeatWatch{}
		}
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
//
// The row runs beside a build before the owner lock alone, reads the pairing as the boot row above
// does (previousBuildOwnerLockProtocol), and skips beside a build at this tree's protocol for that
// row's reason (gateRowSkip): such a build registers without taking the gate, and
// TestTwoBuildsSerialiseOneOwnersWritersBothWays covers that pairing.
func TestThePreviousBuildsRegistrationWaitsForTheGateNoLongerThanItsBound(t *testing.T) {
	build := openPreviousBuild(t)
	skipGateRowBeside(t, build)
	builds := newTwoBuildsOf(t, build)
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
		options := builds.options(nil, nil)
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
	if answer := answerWithin(t, patched, answerBound, "this build's PATCH, once its update of the labels was let go,"); answer.status != http.StatusOK {
		t.Fatalf("this build's PATCH answered %s", answer)
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
	if answer := answerWithin(t, crossBuildWrite{process: previous, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["previous-build"]}`}.send(), answerBound, "the "+builds.previousName()+"'s PATCH"); answer.status != http.StatusOK {
		t.Fatalf("the %s's PATCH answered %s", builds.previousName(), answer)
	}
	var labels []string
	if err := builds.database.Pool.QueryRow(ctx, `select labels from issues where key = $1`, issue).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(labels, "previous-build") || !slices.Contains(labels, "current-build") {
		t.Errorf("the issue's labels are %v; want this build's and the %s's both", labels, builds.previousName())
	}
	if answer := answerWithin(t, crossBuildWrite{process: current, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
		body: `{"labels_add":["current-build-again"]}`}.send(), answerBound, "this build's second PATCH"); answer.status != http.StatusOK {
		t.Fatalf("this build's second PATCH answered %s", answer)
	}
	current.WaitForOutput(t, rowModeLine)
}
