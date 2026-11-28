package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testwait"
)

// renamedSourceLockSeed holds the rename pair sjawhar/legion to cybermonklabs/legion; TOOLS, whose
// one deploy source names the old name, with delivery on or off (%[1]t); and OTHR, a delivery-on
// project whose one source names no repository and so reads its default repository (%[2]s).
const renamedSourceLockSeed = `
	insert into projects (key, name, default_assignee, default_repo) values
		('TOOLS', 'Tools', 'alice', 'acme/tools'), ('OTHR', 'Other', 'alice', %[2]s);
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('sjawhar/legion', 'TOOLS', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
	insert into project_delivery_settings (project_key, delivery, population_authors, excluded_repos, updated_by, updated_at) values
		('TOOLS', %[1]t, '{hubot}', '{}', '{"kind":"user","id":"alice"}', '2026-01-01T00:00:00Z'),
		('OTHR', true, '{hubot}', '{}', '{"kind":"user","id":"alice"}', '2026-01-01T00:00:00Z');
	insert into project_deploy_sources (project_key, position, kind, repo, deploy_workflow_path, production_job_names, pr_checks_workflow_path) values
		('TOOLS', 0, 'github_workflow', 'sjawhar/legion', 'ship.yml', '{ship}', 'ci.yml'),
		('OTHR', 0, 'github_workflow', null, 'deploy.yml', '{release}', 'pr.yml');
`

// deploySourcesRead names each deploy source by its project, its position and the repository it
// names, `default` for none.
const deploySourcesRead = `select string_agg(project_key || '#' || position || '=' || coalesce(repo, 'default'), ' ' order by project_key, position) from project_deploy_sources`

// sharedDeliveryRepositories names each repository that github_workflow sources of two delivery-on
// projects read, with those projects, or none.
const sharedDeliveryRepositories = `
	select coalesce(string_agg(repo || ' ' || projects, ', ' order by repo), 'none') from (
		select lower(coalesce(src.repo, p.default_repo)) as repo, string_agg(src.project_key, ',' order by src.project_key) as projects
		from project_deploy_sources src
		join projects p on p.key = src.project_key
		join project_delivery_settings s on s.project_key = src.project_key and s.delivery
		where src.kind = 'github_workflow'
		group by 1 having count(distinct src.project_key) > 1) shared`

// saveSettingsRow is a save's first write, the settings row of the project at $1 with delivery on
// (delivery.PutProjectSettings' upsert).
const saveSettingsRow = `
	insert into project_delivery_settings (project_key, delivery, population_authors, excluded_repos,
		failure_sources, failures_measured_since, updated_by)
	values ($1, true, '{hubot}', '{}', '{}', null, '{"kind":"user","id":"bob"}')
	on conflict (project_key) do update set
		delivery = excluded.delivery, updated_by = excluded.updated_by, updated_at = now()`

// heldSettingsLocks names each lock backend $1 holds of the settings tables, but the one a read of
// them takes (AccessShareLock), and of a repository (6903), or none.
const heldSettingsLocks = `
	select coalesce(string_agg(l.locktype || ' ' || coalesce(l.relation::regclass::text, l.classid::text) || ' ' || l.mode, ', '), 'none')
	from pg_locks l where l.pid = $1 and l.granted
	  and ((l.relation in ('project_delivery_settings'::regclass, 'project_deploy_sources'::regclass) and l.mode <> 'AccessShareLock')
	    or (l.locktype = 'advisory' and l.classid = 6903 and l.objsubid = 2))`

// takeProjectLock takes project's delivery lock in tx, as a save and a move of a default
// repository take it first (LockDeliveryProject): the advisory lock (6902, hashtext(project)), by
// the key the waits of these tests name.
func takeProjectLock(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(6902, hashtext($1))`, project); err != nil {
		return fmt.Errorf("lock the delivery settings of %s: %w", project, err)
	}
	return nil
}

// errShared is the refusal DELIVERY_REPOSITORY_SHARED of repo, which a source of other reads.
type errShared struct{ repo, other string }

func (e errShared) Error() string {
	return fmt.Sprintf("DELIVERY_REPOSITORY_SHARED: a source of %s, which has delivery on, reads %s", e.other, e.repo)
}

// checkSaveSources is the check that a save of project, or a move of its default repository, makes
// of the repositories its sources read, as delivery's checkSources makes it: it takes their locks
// (LockDeployRepositories), and then refuses one that a github_workflow source of another
// delivery-on project reads (delivery.RepositoryOwner) with errShared.
func checkSaveSources(ctx context.Context, tx pgx.Tx, project string, repos ...string) error {
	if err := LockDeployRepositories(ctx, tx, repos); err != nil {
		return err
	}
	for _, repo := range repos {
		var other string
		err := tx.QueryRow(ctx, `
			select src.project_key from project_deploy_sources src
			join projects p on p.key = src.project_key
			join project_delivery_settings s on s.project_key = src.project_key and s.delivery
			where src.kind = 'github_workflow' and src.project_key <> $2
			  and lower(coalesce(src.repo, p.default_repo)) = lower($1)
			order by src.project_key limit 1`, repo, project).Scan(&other)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read the project that deploys %s: %w", repo, err)
		}
		return errShared{repo: repo, other: other}
	}
	return nil
}

// otherChecksTheNewName is a save of OTHR that adds a source naming cybermonklabs/legion, under
// OTHR's lock, up to its first write: the check of the repositories its sources read, its default
// repository and the new source's.
func otherChecksTheNewName(ctx context.Context, tx pgx.Tx) error {
	return checkSaveSources(ctx, tx, "OTHR", "acme/other", "cybermonklabs/legion")
}

// otherAddsTheNewName is the rest of that save: its settings row and the new source.
func otherAddsTheNewName(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, saveSettingsRow, "OTHR"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		insert into project_deploy_sources (project_key, position, kind, repo, deploy_workflow_path, production_job_names, pr_checks_workflow_path)
		values ('OTHR', 1, 'github_workflow', 'cybermonklabs/legion', 'ship.yml', '{ship}', 'ci.yml')`)
	return err
}

// queuedLock is a lock a backend waits for, by pg_locks (locktype, the relation or the advisory
// lock's first key, and mode), and the backend.
type queuedLock struct {
	pid  uint32
	lock string
}

// waitingBehind is the lock a backend waits for behind tx's own, read in tx, with pid 0 where no
// backend does.
func waitingBehind(ctx context.Context, tx pgx.Tx) (queuedLock, error) {
	var q queuedLock
	err := tx.QueryRow(ctx, `
		select coalesce(max(l.pid), 0), coalesce(max(l.locktype || ' ' || coalesce(l.relation::regclass::text, l.classid::text) || ' ' || l.mode), '')
		from pg_locks l where not l.granted and pg_backend_pid() = any(pg_blocking_pids(l.pid))`).Scan(&q.pid, &q.lock)
	return q, err
}

// A settings save takes its project's lock and reads the project's sources under it
// (delivery.StoredSettings), then takes the lock of each repository they read
// (LockDeployRepositories) and checks that no other delivery-on project's source reads one
// (delivery.RepositoryOwner), all before its first write; a move of a project's default repository
// (delivery.MoveDefaultRepository) does the same and writes neither settings table. So the line of
// project_deploy_sources.repo takes the delivery lock of each project with a source of the old name
// (repoRenameLine.projects), then each pair's old and new name's (repoRenameLine.repositories), and
// only then the settings tables' lock, and renames the sources of the projects it locked alone.
// Over renamedSourceLockSeed, the pass runs past the line of projects.default_repo, which takes
// the names' locks too, and is held at the line before this one (delivery_settings', by
// holdTheEvalLine); a write runs up to the point its row names and the pass goes on. The pass waits
// for the write at this line, holding no write lock of the settings tables and no repository's
// lock; the write commits, and the pass, let on, deletes TOOLS's source, which the new name would
// make read a repository a source of OTHR reads (sharedSources). At the end one delivery-on
// project's source reads cybermonklabs/legion. Each row proves one lock:
//
//   - A save of OTHR that adds a source naming the new name and has checked that name's owner: the
//     pass waits for the new name's lock (6903). Red where the line takes no repository's lock: it
//     renames TOOLS's source beside the save, which then commits a second delivery-on source of
//     cybermonklabs/legion, its check made before the rename.
//   - A move of OTHR's default repository to the new name, which has checked that name's owner and
//     writes neither settings table: the pass waits for the new name's lock (6903). Red the same
//     way.
//   - A save of TOOLS, with delivery off, that turns delivery on and has read its source of the old
//     name, while OTHR's source reads the new name, its default: the pass waits for TOOLS's lock
//     (6902). The save then checks the old name, which no other source reads, and commits. Red
//     where the line takes no project's lock: it renames TOOLS's source while TOOLS has delivery
//     off, so deletes none, and the save then turns delivery on over the renamed source, its check
//     made of the old name.
func TestTheRenamePassLocksWhatASaveChecksBeforeItRenamesASource(t *testing.T) {
	for _, row := range []struct {
		name string
		// toolsDelivery and otherDefault are TOOLS's switch and OTHR's default repository
		// (renamedSourceLockSeed).
		toolsDelivery bool
		otherDefault  string
		// project is the project the write is of. checked is what the write does under that
		// project's lock before the pass goes on, and rest the rest of it up to its commit, made
		// once the pass waits.
		project       string
		checked, rest func(ctx context.Context, tx pgx.Tx) error
		// waits is the lock the pass waits for behind the write.
		waits string
		// sources is project_deploy_sources as the write and the pass leave it.
		sources string
	}{
		{
			name: "a save of OTHR adding a source of the new name", toolsDelivery: true, otherDefault: "acme/other",
			project: "OTHR", checked: otherChecksTheNewName, rest: otherAddsTheNewName,
			waits: "advisory 6903 ExclusiveLock", sources: "OTHR#0=default OTHR#1=cybermonklabs/legion",
		},
		{
			name: "a move of OTHR's default repository to the new name", toolsDelivery: true, otherDefault: "acme/other",
			project: "OTHR",
			checked: func(ctx context.Context, tx pgx.Tx) error {
				return checkSaveSources(ctx, tx, "OTHR", "acme/other", "cybermonklabs/legion")
			},
			rest: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `update projects set default_repo = 'cybermonklabs/legion' where key = 'OTHR'`)
				return err
			},
			waits: "advisory 6903 ExclusiveLock", sources: "OTHR#0=default",
		},
		{
			name: "a save of TOOLS turning its delivery on", toolsDelivery: false, otherDefault: "cybermonklabs/legion",
			project: "TOOLS",
			checked: func(ctx context.Context, tx pgx.Tx) error {
				var repo string
				if err := tx.QueryRow(ctx, `select repo from project_deploy_sources where project_key = 'TOOLS'`).Scan(&repo); err != nil {
					return err
				}
				if repo != "sjawhar/legion" {
					return fmt.Errorf("the save read TOOLS's source of %s, want sjawhar/legion", repo)
				}
				return nil
			},
			rest: func(ctx context.Context, tx pgx.Tx) error {
				if err := checkSaveSources(ctx, tx, "TOOLS", "sjawhar/legion"); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, saveSettingsRow, "TOOLS")
				return err
			},
			waits: "advisory 6902 ExclusiveLock", sources: "OTHR#0=default",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			store, pairs := openRenamedRepositoriesStore(t, fmt.Sprintf(renamedSourceLockSeed, row.toolsDelivery, QuoteLiteral(row.otherDefault)))
			logs := capturePassLogs(t)
			holder := holdTheEvalLine(t, store, "update", "delivery_settings", true)
			passed := make(chan RepoRenamePass, 1)
			go func() { passed <- RewriteRenamedRepositories(ctx, store.Pool, pairs) }()
			awaitTheHeldLine(t, holder, passed)

			write, err := store.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the write: %v", err)
			}
			t.Cleanup(func() { _ = write.Rollback(context.Background()) })
			if err := takeProjectLock(ctx, write, row.project); err != nil {
				t.Fatalf("the write's lock of %s: %v", row.project, err)
			}
			if err := row.checked(ctx, write); err != nil {
				t.Fatalf("the write under %s's lock: %v", row.project, err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("let the pass on: %v", err)
			}
			wait := testwait.Until(t, "the pass to wait for the write, or to answer", func() (queuedLock, bool) {
				select {
				case pass := <-passed:
					passed <- pass
					return queuedLock{lock: "none: the pass answered beside the write"}, true
				default:
				}
				q, err := waitingBehind(ctx, write)
				if err != nil {
					t.Fatalf("read the waits behind the write: %v", err)
				}
				return q, q.pid != 0
			})
			var held string
			if err := write.QueryRow(ctx, heldSettingsLocks, wait.pid).Scan(&held); err != nil {
				t.Fatalf("read the pass's locks: %v", err)
			}
			if wait.lock != row.waits || held != "none" {
				t.Errorf("the pass waits for the write for %q, holding %s; want it to wait for %q, holding no write lock of the settings tables and no repository's lock",
					wait.lock, held, row.waits)
			}

			done := make(chan error, 1)
			go func() {
				if err := row.rest(ctx, write); err != nil {
					_ = write.Rollback(ctx)
					done <- err
					return
				}
				done <- write.Commit(ctx)
			}()
			requireNoDeadlock(t, "the write", testwait.Receive(t, "the write", done))
			if pass := testwait.Receive(t, "the rename pass", passed); pass.Deleted != 1 || pass.Failures != 0 || pass.Left != noneLeft {
				t.Errorf("the pass = %+v (%v), want TOOLS's source deleted, no failure and nothing left; it logged %v", pass, pass.Err(), named(logs(), failedLine))
			}
			for query, want := range map[string]string{deploySourcesRead: row.sources, sharedDeliveryRepositories: "none"} {
				if got := readText(t, store, query); got != want {
					t.Errorf("%s\n= %s\nwant %s", query, got, want)
				}
			}
		})
	}
}

// The other order of TestTheRenamePassLocksWhatASaveChecksBeforeItRenamesASource's first row, over
// the same seed: the line of project_deploy_sources.repo holds TOOLS's lock, both names' and the
// settings tables', and is held at its write of TOOLS's source (holdTheEvalLine), when a save of
// OTHR that adds a source naming the new name begins. The save waits behind the line for the new
// name's lock (6903); the line, let on, renames TOOLS's source and commits; and the save then finds
// TOOLS's source reading the new name and is refused DELIVERY_REPOSITORY_SHARED, so one delivery-on
// project's source reads cybermonklabs/legion. Red where the line takes no repository's lock: the
// save checks the new name beside the held line, finds no source reading it, waits for the tables'
// lock at its first write instead, and commits after the line, a second delivery-on source of
// cybermonklabs/legion.
func TestASaveOfTheNewNameThatMeetsTheRenamedSourcesLineIsRefused(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, fmt.Sprintf(renamedSourceLockSeed, true, QuoteLiteral("acme/other")))
	logs := capturePassLogs(t)
	holder := holdTheEvalLine(t, store, "update", "project_deploy_sources", true)
	passed := make(chan RepoRenamePass, 1)
	go func() { passed <- RewriteRenamedRepositories(ctx, store.Pool, pairs) }()
	line := awaitTheHeldLine(t, holder, passed)
	var names int
	if err := holder.QueryRow(ctx, `select count(*) from pg_locks
		where pid = $1 and granted and locktype = 'advisory' and classid = 6903 and objsubid = 2`, line).Scan(&names); err != nil {
		t.Fatalf("read the line's locks: %v", err)
	}
	if names != 2 {
		t.Errorf("the line holds %d repositories' locks at its write of TOOLS's source, want 2, the pair's names'", names)
	}

	save, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settings save: %v", err)
	}
	t.Cleanup(func() { _ = save.Rollback(context.Background()) })
	var pid uint32
	if err := save.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the save's backend: %v", err)
	}
	saved := make(chan error, 1)
	// The save's statements in PutProjectSettings' order, its read of the stored settings left out:
	// the project's lock, the check of its repositories under their locks, then its writes. A
	// refused save rolls back.
	go func() {
		err := func() error {
			if err := takeProjectLock(ctx, save, "OTHR"); err != nil {
				return err
			}
			if err := otherChecksTheNewName(ctx, save); err != nil {
				return err
			}
			if err := otherAddsTheNewName(ctx, save); err != nil {
				return err
			}
			return save.Commit(ctx)
		}()
		if err != nil {
			_ = save.Rollback(ctx)
		}
		saved <- err
	}()
	waits := testwait.Until(t, "the save to wait beside the line, or to answer", func() (string, bool) {
		select {
		case err := <-saved:
			saved <- err
			return "none: the save answered beside the line", true
		default:
		}
		var lock string
		if err := holder.QueryRow(ctx, `
			select coalesce(max(locktype || ' ' || coalesce(relation::regclass::text, classid::text) || ' ' || mode), '')
			from pg_locks where pid = $1 and not granted`, pid).Scan(&lock); err != nil {
			t.Fatalf("read the save's waits: %v", err)
		}
		return lock, lock != ""
	})
	if want := "advisory 6903 ExclusiveLock"; waits != want {
		t.Errorf("the save waits beside the line for %q, want %q, the new name's lock the line holds", waits, want)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the line on: %v", err)
	}

	var refused errShared
	if err := testwait.Receive(t, "the settings save", saved); !errors.As(err, &refused) || refused.other != "TOOLS" || refused.repo != "cybermonklabs/legion" {
		t.Errorf("the settings save answered %v, want DELIVERY_REPOSITORY_SHARED naming TOOLS's source of cybermonklabs/legion", err)
	}
	if pass := testwait.Receive(t, "the rename pass", passed); pass.Deleted != 0 || pass.Failures != 0 || pass.Left != noneLeft {
		t.Errorf("the pass = %+v (%v), want nothing deleted, no failure and nothing left; it logged %v", pass, pass.Err(), named(logs(), failedLine))
	}
	for query, want := range map[string]string{
		deploySourcesRead:          "OTHR#0=default TOOLS#0=cybermonklabs/legion",
		sharedDeliveryRepositories: "none",
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}
}
