package api

import (
	"context"
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
)

// A writer of this build beside a writer of an earlier build. That build serialises an owner's
// writers on the owner's row - its close takes the issue's row `for no key update` first and holds
// it until it commits - and never takes the owner's advisory lock (store.OwnerLockProtocol), so
// while one of its processes is live a route of this build takes the row too, in its read phase,
// before it reads what its write decides on (store.LockOwner). Each row here registers such a
// process, holds the issue's row on the test's own connection as that build's writer holds it,
// sends a request of this build, lets that writer's write land and commits it: the request must
// decide on what it wrote. The control registers nothing: the request then reads in its read
// phase without waiting, and meets the row only in its write phase.

// registerEarlierBuildProcess writes a server_processes row as a live process of a build before
// the owner lock protocol leaves it: no owner_lock_protocol, so the column's default 0. Its
// heartbeat is the database's now(); stamp stamps it again, so a slow run keeps it live.
func registerEarlierBuildProcess(t *testing.T, database *store.Store) (stamp func()) {
	t.Helper()
	var instance string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, commit, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '{fingerprint}') returning instance::text`).Scan(&instance); err != nil {
		t.Fatalf("register a process of an earlier build: %v", err)
	}
	return func() {
		if _, err := database.Pool.Exec(context.Background(),
			`update server_processes set heartbeat_at = now() where instance = $1::uuid`, instance); err != nil {
			t.Fatalf("stamp the earlier build's heartbeat: %v", err)
		}
	}
}

// readPhaseRowLock matches the statement store.LockOwner takes an issue's row with beside its
// advisory lock (store.ownerRowLockStatement), and no statement of the write phase; readPhaseLock
// matches it or the advisory lock's own statement (owner_lock), which a request queues on when a
// background settlement of the same issue, itself queued on the row, holds the advisory lock.
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
	if answer, early := storetest.WaitForQueuedBehind(t, holder, pid, answered, 10*time.Second); early {
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
	var answer *httptest.ResponseRecorder
	select {
	case answer = <-answered:
	case <-time.After(itemRowOwnerLockFinish):
		t.Fatalf("the request never answered within %s of the earlier build's commit", itemRowOwnerLockFinish)
	}
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
	for _, fleet := range fleetRaceCases {
		t.Run(fleet.name, func(t *testing.T) {
			fixture := newItemRowFixture(t, testServerOptions{}, "Closed meanwhile", "A spec.\n")
			comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{"body": "Resolve this."}, "alice")
			stamp := func() {}
			if fleet.earlier {
				stamp = registerEarlierBuildProcess(t, fixture.database)
			}
			stamp()
			outcome := runEarlierBuildRace(t, fixture, earlierBuildRace{
				send: func(handler http.Handler) *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice")
				},
				meanwhile: func(t *testing.T, holder pgx.Tx) {
					stamp()
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
	for _, fleet := range fleetRaceCases {
		t.Run(fleet.name, func(t *testing.T) {
			fixture := newItemRowFixture(t, testServerOptions{}, "Labelled meanwhile", "A spec.\n")
			stamp := func() {}
			if fleet.earlier {
				stamp = registerEarlierBuildProcess(t, fixture.database)
			}
			stamp()
			outcome := runEarlierBuildRace(t, fixture, earlierBuildRace{
				send: func(handler http.Handler) *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
						map[string]any{"labels_add": []string{"this-build"}}, "alice")
				},
				meanwhile: func(t *testing.T, holder pgx.Tx) {
					stamp()
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
