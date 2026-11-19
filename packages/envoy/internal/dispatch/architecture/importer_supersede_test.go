package architecture

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/testwait"
)

// reSaveSource runs the settings PUT's own upsert (api/architecture_source.go's
// putArchitectureSource) for CORE's legion/arch source on its unchanged branch: the person saves
// the same repository and branch again, which clears last_sync_at, last_commit, last_tree_sha and
// last_error.
func reSaveSource(t *testing.T, process *Importer) {
	t.Helper()
	if _, err := process.store.Pool.Exec(context.Background(), reSave, reSaveArgs...); err != nil {
		t.Fatalf("re-save the source as the settings PUT does: %v", err)
	}
}

// beginReSave writes reSaveSource's save in a transaction it leaves open, for the test to commit:
// until then a load of the row reads it as it was before the save, and a write that takes the
// row's lock (lockSourceRow) waits for the commit, then finds the row written since that load.
// The transaction is rolled back when the test ends uncommitted.
func beginReSave(t *testing.T, process *Importer) pgx.Tx {
	t.Helper()
	tx, err := process.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the settings save: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), reSave, reSaveArgs...); err != nil {
		t.Fatalf("write the settings save: %v", err)
	}
	return tx
}

// reSave and reSaveArgs are the settings PUT's upsert of CORE's source on its unchanged branch.
const reSave = `
	insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
	values ($1, $2, $3, true, $4, $5)
	on conflict (project_key, repo) do update set
		branch = excluded.branch,
		enabled = true,
		installation_id = excluded.installation_id,
		created_by = excluded.created_by,
		last_sync_at = null,
		last_commit = null,
		last_tree_sha = null,
		last_error = null
`

var reSaveArgs = []any{"CORE", archRepo, "main", int64(11), `{"kind":"user","id":"alice"}`}

// A process loads a freshly saved source (last_sync_at null) and is held in its blob reads at the
// older head; another process imports the newer head; the person then re-saves the same repository
// and branch, which sets last_sync_at back to null. Once released, the held process must not
// record the older head it read before both of those writes: the row, the components and the
// events must stay at the newer head.
func TestASettingsReSaveDuringTwoSyncsNeverLeavesTheEarlierHead(t *testing.T) {
	branch := newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles(), commitTwo: infraFiles()})
	branch.holdBlobs(commitOne)
	a, b, database := twoProcesses(t, branch)

	held := syncInBackground(a)
	branch.waitHeld(t)
	branch.moveHead(commitTwo)
	if _, err := b.Sync(context.Background(), "CORE", archRepo); err != nil {
		t.Fatalf("the second process's sync: %v", err)
	}
	reSaveSource(t, b)
	branch.releaseHeld()

	answer := testwait.Receive(t, "the held sync to end once it was released", held)
	if answer.err != nil {
		t.Fatalf("the held sync: %v", answer.err)
	}

	var lastCommit *string
	if err := database.Pool.QueryRow(context.Background(),
		`select last_commit from architecture_sources where project_key = 'CORE'`).Scan(&lastCommit); err != nil {
		t.Fatalf("read the source row: %v", err)
	}
	if lastCommit == nil || *lastCommit != commitTwo {
		got := "<nil>"
		if lastCommit != nil {
			got = *lastCommit
		}
		t.Errorf("the row records commit %s, want %s: the held sync's compare-and-set on last_sync_at "+
			"passed null = null after the re-save (ABA) and recorded the older head it read before "+
			"another process imported %s", got, commitTwo, commitTwo)
	}
	if got := sourceComponents(t, database); !reflect.DeepEqual(got, infraComponents) {
		t.Errorf("components = %#v\nwant the newer head's %#v: the held sync projected the older head over it", got, infraComponents)
	}
	got := projectEvents(t, database)
	newer := slices.Index(got, "architecture.synced "+commitTwo)
	if newer < 0 {
		t.Fatalf("events = %#v, want an architecture.synced for %s", got, commitTwo)
	}
	if slices.Contains(got[newer+1:], "architecture.synced "+commitOne) {
		t.Errorf("events = %#v: architecture.synced for the older head %s was emitted after the newer head %s's",
			got, commitOne, commitTwo)
	}
}

// A source whose every sync is held by a GitHub rate limit recorded in github_rate_limits fails
// the same way each time; the importer's rule is that the same failure over and over emits one
// architecture.sync_failed event, not one per sync. The second sync runs after a minute has passed
// on the database's clock (the recorded limit's end moved one minute closer), as a later tick does.
func TestAHeldGitHubRateLimitEmitsOneSyncFailedEvent(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	_, database := newImporterFixture(t, fake)

	limits := ratelimit.New(database.Pool)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.test", PEM: pemText}, server.URL, githubapp.WithLimits(limits))
	if err != nil {
		t.Fatalf("new github client: %v", err)
	}
	importer := NewImporter(database, client, events.NewBroker())

	if err := limits.Record(ctx, githubapp.AppScope, time.Hour); err != nil {
		t.Fatalf("record the App's rate limit: %v", err)
	}

	first, err := importer.Sync(ctx, "CORE", archRepo)
	var limited *githubapp.RateLimitError
	if !errors.As(err, &limited) || !limited.Held {
		t.Fatalf("first sync: err = %v, want the held rate limit", err)
	}
	if _, err := database.Pool.Exec(ctx, `update github_rate_limits set until = until - interval '1 minute'`); err != nil {
		t.Fatalf("pass a minute on the recorded limit: %v", err)
	}
	second, err := importer.Sync(ctx, "CORE", archRepo)
	if !errors.As(err, &limited) || !limited.Held {
		t.Fatalf("second sync: err = %v, want the held rate limit", err)
	}

	var failed []string
	for _, event := range projectEvents(t, database) {
		if strings.HasPrefix(event, "architecture.sync_failed ") {
			failed = append(failed, event)
		}
	}
	if len(failed) != 1 {
		text := func(value *string) string {
			if value == nil {
				return "<nil>"
			}
			return *value
		}
		t.Errorf("one held rate limit over two syncs emitted %d architecture.sync_failed events, want 1: "+
			"the failure text carries the limit's remaining wait, so every sync records a \"changed\" error "+
			"(last_error %q then %q)\nevents: %#v", len(failed), text(first.LastError), text(second.LastError), failed)
	}
}

// A leased import pass whose sync meets a 503 GitHub answered, finds as it records the failure that
// a settings save wrote the source's row since its load (errSuperseded), and then reads the source
// on the flight's retry, records succeeded with no cause: the flight asks again, so the last try
// that reached GitHub counts, and the superseded try's failures are dropped (githubreads.Hold).
func TestALeasedImportPassRecordsTheRetryOfASupersededTryNotItsFailure(t *testing.T) {
	branch := newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles()})
	branch.holdNextHeadLookupAnswering(http.StatusServiceUnavailable)
	importer, other, database := twoProcesses(t, branch)
	pass, reads := leasedImports(t, importer, database)

	passed := make(chan struct{})
	go func() {
		defer close(passed)
		pass()
	}()
	// The first try loaded the row before its head lookup, so a save while the lookup is held
	// makes that try's record of the 503 stale.
	branch.waitHeld(t)
	reSaveSource(t, other)
	branch.releaseHeld()
	testwait.Receive(t, "the leased import pass to end", passed)

	if got := reads.take(); len(got) != 1 || got[0].read != githubreads.ArchitectureImport || !got[0].succeeded || got[0].cause != "" {
		t.Fatalf("the pass whose superseded try met a 503 and whose retry read the source recorded %+v, want one succeeded %s attempt with no cause",
			got, githubreads.ArchitectureImport)
	}
	var lastCommit, lastError string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(last_commit, ''), coalesce(last_error, '') from architecture_sources where project_key = 'CORE'`).Scan(&lastCommit, &lastError); err != nil {
		t.Fatalf("read the source row: %v", err)
	}
	if lastCommit != commitOne || lastError != "" {
		t.Fatalf("the source row holds commit %q and error %q, want the retry's import of %s and no error", lastCommit, lastError, commitOne)
	}
	for _, event := range projectEvents(t, database) {
		if strings.HasPrefix(event, "architecture.sync_failed") {
			t.Fatalf("events hold %q, want none: the superseded try records nothing", event)
		}
	}
}

// A leased import pass whose sync meets GitHub not answering its head lookup until the flight's
// own bound ends, and finds as it records that failure that a settings save wrote the source's row
// since its load (errSuperseded), retries on the spent bound, so the retry fails at its load of
// the row and asks GitHub nothing. The pass records failed with the superseded try's cause: no
// later try reached GitHub, so nothing replaced that try's failure, and dropping it would record
// a read GitHub never answered as succeeded.
//
// The save comes between the first try's load and its record of the failure whatever the timing.
// It is written, holding the row's lock, before the pass begins, so that load reads the row
// without it, and it commits once the try's head lookup has arrived, after that load. The record
// takes the row's lock (lockSourceRow), so it waits for the commit however late the commit comes,
// and then finds the row written since the load. The flight's bound (syncTimeout) runs on a fake
// clock, which the test moves to the deadline the flight armed once the save has committed, so
// the held lookup ends on that bound and on nothing's timing.
func TestALeasedImportPassKeepsASupersededTrysFailureWhenItsRetryAsksGitHubNothing(t *testing.T) {
	branch := newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles()})
	branch.holdNextHeadLookup()
	fake := clocktest.New(time.Date(2026, time.November, 19, 9, 0, 0, 0, time.UTC))
	importer, other, database := twoProcessesOn(t, branch, fake)
	pass, reads := leasedImports(t, importer, database)
	save := beginReSave(t, other)

	passed := make(chan struct{})
	go func() {
		defer close(passed)
		pass()
	}()
	// The first try loaded the row before its head lookup, so the save's commit once the lookup
	// has arrived makes that try's record of its failure stale. The lookup is released only once
	// the pass has ended, so GitHub answers it nothing within the flight's bound.
	branch.waitHeld(t)
	if err := save.Commit(context.Background()); err != nil {
		t.Fatalf("commit the settings save: %v", err)
	}
	armed := fake.AwaitArmed(t, "the flight's deadline", 1)
	fake.Set(armed[0])
	testwait.Receive(t, "the leased import pass to end once its flight's deadline passed", passed)
	branch.releaseHeld()

	if got := reads.take(); len(got) != 1 || got[0].read != githubreads.ArchitectureImport || got[0].succeeded ||
		!strings.Contains(got[0].cause, "/repos/"+archRepo+"/commits/main") ||
		!strings.HasSuffix(got[0].cause, context.DeadlineExceeded.Error()) {
		t.Fatalf("the pass whose superseded try's head lookup GitHub never answered, and whose retry asked GitHub nothing, recorded %+v; "+
			"want one failed %s attempt whose cause is that lookup's deadline", got, githubreads.ArchitectureImport)
	}
	// The save superseded the first try, which so recorded nothing on the row, and the retry
	// ended before it had anything to record.
	var lastCommit, lastError string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(last_commit, ''), coalesce(last_error, '') from architecture_sources where project_key = 'CORE'`).Scan(&lastCommit, &lastError); err != nil {
		t.Fatalf("read the source row: %v", err)
	}
	if lastCommit != "" || lastError != "" {
		t.Fatalf("the source row holds commit %q and error %q, want neither: the save superseded the try that failed", lastCommit, lastError)
	}
	for _, event := range projectEvents(t, database) {
		if strings.HasPrefix(event, "architecture.sync_failed") {
			t.Fatalf("events hold %q, want none: the superseded try records nothing", event)
		}
	}
}
