package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// RepoProjectColumns are the repo_projects columns ScanRepoProject reads, in its order.
const RepoProjectColumns = `repo, project, is_default, created_by, created_at, renamed_to`

// ScanRepoProject decodes one RepoProjectColumns row into a repository's mapping.
func ScanRepoProject(row pgx.Row) (model.RepoProject, error) {
	var mapping model.RepoProject
	var createdBy []byte
	if err := row.Scan(&mapping.Repo, &mapping.Project, &mapping.Default, &createdBy, &mapping.CreatedAt, &mapping.RenamedTo); err != nil {
		return model.RepoProject{}, err
	}
	if err := json.Unmarshal(createdBy, &mapping.CreatedBy); err != nil {
		return model.RepoProject{}, fmt.Errorf("decode repository project author: %w", err)
	}
	return mapping, nil
}

// LockRepoProjectWrite takes, in this order, every lock a write to repo's mapping in repo_projects
// needs, and returns the mapping as it stands under them (nil when repo has none). projects are
// the projects the write may put the mapping in.
//
//   - repo's own advisory lock, which every writer of the mapping takes whether or not its row
//     exists yet, so the mapping's project and its existence hold still from here on (a default
//     switch in its project may still clear its is_default until the project lock below is held,
//     and the row read after that lock sees it);
//   - the event owners of the mapping's current project and of projects, in one LockOwners call
//     before the first event, which serializes each project's default switch and orders two
//     moves between the same projects;
//   - the mapping's row, for update, which holds the row and its project still until this write
//     commits, so the default it read and the project it is moving the mapping out of cannot
//     change under it. No issue write takes a lock on this row: an issue names any owner/name as
//     its repository, mapped or not, so nothing an issue write does depends on this row.
//
// Every writer of a mapping goes through here, and none takes the change log's lock
// (lockChangesStatement) inside its transaction: the mapping's trigger records its settings
// change in the change log's outbox, which takes no lock (migration 20261117100020), and each
// writer moves it into the log after its commit (DrainRepoProjectChange). These are the locks the release before this one
// takes, in the same order, so two writes of one repository, one of each build of a roll, wait for
// each other in that one order too. The writers: the PUT and the DELETE of api's mapping routes,
// the boot seed (api.SeedRepoProjects), cmd/dispatch-seed's projects, and the delivery fixtures
// that tests and the eval bench write (deliverytest.WriteRepoProjects). The one exception is the
// eval bench's -base load, which inserts its mappings with none of these locks
// (deliverytest.InsertRepoProjects): it writes into the base head's schema, which has no owner lock
// for LockOwners to take, and no server runs while it loads, so no other writer of a mapping is
// there for these locks to order it against.
func LockRepoProjectWrite(ctx context.Context, tx pgx.Tx, broker *events.Broker, repo string, projects ...string) (*model.RepoProject, error) {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(hashtext('dispatch-repo-project'), hashtext($1))
	`, repo); err != nil {
		return nil, fmt.Errorf("lock repository mapping %s: %w", repo, err)
	}
	var current string
	err := tx.QueryRow(ctx, `select project from repo_projects where repo = $1`, repo).Scan(&current)
	switch {
	case err == nil:
		projects = append(projects, current)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if len(projects) == 0 {
		return nil, nil
	}
	owners := make([]model.Event, 0, len(projects))
	for _, project := range projects {
		owners = append(owners, model.Event{ProjectKey: &project})
	}
	if err := broker.LockOwners(ctx, tx, owners...); err != nil {
		return nil, err
	}
	mapping, err := ScanRepoProject(tx.QueryRow(ctx, `
		select `+RepoProjectColumns+` from repo_projects where repo = $1 for update
	`, repo))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mapping, nil
}

// DrainRepoProjectChange is what every writer of a mapping runs right after its write committed:
// the drain of one batch of the change log's outbox (DrainAfterCommit), so the settings change the
// mapping's trigger recorded (migration 20261117100020) is moved into the log where no more than a
// batch was pending ahead of it, as an eval write's own drain moves its rows (evals'
// drainChanges).
func DrainRepoProjectChange(ctx context.Context, pool *store.Pool) {
	DrainAfterCommit(ctx, pool, "dispatch: a repository mapping write could not move its change into the delivery change log; the next drain moves it")
}
