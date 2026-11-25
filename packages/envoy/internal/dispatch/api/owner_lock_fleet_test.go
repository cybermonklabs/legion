package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A writer of this build beside a writer of an earlier build. That build serialises an owner's
// writers on the owner's row - its close takes the issue's row `for no key update` first and holds
// it until it commits - and never takes the owner's advisory lock (store.OwnerLockProtocol), so
// while it is in the fleet a route of this build takes the row too, in its read phase, before it
// reads what its write decides on (store.LockOwner). Each row here registers such a process, holds
// the issue's row on the test's own connection as that build's writer holds it, sends a request of
// this build, lets that writer's write land and commits it: the request must decide on what it
// wrote. The control registers nothing: the request then reads in its read phase without waiting,
// and meets the row only in its write phase.

// registerEarlierBuildProcess writes a server_processes row as a process of a build before the
// owner lock protocol leaves it: no owner_lock_protocol, so the column's default 0, which keeps
// that build in the fleet for store.OwnerLockLegacyWindow, however slow the run.
func registerEarlierBuildProcess(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into server_processes (instance, commit, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '{fingerprint}')`); err != nil {
		t.Fatalf("register a process of an earlier build: %v", err)
	}
}

// newFleetRaceFixture is newItemRowFixture's server and issue, with fleet's process of an earlier
// build, when it has one, registered before the issue is filed. That registration's insert takes
// the owner lock gate and waits, at most 5 s, for every transaction of this build holding it, and
// before the issue there is no document whose settlement could hold it.
func newFleetRaceFixture(t *testing.T, fleet fleetRaceCase, title string) itemRowFixture {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	if fleet.earlier {
		registerEarlierBuildProcess(t, database)
	}
	issue := createInteractionIssue(t, handler, "LOCK", title, "A spec.\n")
	return itemRowFixture{handler: handler, database: database, deps: deps, issueKey: issue.Key, artifactID: issue.PrimaryArtifactID}
}

// readPhaseRowLock matches the statement store.LockOwner takes an issue's row with beside its
// advisory lock (store.ownerLockExclusive's row lock), and no statement of the write phase;
// readPhaseLock matches it or the advisory lock's own statement (owner_lock), which a request
// queues on when a background settlement of the same issue, itself queued on the row, holds the
// advisory lock.
var (
	readPhaseRowLock = regexp.MustCompile(`from issues where key = any\(\$1::text\[\]\) order by key collate "C" for no key update`)
	readPhaseLock    = regexp.MustCompile(readPhaseRowLock.String() + `|as owner_lock`)
)

// earlierBuildRace is one race of a request of this build against a writer of an earlier build.
type earlierBuildRace struct {
	// send runs the request on handler.
	send func(handler http.Handler) *httptest.ResponseRecorder
	// meanwhile is the earlier build's write, run on holder, which holds the issue's row.
	meanwhile func(t *testing.T, holder pgx.Tx)
}

// earlierBuildOutcome is what a race came to: the request's answer, and the statement it was
// waiting in once it had queued behind the row.
type earlierBuildOutcome struct {
	answer    *httptest.ResponseRecorder
	waitingOn string
}

// waitedReadPhase reports whether the request waited in its read phase: on the owner's row taken
// beside the advisory lock, or on the advisory lock itself.
func (o earlierBuildOutcome) waitedReadPhase() bool { return readPhaseLock.MatchString(o.waitingOn) }

// rowLockedReadPhase reports whether the request waited on the owner's row in its read phase.
func (o earlierBuildOutcome) rowLockedReadPhase() bool {
	return readPhaseRowLock.MatchString(o.waitingOn)
}

// runEarlierBuildRace holds fixture's issue row `for no key update` in a transaction of the
// test's own, as a writer of an earlier build holds it from its first statement, sends race's
// request on a handler of one connection, waits for that connection to queue behind the row, runs
// race's write on the holder and commits it, and answers what the request then answered.
func runEarlierBuildRace(t *testing.T, fixture itemRowFixture, race earlierBuildRace) earlierBuildOutcome {
	t.Helper()
	ctx := context.Background()
	handler, pid := oneConnectionHandler(t, fixture.deps)
	holder, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select 1 from issues where key = $1 for no key update`, fixture.issueKey); err != nil {
		t.Fatalf("hold the issue's row as the earlier build's writer: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- race.send(handler) }()
	if answer, early := storetest.WaitForQueuedBehind(t, holder, pid, answered); early {
		t.Fatalf("the request answered status=%d body=%s while the earlier build's writer held the issue's row; want it to wait for that row",
			answer.Code, answer.Body.String())
	}
	var waitingOn string
	if err := fixture.database.Pool.QueryRow(ctx, `select query from pg_stat_activity where pid = $1`, pid).Scan(&waitingOn); err != nil {
		t.Fatal(err)
	}
	race.meanwhile(t, holder)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the earlier build's write: %v", err)
	}
	answer := testwait.Receive(t, "the request's answer once the earlier build's write committed", answered)
	return earlierBuildOutcome{answer: answer, waitingOn: waitingOn}
}

// fleetRaceCase is the registry a race runs beside.
type fleetRaceCase struct {
	name    string
	earlier bool
}

var fleetRaceCases = []fleetRaceCase{
	{name: "beside a live process of an earlier build", earlier: true},
	{name: "with no process of an earlier build registered"},
}

// A comment's resolution is refused on an issue an earlier build's writer closes while the
// resolution is under way (a closed issue still takes a new comment, but not a change to one:
// lockedComment): that writer holds the issue's row when the resolve of this build starts, closes
// the issue and commits, and the resolve, which waited on the row in its read phase, reads the
// issue closed and answers 409 ISSUE_CLOSED. That build's close never takes the advisory lock the
// resolve takes first, so without the row the resolve would read the issue open, write in its
// write phase once the close committed, and resolve a comment on a closed issue: a missed
// refusal. With no such process registered the resolve takes no row in its read phase (it meets
// the row only in its write phase), which is what the control row checks; what it answers then is
// logged, not judged.
func TestACommentResolveIsRefusedOnAnIssueAnEarlierBuildClosesWhileItIsUnderWay(t *testing.T) {
	t.Parallel()
	for _, fleet := range fleetRaceCases {
		t.Run(fleet.name, func(t *testing.T) {
			fixture := newFleetRaceFixture(t, fleet, "Closed meanwhile")
			comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{"body": "Resolve this."}, "alice")
			outcome := runEarlierBuildRace(t, fixture, earlierBuildRace{
				send: func(handler http.Handler) *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice")
				},
				meanwhile: func(t *testing.T, holder pgx.Tx) {
					if _, err := holder.Exec(context.Background(),
						`update issues set status = 'done', closed_at = now() where key = $1`, fixture.issueKey); err != nil {
						t.Fatalf("close the issue as the earlier build: %v", err)
					}
				},
			})
			resolved := fixture.commentResolved(t, comment.ID)
			if !fleet.earlier {
				if outcome.rowLockedReadPhase() {
					t.Errorf("with no earlier build registered the resolve waited on the issue's row in its read phase (%s); want it to take no row there", outcome.waitingOn)
				}
				t.Logf("with no earlier build registered the resolve answered status=%d and the comment is resolved: %t; the advisory lock alone does not exclude a writer that holds the row",
					outcome.answer.Code, resolved)
				return
			}
			if !outcome.waitedReadPhase() {
				t.Errorf("the resolve waited on %q, want its read phase: the issue's row beside the advisory lock (store.LockOwner beside an earlier build), or that lock", outcome.waitingOn)
			}
			if code, _ := refusalOf(t, outcome.answer); outcome.answer.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
				t.Errorf("the resolve answered status=%d body=%s on the issue the earlier build closed meanwhile; want 409 ISSUE_CLOSED",
					outcome.answer.Code, outcome.answer.Body.String())
			}
			if resolved {
				t.Error("the comment is resolved on the issue the earlier build closed meanwhile; want it left as it was")
			}
		})
	}
}

// labels_add adds to the labels the issue holds when the write commits: a PATCH of this build that
// starts while an earlier build's writer holds the issue's row, and adds its label once that writer
// has added one of its own and committed, leaves both. That writer never takes the advisory lock
// the PATCH takes first, so without the row the PATCH would read the labels before that writer's
// commit and write its own over them: the earlier build's label lost.
func TestALabelAddedBesideAnEarlierBuildsLabelKeepsBoth(t *testing.T) {
	t.Parallel()
	for _, fleet := range fleetRaceCases {
		t.Run(fleet.name, func(t *testing.T) {
			fixture := newFleetRaceFixture(t, fleet, "Labelled meanwhile")
			outcome := runEarlierBuildRace(t, fixture, earlierBuildRace{
				send: func(handler http.Handler) *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
						map[string]any{"labels_add": []string{"this-build"}}, "alice")
				},
				meanwhile: func(t *testing.T, holder pgx.Tx) {
					if _, err := holder.Exec(context.Background(),
						`update issues set labels = array_append(labels, 'earlier-build') where key = $1`, fixture.issueKey); err != nil {
						t.Fatalf("add the earlier build's label: %v", err)
					}
				},
			})
			labels := issueLabels(t, fixture.database, fixture.issueKey)
			if outcome.answer.Code != http.StatusOK {
				t.Fatalf("the PATCH answered status=%d body=%s", outcome.answer.Code, outcome.answer.Body.String())
			}
			if !fleet.earlier {
				if outcome.rowLockedReadPhase() {
					t.Errorf("with no earlier build registered the PATCH waited on the issue's row in its read phase (%s); want it to take no row there", outcome.waitingOn)
				}
				t.Logf("with no earlier build registered the issue's labels are %v", labels)
				return
			}
			if !outcome.waitedReadPhase() {
				t.Errorf("the PATCH waited on %q, want its read phase: the issue's row beside the advisory lock (store.LockOwner beside an earlier build), or that lock", outcome.waitingOn)
			}
			if !slices.Contains(labels, "earlier-build") || !slices.Contains(labels, "this-build") {
				t.Errorf("the issue's labels are %v, want both the earlier build's and this build's: an update was lost", labels)
			}
		})
	}
}

// issueLabels is the issue key's labels as stored.
func issueLabels(t *testing.T, database *store.Store, key string) []string {
	t.Helper()
	var labels []string
	if err := database.Pool.QueryRow(context.Background(), `select labels from issues where key = $1`, key).Scan(&labels); err != nil {
		t.Fatal(fmt.Errorf("read the issue's labels: %w", err))
	}
	return labels
}

// earlierBuildArrivals are the two inserts with which an earlier build arrives in the fleet, as
// its boot sends them: its dashboard publish, the first thing it writes, and its registration.
// Neither names owner_lock_protocol, so each takes the owner lock gate exclusively and stamps the
// earlier build's mark (migration 20261115200014_owner_lock_legacy_gate).
var earlierBuildArrivals = []struct {
	name, insert string
}{
	{"its dashboard publish", `
		insert into dashboard_builds (build, entry, commit) values (gen_random_uuid()::text, '/assets/index-earlier.js', 'earlier0build')
		on conflict (build) do update set last_started_at = now()`},
	{"its registration", `
		insert into server_processes (instance, commit, dashboard_build, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '', '{fingerprint}')
		on conflict (instance) do update set heartbeat_at = now()`},
}

// pauseLabelWrite holds backend pid's next write of an issue's labels as that statement opens,
// before it reaches a row, until the transaction it returns ends: a trigger has each UPDATE
// naming issues.labels on that backend take a shared hold on a key the returned transaction
// holds. A writer of the issue's row on any other backend passes through.
func pauseLabelWrite(t *testing.T, database *store.Store, pid uint32) pgx.Tx {
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
			when (pg_backend_pid() = %d) execute function dispatch_test_hold_labels('%d')`, pid, holdLabels),
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the label write of backend %d: %v", pid, err)
		}
	}
	pause, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pause.Exec(ctx, `select pg_advisory_xact_lock($1)`, holdLabels); err != nil {
		_ = pause.Rollback(ctx)
		t.Fatalf("take the hold the label write waits on: %v", err)
	}
	return pause
}

// An earlier build that arrives while a write of this build is between its owner lock and its
// write phase waits for that write to commit, and a write the earlier build makes once it has
// arrived lands after it and is kept. The write of this build is a PATCH adding a label, sent with
// no earlier build in the fleet, so it reads the labels under its owner lock alone and holds the
// owner lock gate shared; the test pauses it as its write phase opens, at its update of the
// labels and before that update reaches the issue's row (pauseLabelWrite), so no writer of the
// issue's row waits on the pause. The earlier build then arrives on a connection of its own - its
// dashboard publish, or its registration - and, once arrived, adds its own label as its writers
// do, the issue's row first. The arrival waits on the gate until the PATCH commits, so the earlier
// build's label is added to the PATCH's. Without the gate the arrival goes through at once, the
// earlier build's label is written while the PATCH waits, and the PATCH then writes the labels it
// read over it. A write of this build after the arrival takes the issue's row in its read phase.
// The race starts once the seeded spec owes no settlement and no backend holds the gate
// (awaitTheGateFree), so the arrival's wait for the gate is the PATCH's alone, not a settlement
// of the spec the fixture seeded.
//
// The registration's wait for the gate is bounded at 5 seconds (owner_lock_legacy_registration's
// lock_timeout, migration 20261115200014_owner_lock_legacy_gate), and that bound is a row of its
// own (store's TestAnEarlierBuildsRegistrationWaitsForTheGateNoLongerThanItsBound). Here the
// subject is the order: the arrival waits on the gate for as long as the PATCH holds it, however
// long the test takes to observe that wait and let the PATCH go. So this database's copy of the
// function waits without a bound, and no wait of this row ends on a clock: each ends on the fact
// it waits for, bounded only by the test binary's deadline (testwait).
func TestAnEarlierBuildsWriteAfterItArrivesBesideAWriteInFlightIsKept(t *testing.T) {
	t.Parallel()
	for _, arrival := range earlierBuildArrivals {
		t.Run(arrival.name, func(t *testing.T) {
			fixture := newItemRowFixture(t, testServerOptions{}, "Arrived meanwhile", "A spec.\n")
			awaitTheGateFree(t, fixture.database, fixture.artifactID)
			ctx := context.Background()
			if _, err := fixture.database.Pool.Exec(ctx, `alter function public.owner_lock_legacy_registration() set lock_timeout = 0`); err != nil {
				t.Fatalf("lift the registration's bound on its wait for the gate in this test's database: %v", err)
			}
			handler, pid := oneConnectionHandler(t, fixture.deps)
			pause := pauseLabelWrite(t, fixture.database, pid)
			defer pause.Rollback(ctx)
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				answered <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
					map[string]any{"labels_add": []string{"this-build"}}, "alice")
			}()
			if answer, early := storetest.WaitForQueuedBehind(t, pause, pid, answered); early {
				t.Fatalf("the PATCH answered status=%d body=%s before it reached its write phase; want it to wait at its update of the labels there",
					answer.Code, answer.Body.String())
			}
			if mode, granted := gateHeldBy(t, fixture.database, pid); mode != "ShareLock" || !granted {
				t.Fatalf("the PATCH, with no earlier build in the fleet, holds the owner lock gate as %q (granted %t); want it held shared", mode, granted)
			}

			arrivalConn, err := fixture.database.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer arrivalConn.Release()
			arrivalPID := arrivalConn.Conn().PgConn().PID()
			arrived := make(chan error, 1)
			began := time.Now()
			go func() {
				_, err := arrivalConn.Exec(ctx, arrival.insert)
				arrived <- err
			}()
			// The arrival either goes through, which it must not, or waits on the PATCH's backend.
			type arrivalSeen struct {
				through bool
				err     error
			}
			seen := testwait.UntilPolling(t, 5*time.Millisecond, arrival.name+" to go through or wait for the PATCH",
				func() (arrivalSeen, bool) {
					select {
					case err := <-arrived:
						return arrivalSeen{through: true, err: err}, true
					default:
					}
					var blocked bool
					if err := fixture.database.Pool.QueryRow(ctx, `select $2::integer = any(pg_blocking_pids($1))`, arrivalPID, pid).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					return arrivalSeen{}, blocked
				})
			if seen.err != nil {
				t.Fatalf("%s: %v", arrival.name, seen.err)
			}
			if seen.through {
				t.Errorf("%s went through in %s while this build's PATCH, which read no earlier build in the fleet, was between its owner lock and its write; want it to wait for that PATCH (the owner lock gate)",
					arrival.name, time.Since(began).Round(time.Millisecond))
			}
			waited := !seen.through
			earlierWrite := func() {
				tx, err := fixture.database.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err := tx.Exec(ctx, `select 1 from issues where key = $1 for no key update`, fixture.issueKey); err != nil {
					t.Fatalf("take the issue's row as the earlier build's writer: %v", err)
				}
				if _, err := tx.Exec(ctx, `update issues set labels = array_append(labels, 'earlier-build') where key = $1`, fixture.issueKey); err != nil {
					t.Fatalf("add the earlier build's label: %v", err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("commit the earlier build's label: %v", err)
				}
			}
			if waited {
				// The arrival is still waiting on the PATCH as the PATCH is let go: it has not
				// ended, and the PATCH's backend still blocks it.
				select {
				case err := <-arrived:
					t.Fatalf("%s ended (%v) while the PATCH it waited for was still under way", arrival.name, err)
				default:
				}
				if err := fixture.database.Pool.QueryRow(ctx, `select $2::integer = any(pg_blocking_pids($1))`, arrivalPID, pid).Scan(&waited); err != nil {
					t.Fatal(err)
				}
				if !waited {
					t.Fatalf("%s stopped waiting on the PATCH while the PATCH was still under way", arrival.name)
				}
				if err := pause.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if err := testwait.Receive(t, arrival.name+" to go through once the PATCH's write phase was let go", arrived); err != nil {
					t.Fatalf("%s: %v", arrival.name, err)
				}
				t.Logf("%s waited %s, until the PATCH committed", arrival.name, time.Since(began).Round(time.Millisecond))
				earlierWrite()
			} else {
				earlierWrite()
				if err := pause.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if answer := testwait.Receive(t, "the PATCH's answer once its update of the labels was let go", answered); answer.Code != http.StatusOK {
				t.Fatalf("the PATCH answered status=%d body=%s", answer.Code, answer.Body.String())
			}
			if labels := issueLabels(t, fixture.database, fixture.issueKey); !slices.Contains(labels, "earlier-build") || !slices.Contains(labels, "this-build") {
				t.Errorf("the issue's labels are %v, want both this build's and the earlier build's: the label the earlier build added once it had arrived was lost under the PATCH that read the labels before it", labels)
			}

			outcome := runEarlierBuildRace(t, fixture, earlierBuildRace{
				send: func(handler http.Handler) *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
						map[string]any{"labels_add": []string{"this-build-again"}}, "alice")
				},
				meanwhile: func(t *testing.T, holder pgx.Tx) {
					if _, err := holder.Exec(ctx, `update issues set labels = array_append(labels, 'earlier-build-again') where key = $1`, fixture.issueKey); err != nil {
						t.Fatalf("add the earlier build's second label: %v", err)
					}
				},
			})
			if !outcome.rowLockedReadPhase() {
				t.Errorf("the PATCH after %s waited on %q, want the issue's row in its read phase (store.LockOwner beside an earlier build)", arrival.name, outcome.waitingOn)
			}
			if labels := issueLabels(t, fixture.database, fixture.issueKey); !slices.Contains(labels, "earlier-build-again") || !slices.Contains(labels, "this-build-again") {
				t.Errorf("the issue's labels after the second race are %v, want both second labels", labels)
			}
		})
	}
}

// awaitTheGateFree waits until document owes no settlement and no backend holds or waits for the
// owner lock gate: an earlier build's arrival made then waits on the gate for nothing the test did
// not start itself. Its one bound is the test binary's deadline (testwait.UntilPolling), which
// names what was still owed or held if it comes first.
func awaitTheGateFree(t *testing.T, database *store.Store, document string) {
	t.Helper()
	testwait.UntilPolling(t, 10*time.Millisecond, "the seeded document to owe no settlement and the owner lock gate to be free",
		func() (string, bool) {
			var owed, held bool
			if err := database.Pool.QueryRow(context.Background(), `
				select exists (select from doc_settlements_pending where artifact_id = $1::uuid),
				       exists (select from pg_locks
				               where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
				                 and classid::bigint = $2 and objid::bigint = $3 and objsubid = 2)`,
				document, store.OwnerLockGateNamespace, store.OwnerLockGateKey).Scan(&owed, &held); err != nil {
				t.Fatalf("read the document's settlement and the owner lock gate: %v", err)
			}
			return fmt.Sprintf("settlement owed %t, gate held or waited for %t", owed, held), !owed && !held
		})
}

// gateHeldBy is the owner lock gate as backend pid holds or waits for it, by pg_locks: its mode
// ("ShareLock", "ExclusiveLock"; "" when pid neither holds nor waits for it) and whether it is
// granted.
func gateHeldBy(t *testing.T, database *store.Store, pid uint32) (mode string, granted bool) {
	t.Helper()
	err := database.Pool.QueryRow(context.Background(), `
		select mode, granted from pg_locks
		where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
		  and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3`,
		store.OwnerLockGateNamespace, store.OwnerLockGateKey, pid).Scan(&mode, &granted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the owner lock gate of backend %d: %v", pid, err)
	}
	return mode, granted
}
