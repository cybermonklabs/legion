package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The lines the rename pass logs. Each line of one issue or one read names the old name and the
// new one it acts for; the table line counts every pair, and its `left` holds, by each pair's
// `from`, the rows that still name it once the pass has run, the count dispatch://INFRA-4412 waits
// on to be zero before the pair can leave `repository_renames`. The line of a pair the pass leaves
// names the pair as `legion.yaml` spells it and why GitHub does not show its transfer.
const (
	renameTableLine   = "rename pass: wrote each configured rename's new name over its old one on the stored rows of a table"
	renameLeftLine    = "rename pass: left a configured rename's stored names as they are, since GitHub does not show its transfer yet; a later boot or pod end writes them once it does"
	renameKeptLine    = "rename pass: kept an issue's stored repository names as they are, since its pod may be live"
	renameDeletedLine = "rename pass: deleted an issue's pull request row, since another issue's row holds its branch under the new name"
	renameFailedLine  = "rename pass: the write of an issue's stored repository names failed; the next boot or pod end writes them again"
	renameUnreadLine  = "rename pass: the stored rows that name a rename's old name could not be read; the next boot or pod end reads them again"
)

// renamePass is this release's one-time rename pass (dispatch://INFRA-4401 A10): for each pair of
// `repository_renames` whose transfer GitHub shows, it writes the pair's `to`, in GitHub's
// spelling, over its `from` on every stored row of the project's issues that names the `from`
// (record.RenameIssueRepository), so that once it has run no record names the old repository and
// the pair can go. dispatch://INFRA-3532's own change removes it. It runs at boot over every issue
// of the project (workflowRuntime.renameAtBoot) and at the end of an issue's pod over that issue
// alone (workflowRuntime.podEndsOn), and is idempotent: a row it wrote names the `to`, which no
// pair's `from` matches, so a second run writes nothing.
//
// A pair may be configured before its transfer (a pair serves no repository of its own), and a
// record rewritten then would address a repository GitHub does not hold yet, which nothing could
// undo, since the pass matches only the `from`. So the pass writes a pair only once GitHub shows
// that the transfer has happened (transferred), and leaves every other pair as it is, with one
// line saying why, for a later pass to write.
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
	// transferred is whether GitHub shows pair's transfer (workflowRuntime.renameTransfer):
	// GitHub's spelling of the pair's `to`, which the pass writes, and otherwise why the pair is
	// left as it is, "" where it is not.
	transferred func(ctx context.Context, pair ghrepo.Rename) (to ghrepo.Repository, left string)
	// keep is the live-pod rule: why issue's rows are left as they are, "" for none, asked with
	// each spelling of the old name its rows hold and the pair's own (workspace.Location keeps
	// the case it is handed). Nil keeps nothing: the end of a pod that the runtime proved gone.
	keep renameKeep
	log  *slog.Logger
}

// renameKeep is a rename pass's live-pod rule (renamePass.keep).
type renameKeep func(issue string, names []string) string

// bootKeep is the live-pod rule at boot. Under a runtime that runs a pod per issue (issuePods, the
// one question every rule of the daemon asks a runtime of that) it keeps an issue while the runtime
// holds its Sandbox or its pod, or cannot say whether it holds a pod of it: the Kubernetes runtime
// answers both from its informers' stores, which sandbox.New syncs before it returns. Under tmux,
// which runs no pod, it keeps an issue while a workspace directory of the issue is under the old
// name (workspaceKeep, INFRA-4401 P2).
func bootKeep(rt runtime.Runtime, stateDir string) renameKeep {
	pods, ok := rt.(issuePods)
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
// issue (issuePods) it keeps nothing: the hook that runs the pass is the proof that no pod of the
// issue is live (SuspendIssue's nil return, which comes once the pod and its Sandbox are gone, or
// the tree's cleanup's, which waits until no Sandbox of the tree is left), and the informer's
// store, which may still hold the Sandbox for a moment, is not read. Under tmux, whose tree
// cleanup does nothing and which suspends no issue, P2's workspace-directory rule decides
// (workspaceKeep).
func podEndKeep(rt runtime.Runtime, stateDir string) renameKeep {
	if _, ok := rt.(issuePods); ok {
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
			dir, err := workspaceDir(stateDir, repo, issue)
			switch {
			case dir == "":
				// workspace.Location names no directory for the name, so none keeps the issue.
			case err == nil:
				return "its workspace directory " + dir + " is under the old name"
			case !errors.Is(err, fs.ErrNotExist):
				return fmt.Sprintf("its workspace directory %s under the old name cannot be read: %v", dir, err)
			}
		}
		return ""
	}
}

// renameTally is what the pass did on one table: the rows it wrote, the rows whose write failed,
// and the rows still on each pair's `from` once it ran, by that `from`, which every pair's
// candidates read again over the whole project count (record.RenameCandidates).
type renameTally struct {
	written, failed int
	left            map[string]int
}

// run runs the pass over every issue of the project whose rows name a pair's `from`, or over keys
// alone when keys is not nil, and answers whether it wrote a row. Each pair is decided before its
// rows are read (transferred): a pair whose transfer GitHub shows is written in GitHub's spelling
// of its `to`, and every other pair is left as it is, with one line naming it and why
// (renameLeftLine). It logs that line, and one line per table (renameTableLine), whenever it runs
// at boot, and at the end of a pod only where the pod's issues named a `from`, so that every pod
// end after the stored names are rewritten logs nothing.
func (p renamePass) run(ctx context.Context, keys []string) (wrote bool) {
	pairs := p.renames.Pairs()
	if len(pairs) == 0 {
		return false
	}
	tallies := make(map[record.RenameTable]*renameTally, len(record.RenameTables))
	for _, table := range record.RenameTables {
		tallies[table] = &renameTally{left: map[string]int{}}
	}
	named := false
	for _, pair := range pairs {
		from := pair.From.String()
		to, left := p.transferred(ctx, pair)
		if ctx.Err() != nil {
			return wrote
		}
		leave := func() { p.log.Warn(renameLeftLine, "from", from, "to", pair.To.String(), "reason", left) }
		if left != "" && keys == nil {
			leave()
			continue
		}
		candidates, err := p.candidates(ctx, from, keys)
		if err != nil {
			if ctx.Err() != nil {
				return wrote
			}
			p.log.Warn(renameUnreadLine, "from", from, "to", pair.To.String(), "error", err)
			continue
		}
		named = named || len(candidates) > 0
		if left != "" {
			if len(candidates) > 0 {
				leave()
			}
			continue
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				return wrote
			}
			if p.rename(ctx, candidate, from, to.String(), tallies) {
				wrote = true
			}
		}
	}
	if keys != nil && !named {
		return wrote
	}
	for _, pair := range pairs {
		from := pair.From.String()
		left, err := p.candidates(ctx, from, nil)
		if err != nil {
			if ctx.Err() != nil {
				return wrote
			}
			p.log.Warn(renameUnreadLine, "from", from, "to", pair.To.String(), "error", err)
			continue
		}
		for _, table := range record.RenameTables {
			tallies[table].left[from] = 0
		}
		for _, candidate := range left {
			for table, rows := range candidate.Rows {
				tallies[table].left[from] += rows
			}
		}
	}
	for _, table := range record.RenameTables {
		tally := tallies[table]
		p.log.Info(renameTableLine, "table", string(table), "written", tally.written, "failed", tally.failed, "left", tally.left)
	}
	return wrote
}

// rename writes to over from on candidate's rows unless keep keeps them, counting what it did on
// tallies, and answers whether it wrote or deleted a row.
func (p renamePass) rename(ctx context.Context, candidate record.RenameCandidate, from, to string, tallies map[record.RenameTable]*renameTally) bool {
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
// under the lock every fact applies under (serializedTx), bounded by bootTimeout.
func (p renamePass) write(ctx context.Context, issue, from, to string) (record.RenameWrite, error) {
	attempt, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	var written record.RenameWrite
	err := serializedTx(attempt, p.pool, "serialize the rename of "+issue+" with every fact", func(tx pgx.Tx) error {
		var err error
		written, err = record.RenameIssueRepository(attempt, tx, issue, from, to)
		return err
	})
	return written, err
}

// renamePass is the rename pass over the project's stored records under keep, for the pairs the
// deployment configures (repositories.renames), each written once GitHub shows its transfer
// (renameTransfer).
func (w *workflowRuntime) renamePass(keep renameKeep) renamePass {
	var renames ghrepo.Renames
	if w.repositories != nil {
		renames = w.repositories.renames
	}
	log := w.log
	if log == nil {
		log = slog.Default()
	}
	return renamePass{pool: w.pool, project: w.dispatchProject, renames: renames, transferred: w.renameTransfer, keep: keep, log: log}
}

// renameTransfer is whether GitHub shows that pair's transfer has happened, the rename pass's
// question of each pair before it reads the pair's rows (renamePass.transferred): both role Apps
// answered installed on the pair's `to` and neither on its `from`, each name read under its own
// name and following no redirect (readInstallations), which is the rename rule's own question
// (servesLocked) once GitHub has answered both. The answers are the repository state's where it
// holds them (entryOf); a name it holds no answer for yet — at boot, where the pass runs before the
// refresher's first read of the pairs — the pass reads itself and keeps in the state as the
// refresher would (readPairNames).
//
// It answers GitHub's spelling of the `to`, which the pass writes on every column: a record's
// GitHub spelling is what GitHub intake builds its case-sensitive subjects from
// (record.Issue.GitHubRepo), and a pair compares names without case, so `legion.yaml` may spell the
// `to` as nothing publishes under. The spelling is the one the state records for the `to`
// (recordRepository), else GitHub's read of its name and default branch (defaultBranches.Named),
// bounded by installReadAttempt.
//
// Otherwise it answers why the pair is left as it is: a read that did not answer, a role App not
// installed on the `to`, one still installed on the `from` — a pair configured before its transfer
// — or a read of the `to`'s name that did not answer or named another repository. A later pass
// asks again: the next pod end, of what the state holds then, or the next boot.
func (w *workflowRuntime) renameTransfer(ctx context.Context, pair ghrepo.Rename) (ghrepo.Repository, string) {
	var unread []ghrepo.Repository
	for _, name := range []ghrepo.Repository{pair.From, pair.To} {
		if !w.repositories.entryOf(name.String()).answered() {
			unread = append(unread, name)
		}
	}
	w.readPairNames(ctx, unread)
	unanswered := func(name ghrepo.Repository, held repositoryEntry) string {
		why := "GitHub has not answered both role Apps' installation reads of " + name.String()
		if held.readError != "" {
			why += ": " + held.readError
		}
		return why
	}
	from, to := w.repositories.entryOf(pair.From.String()), w.repositories.entryOf(pair.To.String())
	switch {
	case !to.answered():
		return ghrepo.Repository{}, unanswered(pair.To, to)
	case !from.answered():
		return ghrepo.Repository{}, unanswered(pair.From, from)
	case !to.served():
		return ghrepo.Repository{}, appauth.NotInstalledOn(to.missing(), pair.To.String())
	case len(from.missing()) != len(appauth.Roles):
		return ghrepo.Repository{}, stillInstalledOn(from.appInstallations, pair.From.String())
	}
	spelled := to.recorded.Name
	if !strings.EqualFold(spelled.String(), pair.To.String()) {
		attempt, cancel := context.WithTimeout(ctx, installReadAttempt)
		read, err := w.defaultBranches.Named(attempt, pair.To)
		cancel()
		switch {
		case err != nil:
			return ghrepo.Repository{}, fmt.Sprintf("the name and default branch of %s could not be read: %v", pair.To, err)
		case !strings.EqualFold(read.Name.String(), pair.To.String()):
			return ghrepo.Repository{}, fmt.Sprintf("GitHub names %s as %s, another repository", pair.To, read.Name)
		}
		spelled = read.Name
	}
	return spelled, ""
}

// stillInstalledOn is the sentence naming each role App GitHub answered is installed on from, a
// configured pair's `from`, where a transfer leaves none: "the implement and review Apps are still
// installed on sjawhar/legion", or one App alone.
func stillInstalledOn(apps appInstallations, from string) string {
	var installed []string
	for _, role := range appauth.Roles {
		if apps.of(role).installed {
			installed = append(installed, string(role))
		}
	}
	if len(installed) == 1 {
		return "the " + installed[0] + " App is still installed on " + from
	}
	return "the " + strings.Join(installed, " and ") + " Apps are still installed on " + from
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
