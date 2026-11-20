// Package architecture imports a project's architecture model. The importer
// fetches a source's notes through the GitHub App, parses them with the notes
// package, and projects the model into the components tables.
package architecture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SourceDir is the fixed directory an architecture source's model is read
// from, at the configured branch's head commit.
const SourceDir = ".dispatch/architecture"

// ErrNoSource is "this project has no architecture source configured" (or
// none from the named repository).
var ErrNoSource = errors.New("no architecture source configured")

// SystemActor authors every importer-written event: the importer is not a
// human and not a session.
var SystemActor = model.Actor{Kind: "system", ID: "architecture-importer"}

// SourceColumns is the architecture_sources select list ScanSource reads, in
// scan order; every query that returns a source row selects exactly this,
// against the table under its own name (a select's from, or the target of an
// insert, update or delete ... returning). Its last column counts the
// components the source currently projects.
const SourceColumns = `project_key, id, repo, branch, enabled, installation_id, created_by, created_at, last_sync_at, last_commit, last_error, last_tree_sha,
	(select count(*) from components where components.project_key = architecture_sources.project_key and components.source_id = architecture_sources.id)`

// syncTimeout bounds one whole sync — every GitHub call plus the projection —
// so a slow upstream ends one source's shared flight (and so every caller
// waiting on it) instead of hanging on indefinitely.
var syncTimeout = 2 * time.Minute

// errSourceMoved is a sync that found the source's repo or branch changed
// while its fetch was in flight: nothing was written, and the next trigger
// imports the new configuration.
var errSourceMoved = errors.New("the architecture source changed during the sync; nothing was recorded")

// errSuperseded is a write that found the source row's version (xmin) no longer the one its sync
// loaded before fetching: another sync (any process's), a settings save or any other write
// committed since, so what this sync read from GitHub is older than what the row holds. Nothing
// was written, and Sync retries from the load.
var errSuperseded = errors.New("another sync of the architecture source recorded first; nothing was recorded")

// loadedSource is the source row as a sync loaded it before its fetch, with the row's version
// then: PostgreSQL's xmin, the transaction that wrote the row's current version, read as text.
// Every write of the row by any build changes it -- an importer write, a settings save that
// writes the same repository and branch again (and sets last_sync_at back to null, so
// last_sync_at cannot be the version), a toggle -- while the importer's own `for update` lock
// does not, and no migration is needed. A sync's writes compare it under the row lock.
type loadedSource struct {
	model.ArchitectureSource
	version string
}

// Importer imports a project's architecture model from its configured
// sources, one repository each. Every trigger — the five-minute ticker, the
// Settings Refresh button, and the `dispatch architecture-sync` command —
// funnels through Sync. What orders the writes of every process is the source
// row: each write takes its lock and commits only while the row's version
// (xmin) is still the one the sync loaded before its fetch, so a sync another
// write overtook never records the older head, files or failure it read
// (errSuperseded). Within one process, the callers of one source share one
// flight instead of each making its own set of GitHub calls, no lock is held
// across a GitHub call, and two sources — of one project or of two — never
// wait on each other here; the source row still orders what every process
// writes.
type Importer struct {
	store  *store.Store
	github *githubapp.Client
	events *events.Broker

	// GitHubReads records each pass of the running import (the pass syncAllLeased runs under its
	// lease) as the newest attempt of githubreads.ArchitectureImport, which GET /healthz answers:
	// whether a GitHub read of the pass failed, and the first failure. A sync the Settings Refresh
	// route or `dispatch architecture-sync` runs records nothing. Nil records nothing.
	GitHubReads githubreads.Recorder

	// owner outlives every caller: the process stopping (Close) is the only
	// thing that cancels a flight, since a flight runs on behalf of callers
	// that have since left.
	owner   context.Context
	abandon context.CancelFunc
	running syncCount
	// syncs is keyed on the source, project and repository both, not on the
	// project: Sync answers that source's row, and SyncProject needs one
	// result per repository. Two sources of one project cannot corrupt each
	// other without a mutex here, because every projection takes
	// lockProjectSources (`order by id for update`) and judges heldElsewhere
	// inside that lock.
	syncs singleflight.Group
	// ended is, for each source a call of Sync is running for, its newest
	// flight to have ended, on syncs' keys: what a caller that refused an
	// older flight takes when it began after the caller's call. Sync enters
	// its key on the way in and leaves it on the way out, so no entry
	// outlives the calls of its key.
	ended endedFlights

	// afterFlightTaken runs in Sync once a caller holds the flight it waits
	// on, joined or started; afterSyncRefusal once it has refused a flight
	// whose head lookup came before its call, before it looks again; and
	// afterCalledAt once it has stamped its call, before it looks for a
	// flight. Nothing in production sets any of them; the package's tests of
	// late refusers wait on the first rather than sleeping for their callers
	// to join, and hold a caller on the other two.
	afterFlightTaken func()
	afterSyncRefusal func()
	afterCalledAt    func()
}

// NewImporter wires the importer to the database, the GitHub App client (nil
// is the "no app credentials" state: every sync answers ErrNoAppKey), and the
// event broker.
func NewImporter(database *store.Store, github *githubapp.Client, broker *events.Broker) *Importer {
	owner, abandon := context.WithCancel(context.Background())
	return &Importer{store: database, github: github, events: broker, owner: owner, abandon: abandon}
}

// Close ends the importer's flights and waits for them, and for each call of
// Sync waiting on one, which ends once the flight it joined has answered:
// architecture.Run is not among the waits shutdown takes
// (cmd/dispatch/shutdown.go waits background.Wait and waitIntakes, then
// waitUntilIdle and pool.Close), and a flight runs on a context the caller's
// cancellation no longer reaches, so without this a sync still fetching when
// the process stops would write into a closing pool. It is safe to call twice,
// and on a nil importer.
func (i *Importer) Close() {
	if i == nil {
		return
	}
	i.abandon()
	i.running.stop()
}

// HasApp reports whether the App credentials needed to sync are configured.
func (i *Importer) HasApp() bool {
	return i != nil && i.github != nil
}

// EnabledSource names one enabled architecture source: its project and its
// repository.
type EnabledSource struct {
	Project string
	Repo    string
}

// ListEnabled names every enabled architecture source, ordered by project and
// then by the source's id: the ticker's work list.
func (i *Importer) ListEnabled(ctx context.Context) ([]EnabledSource, error) {
	rows, err := i.store.Pool.Query(ctx, `
		select project_key, repo from architecture_sources where enabled order by project_key, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list enabled architecture sources: %w", err)
	}
	defer rows.Close()
	var sources []EnabledSource
	for rows.Next() {
		var source EnabledSource
		if err := rows.Scan(&source.Project, &source.Repo); err != nil {
			return nil, fmt.Errorf("scan enabled architecture source: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enabled architecture sources: %w", err)
	}
	return sources, nil
}

// SyncResult is one source's outcome of SyncProject: the row as Sync answered
// it and Sync's error. A failure that could not be recorded carries a zero
// Source.
type SyncResult struct {
	Source model.ArchitectureSource
	Err    error
}

// SyncProject imports every source of project, in id order, attempting each
// whatever an earlier one did, and answers each source's outcome in that
// order. A source deleted while the call runs is left out. A project with no
// source is ErrNoSource.
func (i *Importer) SyncProject(ctx context.Context, project string) ([]SyncResult, error) {
	rows, err := i.store.Pool.Query(ctx,
		`select repo from architecture_sources where project_key = $1 order by id`, project)
	if err != nil {
		return nil, fmt.Errorf("list architecture sources of %s: %w", project, err)
	}
	repos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list architecture sources of %s: %w", project, err)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("%w for %s", ErrNoSource, project)
	}
	results := make([]SyncResult, 0, len(repos))
	for _, repo := range repos {
		source, err := i.Sync(ctx, project, repo)
		if errors.Is(err, ErrNoSource) {
			continue
		}
		results = append(results, SyncResult{Source: source, Err: err})
	}
	return results, nil
}

// Sync imports one source's architecture model — project's source from repo —
// at its branch's head, and replaces only that source's components.
//
// It returns the source row as it now stands plus, when the import failed, the
// failure itself: the row carries the same text in last_error, the source's
// previous projection and every other source's stay up (the spec's drift
// rule), and the caller decides how to answer. A missing source row is
// ErrNoSource with a zero row.
//
// A healthy source whose branch head is the recorded commit — or whose new
// head carries an unchanged architecture subtree — only refreshes the
// bookkeeping (last_sync_at, and the commit for the moved head): no file
// fetch, no re-projection, no event, so a caller looping the sync route is
// bounded to the head lookup.
//
// Events follow state changes of the source, not ticks: `architecture.synced`
// when its imported model changes (or it is recovering from a failure),
// `architecture.sync_failed` when its error text first appears or changes. A
// steady source — same commit, or the same failure over and over — emits
// nothing.
//
// Writes are ordered by when their sync loaded the source row, not by commit
// date: the branch head is what is imported, so a force-push back to an older
// commit still imports. A sync whose write finds the row changed since its load
// (errSuperseded: another process's sync, or a settings save, committed first)
// starts again from the load inside the same deadline; each retry follows
// another committed write, and usually ends in the unchanged-head refresh.
//
// Callers of one source share one flight in this process: the first makes the
// GitHub calls and runs the retry chain, and the others read its answer, so a
// Refresh clicked twice over costs one fetch. A flight older than its caller
// does not serve it, though — the caller stamps the moment it asked and takes
// an answer only from a flight that issued its head lookup no earlier, so a
// person who pushes and then asks for a Refresh is never told the pre-push
// commit with a fresh last_sync_at. A caller that refused a flight and looks
// again after the next one has ended takes that one's answer rather than
// starting a third, so the callers that refused one flight make one more set
// of GitHub calls between them, however late each looks again. That answer is
// kept only while a call of the source runs (endedFlights).
func (i *Importer) Sync(ctx context.Context, project, repo string) (model.ArchitectureSource, error) {
	// A caller that has already hung up, and an importer whose process is
	// stopping, start no flight: the work would outlive both of them.
	if err := ctx.Err(); err != nil {
		return model.ArchitectureSource{}, err
	}
	if err := i.owner.Err(); err != nil {
		return model.ArchitectureSource{}, err
	}
	key := syncKey(project, repo)
	// The key is entered before the call is stamped, so a flight that looks
	// the head up after this call's stamp, and so is fresh for it, finds the
	// entry held open when it ends, whoever started it and whether its
	// starter is still there.
	i.ended.enter(key)
	defer i.ended.leave(key)
	// calledAt is stamped once, before any join: what a flight has to be
	// newer than is the moment this caller asked, not the moment it last
	// looked.
	calledAt := time.Now()
	// The call counts as a sync running (Syncing) from here until the flight
	// it joined last has answered, whether or not its caller waits that long:
	// a flight goes on writing after a caller that left it, and until the
	// flight's goroutine begins (flight), this count is the only one it has.
	if !i.running.begin() {
		return model.ArchitectureSource{}, i.owner.Err()
	}
	if i.afterCalledAt != nil {
		i.afterCalledAt()
	}
	for {
		// A flight this caller starts hands back the newest flight of the
		// source to have ended, when its head lookup came no earlier than
		// this caller's call, as it would have had the caller joined it, and
		// makes no GitHub call. Otherwise it runs a flight of its own and
		// records it once it ends.
		answered := i.syncs.DoChan(key, func() (any, error) {
			if ended := i.ended.fresh(key, calledAt); ended != nil {
				return ended, ended.err
			}
			flight, err := i.flight(ctx, project, repo)
			flight.err = err
			i.ended.record(key, flight)
			return flight, err
		})
		if i.afterFlightTaken != nil {
			i.afterFlightTaken()
		}
		select {
		case <-ctx.Done():
			// singleflight hands every caller's channel the flight's one
			// answer, so this receive ends once the flight has.
			go func() {
				<-answered
				i.running.done()
			}()
			return model.ArchitectureSource{}, ctx.Err()
		case result := <-answered:
			flight := result.Val.(*syncFlight)
			// Not Before rather than After: two readings of the clock that
			// land on the same instant must end the loop, not spin it.
			if !flight.servedAt().Before(calledAt) {
				i.running.done()
				return flight.source, result.Err
			}
			// That flight looked the head up before this caller existed, so
			// its row may predate a push the caller already knows about: take
			// the next one. singleflight deletes the key before it sends a
			// result, and the flight records itself as its source's newest
			// ended one before that, so the flight joined or started now began
			// after calledAt.
			//
			// That flight may still answer with one older than this call.
			// When an older caller that refused a flight started it, it
			// hands back the newest ended flight that is fresh for that older
			// call, and this caller, which joined it, refuses that answer too
			// and goes round again. That round makes no GitHub call: the
			// flight it joined answered from the record. The loop still ends.
			// A flight this caller starts answers only with a flight fresh
			// for this call, or makes its own head lookup after it. Only a
			// caller older than this one can hand it an answer it refuses,
			// those callers are a fixed set once this call is made, and each
			// of them ends by the same argument, the oldest first.
			if i.afterSyncRefusal != nil {
				i.afterSyncRefusal()
			}
		}
	}
}

// loadSourceByRepo reads the project's source from repo with its row version; none is
// ErrNoSource.
func (i *Importer) loadSourceByRepo(ctx context.Context, project, repo string) (loadedSource, error) {
	var source loadedSource
	var err error
	source.ArchitectureSource, err = scanOneSource(i.store.Pool.QueryRow(ctx,
		`select `+SourceColumns+`, xmin::text from architecture_sources where project_key = $1 and repo = $2`, project, repo),
		project+" from "+repo, &source.version)
	return source, err
}

// reloadSource re-reads source's row by its id; a deleted source is ErrNoSource.
func (i *Importer) reloadSource(ctx context.Context, source model.ArchitectureSource) (model.ArchitectureSource, error) {
	return scanOneSource(i.store.Pool.QueryRow(ctx,
		`select `+SourceColumns+` from architecture_sources where project_key = $1 and id = $2`, source.Project, source.ID),
		source.Project)
}

// scanOneSource decodes the one source row, and extra's columns after SourceColumns; none is
// ErrNoSource for name.
func scanOneSource(row pgx.Row, name string, extra ...any) (model.ArchitectureSource, error) {
	source, err := scanSource(row, extra...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ArchitectureSource{}, fmt.Errorf("%w for %s", ErrNoSource, name)
		}
		return model.ArchitectureSource{}, fmt.Errorf("read architecture source: %w", err)
	}
	return source, nil
}

// resolve splits the stored repository and resolves the installation token
// (the stored id is a cache: PR A's contract). A re-resolved installation is
// recorded only on the row as the sync loaded it, and the sync then carries
// that write's version as its own; a row written by anything else since the
// load is left alone, and the sync's own write finds it superseded and starts
// again from the load, which resolves again.
func (i *Importer) resolve(ctx context.Context, source *loadedSource) (owner, name, token string, err error) {
	// The stored `owner/name` is held to the names GitHub allows before its halves go into any
	// GitHub path (refs.SplitGitHubRepo), and refused with an error naming it otherwise. The
	// route that saves a source holds it to that rule already (api's parseSourceRepo); this keeps
	// a row stored before that, or by direct SQL, from ever reaching GitHub.
	owner, name, err = refs.SplitGitHubRepo(source.Repo)
	if err != nil {
		return "", "", "", err
	}
	installation, err := i.github.Installation(ctx, owner, name)
	if err != nil {
		return "", "", "", err
	}
	if installation.ID != source.InstallationID {
		var version string
		err := i.inTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				update architecture_sources set installation_id = $3
				where project_key = $1 and id = $2 and xmin::text = $4
				returning xmin::text`,
				source.Project, source.ID, installation.ID, source.version).Scan(&version)
		})
		switch {
		case err == nil:
			source.version = version
		case !errors.Is(err, pgx.ErrNoRows):
			return "", "", "", fmt.Errorf("record re-resolved installation: %w", err)
		}
		source.InstallationID = installation.ID
	}
	token, err = i.github.Token(ctx, installation.ID)
	if err != nil {
		return "", "", "", err
	}
	return owner, name, token, nil
}

// IsAccessFailure reports whether a sync failure is credential- or
// configuration-shaped: something a human fixes in GitHub or Settings, rather
// than an upstream outage or a malformed model.
func IsAccessFailure(err error) bool {
	return errors.Is(err, githubapp.ErrNoAppKey) ||
		errors.Is(err, githubapp.ErrNoInstallation) ||
		errors.Is(err, githubapp.ErrNoContentsRead) ||
		errors.Is(err, githubapp.ErrNoBranch)
}

// ScanSource decodes one SourceColumns row into a model.ArchitectureSource.
func ScanSource(row pgx.Row) (model.ArchitectureSource, error) {
	return scanSource(row)
}

// scanSource is ScanSource for a row that selects extra's columns after SourceColumns.
func scanSource(row pgx.Row, extra ...any) (model.ArchitectureSource, error) {
	var source model.ArchitectureSource
	var createdBy []byte
	if err := row.Scan(append([]any{
		&source.Project, &source.ID, &source.Repo, &source.Branch, &source.Enabled, &source.InstallationID,
		&createdBy, &source.CreatedAt, &source.LastSyncAt, &source.LastCommit, &source.LastError,
		&source.LastTreeSha, &source.Components,
	}, extra...)...); err != nil {
		return model.ArchitectureSource{}, err
	}
	if err := json.Unmarshal(createdBy, &source.CreatedBy); err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("decode architecture source author: %w", err)
	}
	return source, nil
}
