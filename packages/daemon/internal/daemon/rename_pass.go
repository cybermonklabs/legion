package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The lines the rename pass logs. Each line of one issue or one read names the old name and the
// new one it acts for; the table line counts every pair, and its `left` holds, by each pair's
// `from`, the rows that still name it once the pass has run, the count dispatch://INFRA-4412 waits
// on to be zero before the pair can leave `repository_renames`.
const (
	renameTableLine   = "rename pass: wrote each configured rename's new name over its old one on the stored rows of a table"
	renameKeptLine    = "rename pass: kept an issue's stored repository names as they are, since its pod may be live"
	renameDeletedLine = "rename pass: deleted an issue's pull request row, since another issue's row holds its branch under the new name"
	renameFailedLine  = "rename pass: the write of an issue's stored repository names failed; the next boot or pod end writes them again"
	renameUnreadLine  = "rename pass: the stored rows that name a rename's old name could not be read; the next boot or pod end reads them again"
)

// renamePass is this release's one-time rename pass (dispatch://INFRA-4401 A10): for each pair of
// `repository_renames`, it writes the pair's `to` over its `from` on every stored row of the
// project's issues that names the `from` (record.RenameIssueRepository), so that once it has run
// no record names the old repository and the pair can go. dispatch://INFRA-3532's own change
// removes it. It runs at boot over every issue of the project (workflowRuntime.renameAtBoot) and
// at the end of an issue's pod over that issue alone (workflowRuntime.podEndsOn), and is
// idempotent: a row it wrote names the `to`, which no pair's `from` matches, so a second run
// writes nothing.
//
// Each issue is written in one transaction of its own, under the lock every fact applies under
// (intake.SerializeFacts), so no fact applies with a copy of the issue read before the pass wrote
// it. An issue whose pod may still be live is left as it is (keep), so a role launched into that
// pod keeps the workspace the pod made under the old name; the issue's next pod starts under the
// new one. A write that fails is logged with its error and changes nothing else: the pass goes on
// with the next issue, and the next boot or pod end writes the rows again. Nothing it meets stops
// the boot.
type renamePass struct {
	pool    *store.Pool
	project string
	renames ghrepo.Renames
	// keep is the live-pod rule: why issue's rows are left as they are, "" for none, asked with
	// each spelling of the old name its rows hold and the pair's own (workspace.Location keeps
	// the case it is handed). Nil keeps nothing: the end of a pod that the runtime proved gone.
	keep renameKeep
	log  *slog.Logger
}

// renameKeep is a rename pass's live-pod rule (renamePass.keep).
type renameKeep func(issue string, names []string) string

// issuePodPresence is what the rename pass asks a runtime that runs a pod per issue: whether it
// holds the issue's Sandbox, and which pod that Sandbox holds, or that it cannot say
// (runtime.IssuePodReader). The Kubernetes runtime answers both from its informers' stores, which
// sandbox.New syncs before it returns (sandbox.Runtime.IssueSandbox, IssuePod). tmux implements
// neither: it runs no pod.
type issuePodPresence interface {
	IssueSandbox(issue string) (name string, present bool)
	runtime.IssuePodReader
}

// bootKeep is the live-pod rule at boot. Under a runtime that runs a pod per issue it keeps an
// issue while the runtime holds its Sandbox or its pod, or cannot say whether it holds a pod of
// it; under tmux, which runs no pod, it keeps an issue while a workspace directory of the issue is
// under the old name (workspaceKeep, INFRA-4401 P2).
func bootKeep(rt runtime.Runtime, stateDir string) renameKeep {
	pods, ok := rt.(issuePodPresence)
	if !ok {
		return workspaceKeep(stateDir)
	}
	return func(issue string, _ []string) string {
		if name, present := pods.IssueSandbox(issue); present {
			return "the runtime holds its Sandbox " + name
		}
		pod, read := pods.IssuePod(issue)
		switch {
		case !read:
			return "the runtime cannot say whether it holds a pod of the issue"
		case pod != "":
			return "the runtime holds its pod " + pod
		}
		return ""
	}
}

// podEndKeep is the live-pod rule at the end of an issue's pod. Under a runtime that runs a pod per
// issue it keeps nothing: the hook that runs the pass is the proof that no pod of the issue is
// live (SuspendIssue's nil return, which comes once the pod and its Sandbox are gone, or the tree's
// cleanup's, which waits until no Sandbox of the tree is left), and the informer's store, which
// may still hold the Sandbox for a moment, is not read. Under tmux, whose tree cleanup does nothing
// and which suspends no issue, P2's workspace-directory rule decides (workspaceKeep).
func podEndKeep(rt runtime.Runtime, stateDir string) renameKeep {
	if _, ok := rt.(issuePodPresence); ok {
		return nil
	}
	return workspaceKeep(stateDir)
}

// workspaceKeep is the live-pod rule of a daemon whose runtime works each issue in a workspace
// directory on its own host (INFRA-4401 P2): the directory is named by the repository's owner and
// name (workspace.Location), so an issue is kept while a workspace directory of it is under any of
// the names asked, and a directory the daemon cannot read keeps it too. A later boot writes it once
// the directory is gone (the linger's removal).
func workspaceKeep(stateDir string) renameKeep {
	return func(issue string, names []string) string {
		for _, name := range names {
			repo, err := ghrepo.Parse("a name the rename pass reads", name)
			if err != nil {
				continue
			}
			location, err := workspace.Location(stateDir, repo, issue)
			if err != nil {
				continue
			}
			_, err = os.Stat(location.Dir)
			switch {
			case err == nil:
				return "its workspace directory " + location.Dir + " is under the old name"
			case !errors.Is(err, fs.ErrNotExist):
				return fmt.Sprintf("its workspace directory %s under the old name cannot be read: %v", location.Dir, err)
			}
		}
		return ""
	}
}

// renameTally is what the pass did on one table: the rows it wrote, the rows whose write failed,
// and the rows still on each pair's `from` once it ran, by that `from`.
type renameTally struct {
	written, failed int
	left            map[string]int
}

// run runs the pass over every issue of the project whose rows name a pair's `from`, or over keys
// alone when keys is not nil, and answers whether it wrote a row. It logs one line per table
// (renameTableLine) whenever it runs at boot, and at the end of a pod only where the pod's issues
// named a `from`, so that every pod end after the stored names are rewritten logs nothing.
func (p renamePass) run(ctx context.Context, keys []string) (wrote bool) {
	pairs := p.renames.Pairs()
	if len(pairs) == 0 {
		return false
	}
	tallies := make(map[string]*renameTally, len(record.RenameTables))
	for _, table := range record.RenameTables {
		tallies[table] = &renameTally{left: map[string]int{}}
	}
	named := false
	for _, pair := range pairs {
		from, to := pair.From.String(), pair.To.String()
		candidates, err := p.candidates(ctx, from, keys)
		if err != nil {
			if ctx.Err() != nil {
				return wrote
			}
			p.log.Warn(renameUnreadLine, "from", from, "to", to, "error", err)
			continue
		}
		named = named || len(candidates) > 0
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				return wrote
			}
			if p.rename(ctx, candidate, from, to, tallies) {
				wrote = true
			}
		}
	}
	if keys != nil && !named {
		return wrote
	}
	for _, pair := range pairs {
		from := pair.From.String()
		left, err := p.left(ctx, from)
		if err != nil {
			if ctx.Err() != nil {
				return wrote
			}
			p.log.Warn(renameUnreadLine, "from", from, "to", pair.To.String(), "error", err)
			continue
		}
		for table, rows := range left {
			tallies[table].left[from] = rows
		}
	}
	for _, table := range record.RenameTables {
		tally := tallies[table]
		p.log.Info(renameTableLine, "table", table, "written", tally.written, "failed", tally.failed, "left", tally.left)
	}
	return wrote
}

// rename writes to over from on candidate's rows unless keep keeps them, counting what it did on
// tallies, and answers whether it wrote or deleted a row.
func (p renamePass) rename(ctx context.Context, candidate record.RenameCandidate, from, to string, tallies map[string]*renameTally) bool {
	if p.keep != nil {
		names := candidate.Spellings
		if !slices.Contains(names, from) {
			names = append(slices.Clone(names), from)
		}
		if why := p.keep(candidate.Issue, names); why != "" {
			p.log.Info(renameKeptLine, "issue", candidate.Issue, "from", from, "to", to, "reason", why)
			return false
		}
	}
	written, err := p.write(ctx, candidate.Issue, from, to)
	if err != nil {
		p.log.Warn(renameFailedLine, "issue", candidate.Issue, "from", from, "to", to, "error", err)
		for table, rows := range candidate.Rows {
			tallies[table].failed += rows
		}
		return false
	}
	wrote := false
	for table, rows := range written.Rows {
		tallies[table].written += rows
		wrote = wrote || rows > 0
	}
	if deleted := written.Deleted; deleted != nil {
		wrote = true
		p.log.Warn(renameDeletedLine, "issue", deleted.Issue, "kept", deleted.Holder, "repo", deleted.Repo, "to", deleted.To,
			"branch", deleted.Branch, "number", deleted.Number)
	}
	return wrote
}

// candidates is each issue of the project, or of keys, whose rows name from
// (record.RenameCandidates), read in a transaction of its own bounded by bootTimeout.
func (p renamePass) candidates(ctx context.Context, from string, keys []string) ([]record.RenameCandidate, error) {
	read, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	var candidates []record.RenameCandidate
	err := pgx.BeginFunc(read, p.pool, func(tx pgx.Tx) error {
		var err error
		candidates, err = record.RenameCandidates(read, tx, p.project, from, keys)
		return err
	})
	return candidates, err
}

// write writes to over from on issue's rows (record.RenameIssueRepository) in one transaction
// under the lock every fact applies under, bounded by bootTimeout.
func (p renamePass) write(ctx context.Context, issue, from, to string) (record.RenameWrite, error) {
	attempt, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	var written record.RenameWrite
	err := pgx.BeginFunc(attempt, p.pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(attempt, tx); err != nil {
			return fmt.Errorf("serialize the rename of %s with every fact: %w", issue, err)
		}
		var err error
		written, err = record.RenameIssueRepository(attempt, tx, issue, from, to)
		return err
	})
	return written, err
}

// left is how many rows of each table of the project still name from (record.RenameLeft), read
// in a transaction of its own bounded by bootTimeout.
func (p renamePass) left(ctx context.Context, from string) (map[string]int, error) {
	read, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	var left map[string]int
	err := pgx.BeginFunc(read, p.pool, func(tx pgx.Tx) error {
		var err error
		left, err = record.RenameLeft(read, tx, p.project, from)
		return err
	})
	return left, err
}

// renamePass is the rename pass over the project's stored records under keep, for the pairs the
// deployment configures (repositories.renames).
func (w *workflowRuntime) renamePass(keep renameKeep) renamePass {
	var renames ghrepo.Renames
	if w.repositories != nil {
		renames = w.repositories.renames
	}
	log := w.log
	if log == nil {
		log = slog.Default()
	}
	return renamePass{pool: w.pool, project: w.dispatchProject, renames: renames, keep: keep, log: log}
}

// renameAtBoot runs the rename pass over every issue of the project at boot, under rt's live-pod
// rule (bootKeep): after supervision opened and the worker image passed its probe, so the runtime
// answers for the pods it holds, and before the consumers open, so no fact and no launch reads a
// record while the pass writes it. After a pass that wrote a row, the refresher reads the live
// records again (wakeLive), so the cache set and the heard set name the new names.
func (w *workflowRuntime) renameAtBoot(ctx context.Context, rt runtime.Runtime) {
	if w.renamePass(bootKeep(rt, w.stateDir)).run(ctx, nil) {
		w.wakeLive()
	}
}

// podEndsOn is what the end of issues' pods under rt runs (outbox.podEnded): it drops the
// repository kept for each of them (dropLaunchedRepository), so the next launch reads it again,
// and runs the rename pass over them under podEndKeep's rule, waking the refresher's read of the
// live records (wakeLive) after a pass that wrote a row.
func (w *workflowRuntime) podEndsOn(rt runtime.Runtime) func(context.Context, []string) {
	pass := w.renamePass(podEndKeep(rt, w.stateDir))
	return func(ctx context.Context, issues []string) {
		for _, issue := range issues {
			w.dropLaunchedRepository(issue)
		}
		if pass.run(ctx, issues) {
			w.wakeLive()
		}
	}
}
