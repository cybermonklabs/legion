//go:build twobuild

package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
		options.BindsItsPort = true
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
