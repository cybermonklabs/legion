package architecture

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// One import pass imports every enabled source, two of one project included, and a source whose
// import fails - here the second of CORE's, before OPS's - records its failure and leaves the
// sources after it to import.
func TestAnImportPassSyncsEverySourceAndGoesOnPastAFailure(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), repos: map[string]*fakeRepo{
		"acme/broken": {commit: commitAcmeBroken, branchMissing: true},
		"legion/ops":  {commit: commitOps, files: map[string]string{"ops.md": "---\ntitle: Operations\n---\nRunbooks.\n"}},
	}}
	importer, database := newImporterFixture(t, fake)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('OPS', 'Operations', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'acme/broken', 'main', 11, '{"kind":"user","id":"alice"}'),
			('OPS', 'legion/ops', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}

	syncAll(ctx, importer)

	rows, err := database.Pool.Query(ctx, `
		select concat_ws(' ', project_key, repo, coalesce(last_commit, '-'), case when last_error is null then 'ok' else 'failed' end,
		                 (select count(*) from components c where c.project_key = s.project_key and c.source_id = s.id))
		from architecture_sources s order by project_key, id
	`)
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	want := []string{
		"CORE legion/arch " + commitOne + " ok 3",
		"CORE acme/broken - failed 0",
		"OPS legion/ops " + commitOps + " ok 1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources after one pass = %#v\nwant %#v", got, want)
	}
}

// writeStatement is a statement that writes rows.
var writeStatement = regexp.MustCompile(`(?is)^\s*(insert|update|delete)\b`)

// transactionStatement frames a transaction instead of writing anything itself. Every write the
// importer makes runs in a transaction of its own (Importer.inTransaction), so the freshness stamp
// carries one begin and one commit; what a frame carries is recorded too, so a write other than the
// stamp is still caught, and counting the frames still catches a second transaction (a row lock,
// say) the unchanged-head pass has no business opening.
var transactionStatement = regexp.MustCompile(`(?is)^\s*(begin|commit|rollback)\b`)

// touchStatement is the unchanged-head short-circuit's one write (Importer.touch).
var touchStatement = regexp.MustCompile(`(?is)^\s*update\s+architecture_sources\s+set\s+last_sync_at\s*=\s*now\(\),\s*last_commit\s*=\s*\$5\b`)

// The import timer stays, because it repairs a push that never arrived, and most of its passes
// find heads that have not moved, so such a pass must stay cheap: it fetches no tree and no blob,
// appends no event, and writes each source's freshness stamp once (touch) and nothing else - here
// over three sources of two projects, read through a pool of its own whose every statement is
// recorded.
func TestAnImportPassOverUnchangedHeadsFetchesNothingAndTouchesEachSourceOnce(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), repos: map[string]*fakeRepo{
		"acme/infra": {commit: commitInfraOne, files: map[string]string{"infra.md": "---\ntitle: Infrastructure\n---\nClusters.\n"}},
		"legion/ops": {commit: commitOps, files: map[string]string{"ops.md": "---\ntitle: Operations\n---\nRunbooks.\n"}},
	}}
	importer, database := newImporterFixture(t, fake)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('OPS', 'Operations', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'acme/infra', 'main', 11, '{"kind":"user","id":"alice"}'),
			('OPS', 'legion/ops', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	syncAll(ctx, importer)
	var imported int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_sources where last_commit is not null and last_error is null`).Scan(&imported); err != nil || imported != 3 {
		t.Fatalf("%d of 3 sources imported by the first pass (err %v); the pass under test needs every source current", imported, err)
	}
	trees, blobs := fake.reads()
	eventsBefore := projectEvents(t, database)
	var lastSync time.Time
	if err := database.Pool.QueryRow(ctx, `select max(last_sync_at) from architecture_sources`).Scan(&lastSync); err != nil {
		t.Fatalf("read last_sync_at: %v", err)
	}

	recorder := &storetest.StatementRecorder{}
	syncAll(ctx, NewImporter(&store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}, importer.github, events.NewBroker()))

	if gotTrees, gotBlobs := fake.reads(); gotTrees != trees || gotBlobs != blobs {
		t.Fatalf("the pass over unchanged heads read %d trees and %d blobs; want none", gotTrees-trees, gotBlobs-blobs)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, eventsBefore) {
		t.Fatalf("the pass over unchanged heads appended events: %v -> %v", eventsBefore, got)
	}
	var fresh int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_sources where last_sync_at > $1`, lastSync).Scan(&fresh); err != nil || fresh != 3 {
		t.Fatalf("%d of 3 sources carry a freshness stamp from the pass over unchanged heads (err %v); want every one", fresh, err)
	}
	touches, framing := 0, 0
	for _, sql := range recorder.Statements() {
		switch {
		case touchStatement.MatchString(sql):
			touches++
		case transactionStatement.MatchString(sql):
			framing++
		case writeStatement.MatchString(sql):
			t.Errorf("the pass over unchanged heads ran a write other than the freshness stamp: %s", strings.TrimSpace(sql))
		}
	}
	if touches != 3 {
		t.Fatalf("the pass over unchanged heads stamped freshness %d times over 3 sources; want one each", touches)
	}
	if framing != 2*touches {
		t.Fatalf("the pass over unchanged heads ran %d transaction statements around %d freshness stamps; want each stamp's own begin and commit and no other transaction", framing, touches)
	}
}

// readRecorder is a githubreads.Recorder that keeps each attempt it is handed, in the order the
// passes recorded them, so a test reads what a pass gave GET /healthz to answer.
type readRecorder struct {
	mu       sync.Mutex
	recorded []recordedRead
}

// recordedRead is one attempt a readRecorder was handed.
type recordedRead struct {
	read      githubreads.Read
	succeeded bool
	cause     string
}

func (r *readRecorder) record(_ context.Context, read githubreads.Read, succeeded bool, cause string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, recordedRead{read: read, succeeded: succeeded, cause: cause})
	return nil
}

// take answers the attempts recorded since the last take, and forgets them.
func (r *readRecorder) take() []recordedRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := r.recorded
	r.recorded = nil
	return taken
}

// leasedImports answers the pass the import's loop runs under its lease (syncAllLeased) over
// importer, each call due one SyncInterval after the one before so the lease's record of the last
// period served never skips it, and the recorder importer records each pass's attempt at reading
// GitHub into.
func leasedImports(t *testing.T, importer *Importer, database *store.Store) (pass func(), reads *readRecorder) {
	t.Helper()
	leases := architectureLeases(t, database)
	reads = &readRecorder{}
	importer.GitHubReads = reads.record
	due := time.Now()
	return func() {
		syncAllLeased(context.Background(), importer, leases, due)
		due = due.Add(SyncInterval)
	}, reads
}

// Each leased import pass records its attempt at reading GitHub, which GET /healthz answers: a
// pass whose first request GitHub answered 503 records failed, with that answer as its cause; the
// next pass, which GitHub answered, records succeeded; and a pass with no enabled source, which
// asks GitHub nothing, records nothing at all.
func TestEachLeasedImportPassRecordsItsAttemptAtReadingGitHub(t *testing.T) {
	ctx := context.Background()
	// The first request of a sync looks the installation of the source's repository up.
	fake := &fakeSource{t: t, files: validFiles(), installationStatus: http.StatusServiceUnavailable}
	importer, database := newImporterFixture(t, fake)
	pass, reads := leasedImports(t, importer, database)

	pass()
	installation := "/repos/" + archRepo + "/installation"
	got := reads.take()
	if len(got) != 1 || got[0].read != githubreads.ArchitectureImport || got[0].succeeded ||
		!strings.Contains(got[0].cause, installation) || !strings.Contains(got[0].cause, "status 503") {
		t.Fatalf("the pass GitHub answered 503 recorded %+v, want one failed %s attempt whose cause is that 503", got, githubreads.ArchitectureImport)
	}

	fake.installationStatus = 0
	pass()
	if got := reads.take(); len(got) != 1 || got[0].read != githubreads.ArchitectureImport || !got[0].succeeded || got[0].cause != "" {
		t.Fatalf("the pass GitHub answered recorded %+v, want one succeeded %s attempt with no cause", got, githubreads.ArchitectureImport)
	}
	if rows := componentRows(t, database); len(rows) == 0 {
		t.Fatal("the pass GitHub answered imported no component")
	}

	if _, err := database.Pool.Exec(ctx, `update architecture_sources set enabled = false`); err != nil {
		t.Fatalf("disable the source: %v", err)
	}
	installations, heads := fake.lookups()
	pass()
	if got := reads.take(); len(got) != 0 {
		t.Fatalf("a pass with no enabled source recorded %+v, want nothing", got)
	}
	if moreInstallations, moreHeads := fake.lookups(); moreInstallations != installations || moreHeads != heads {
		t.Fatalf("the pass with no enabled source looked up %d installations and %d heads, want none",
			moreInstallations-installations, moreHeads-heads)
	}
}

// A leased import pass records failed when a source's sync gives up a read of GitHub on an answer
// the client leaves to its caller and notes nothing of -- a 404 on the installation lookup, a
// missing Contents permission, a 422 for the branch's head, the directory or a file -- so in each
// failed row only the sync's own note can make the record say so, and its cause names what
// failed. A directory GitHub answers 404 for is an empty model (Dir), and notes that do not parse,
// a note over notes.MaxFileSize and a listing over notes.MaxFiles, which DirFiles refuses by the
// listing before any blob read, are the source's content, which last_error records: none is a
// read of GitHub that failed, and each records succeeded, the pass having sent the installation,
// token, head and directory reads.
func TestALeasedImportPassRecordsAReadOfGitHubItGaveUpAsFailed(t *testing.T) {
	for _, row := range []struct {
		name string
		// arrange sets GitHub's answers before the pass.
		arrange   func(fake *fakeSource)
		succeeded bool
		// causeHas is what the cause of a failed attempt must name.
		causeHas []string
		// lastError is whether the sync records a failure on the source row: every row but the
		// empty directory's, since notes that do not parse fail the sync without failing a read.
		lastError bool
		// lastErrorHas is what a recorded last_error must name, where the row names one.
		lastErrorHas string
	}{{
		name: "a source whose repository the App is not installed on",
		// GitHub answers the installation lookup of a repository the App is not installed on 404.
		arrange:   func(fake *fakeSource) { fake.installationStatus = http.StatusNotFound },
		causeHas:  []string{"not installed", archRepo},
		lastError: true,
	}, {
		name:      "a source whose installation lacks Contents read",
		arrange:   func(fake *fakeSource) { fake.contents = "none" },
		causeHas:  []string{"lacks Contents: read", archRepo},
		lastError: true,
	}, {
		name: "a source whose branch GitHub does not have",
		// GitHub answers the head of a branch it does not have 422 (writeNoCommit).
		arrange:   func(fake *fakeSource) { fake.branchMissing = true },
		causeHas:  []string{"branch does not exist", archRepo},
		lastError: true,
	}, {
		name:      "a source whose directory read GitHub answers 422",
		arrange:   func(fake *fakeSource) { fake.treeStatus = http.StatusUnprocessableEntity },
		causeHas:  []string{"/repos/" + archRepo + "/git/trees/", "status 422"},
		lastError: true,
	}, {
		name:      "a source whose file read GitHub answers 422",
		arrange:   func(fake *fakeSource) { fake.blobStatus = http.StatusUnprocessableEntity },
		causeHas:  []string{"/repos/" + archRepo + "/git/blobs/", "status 422"},
		lastError: true,
	}, {
		name:      "a source whose directory GitHub answers 404 for",
		arrange:   func(fake *fakeSource) { fake.treeStatus = http.StatusNotFound },
		succeeded: true,
	}, {
		name:      "a source whose listing names a note over the size limit",
		arrange:   func(fake *fakeSource) { fake.files["big.md"] = strings.Repeat("a", notes.MaxFileSize+1) },
		succeeded: true,
		lastError: true,
	}, {
		name: "a source whose listing names more notes than a set holds",
		arrange: func(fake *fakeSource) {
			for i := range notes.MaxFiles {
				fake.files[fmt.Sprintf("extra-%04d.md", i)] = "# Extra\n"
			}
		},
		succeeded:    true,
		lastError:    true,
		lastErrorHas: notes.ErrSetTooLarge.Error(),
	}, {
		name: "a source whose notes do not parse",
		arrange: func(fake *fakeSource) {
			fake.files = map[string]string{"api.md": "x\n", "API.md": "duplicate\n"}
		},
		succeeded: true,
		lastError: true,
	}} {
		t.Run(row.name, func(t *testing.T) {
			fake := &fakeSource{t: t, files: validFiles()}
			row.arrange(fake)
			importer, database := newImporterFixture(t, fake)
			pass, reads := leasedImports(t, importer, database)

			pass()

			var lastError string
			if err := database.Pool.QueryRow(context.Background(),
				`select coalesce(last_error, '') from architecture_sources where project_key = 'CORE'`).Scan(&lastError); err != nil {
				t.Fatalf("read the source's last_error: %v", err)
			}
			if (lastError != "") != row.lastError {
				t.Fatalf("the source's last_error after the pass = %q, want one recorded: %t", lastError, row.lastError)
			}
			if !strings.Contains(lastError, row.lastErrorHas) {
				t.Fatalf("the source's last_error after the pass = %q, want it to name %q", lastError, row.lastErrorHas)
			}
			got := reads.take()
			if len(got) != 1 || got[0].read != githubreads.ArchitectureImport {
				t.Fatalf("the pass recorded %+v, want one %s attempt", got, githubreads.ArchitectureImport)
			}
			if row.succeeded {
				if !got[0].succeeded || got[0].cause != "" {
					t.Fatalf("the pass recorded %+v, want succeeded with no cause", got[0])
				}
				return
			}
			if got[0].succeeded {
				t.Fatalf("the pass recorded succeeded, want failed (last_error %q)", lastError)
			}
			for _, text := range row.causeHas {
				if !strings.Contains(got[0].cause, text) {
					t.Fatalf("the recorded cause %q does not name %q", got[0].cause, text)
				}
			}
		})
	}
}
