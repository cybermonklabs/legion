package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"time"

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
	renameKeptLine    = "rename pass: kept an issue's stored repository names as they are, since its workspace may still be under the old name"
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
// line saying why, for a later pass to write. It asks that of a pair only while a row of the
// issues it runs over still names the pair's `from` (run), so a pass with nothing to write has no
// transfer to prove and reads nothing of GitHub.
//
// Each issue is written in one transaction of its own, under the lock every fact applies under
// (intake.SerializeFacts), so no fact applies with a copy of the issue read before the pass wrote
// it. An issue whose pod may still be live is left as it is (keep), so a role launched into that
// pod keeps the workspace the pod made under the old name; the issue's next pod starts under the
// new one. An issue whose volume the runtime keeps for its next pod is left as it is too, since
// that pod mounts the volume with its workspace under the old name (podEndKeep). A write that
// fails is logged with its error and changes nothing else: the pass goes on with the next issue,
// and the next boot or pod end writes the rows again. Nothing it meets stops the boot.
type renamePass struct {
	pool    *store.Pool
	project string
	renames ghrepo.Renames
	// transferred is whether GitHub shows pair's transfer (workflowRuntime.renameTransfer):
	// GitHub's spelling of the pair's `to`, which the pass writes, and otherwise why the pair is
	// left as it is, "" where it is not. run asks it only of a pair whose `from` a row it read
	// still names (candidates).
	transferred func(ctx context.Context, pair ghrepo.Rename) (to ghrepo.Repository, left string)
	// keep is the live-pod rule: why issue's rows are left as they are, "" for none, asked with
	// each spelling of the old name its rows hold and the pair's own (workspace.Location keeps
	// the case it is handed).
	keep renameKeep
	// written is the hook each write that wrote or deleted a row registers on its commit
	// (store.OnCommit): the refresher's read of the live records (wakeLive), so the cache set and
	// the heard set name the new names. A write whose reply is lost and that the store says
	// committed still wakes it, and its retry, which finds no row on the `from`, wakes nothing.
	written func()
	log     *slog.Logger
}

// renameKeep is a rename pass's live-pod rule (renamePass.keep).
type renameKeep func(issue string, names []string) string

// renameKeptVolume is why both live-pod rules of a runtime that runs a pod per issue keep an issue
// whose volume the runtime keeps for the issue's next pod (runtime.IssueVolumeReader).
const renameKeptVolume = "the runtime keeps its volume for the issue's next pod"

// bootKeep is the live-pod rule at boot. Under a runtime that runs a pod per issue (issuePods, the
// one question every rule of the daemon asks a runtime of that) it keeps an issue while the runtime
// holds its Sandbox, keeps its volume for the issue's next pod (IssueKeepsVolume, as podEndKeep
// does), holds its pod, or cannot say whether it holds a pod of it: the Kubernetes runtime answers
// each from its informers' stores, which sandbox.New syncs before it returns. The volume is asked
// on its own, because the Kubernetes runtime answers the Sandbox an idle release keeps under
// keep_volume absent (sandbox.Runtime.IssueSandbox), and that Sandbox holds no pod. Under tmux,
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
		if pods.IssueKeepsVolume(issue, time.Now()) {
			return renameKeptVolume
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

// podEndKeep is the live-pod rule at the end of an issue's pod. The hook that runs the pass is the
// proof that no pod of the issue is live. A close's teardown (SuspendIssue) deletes the issue's
// Sandbox, and so does a tree's cleanup, which waits until no Sandbox of the tree is left. An idle
// release's teardown (ReleaseIssue) deletes it too, but under keep_volume it keeps a Sandbox that
// owns the issue's kept volume, Suspended, with that volume, for the issue's next pod. That pod
// mounts the volume, and the workspace directory on it is under the name the record holds now
// (workspace.Location): with the new name written, the pod would make a fresh workspace under the
// new name and leave the work under the old one. So under a runtime that runs a pod per issue
// (issuePods) the rule keeps an issue on its old name while the runtime keeps its volume for the
// issue's next pod (IssueKeepsVolume), and keeps no other issue. A later pass writes such an issue:
// the pass of its close, once SuspendIssue has deleted the Sandbox and its volume, or a later boot
// that finds no kept volume. The informer's store can still hold a deleted kept Sandbox for a
// moment, so the pass of a close under keep_volume may leave the write to the next boot, which
// errs toward keeping.
//
// The rule does not ask whether the runtime holds the Sandbox (IssueSandbox). That answers a
// Sandbox this runtime's own delete took absent at once, but the informer's store can still hold a
// Sandbox that another hand deleted outright, which it answers present until the watch delivers
// the deletion, and a keep on that Sandbox would hold the write until the next boot.
// IssueKeepsVolume reads the same stored Sandbox, but it is true only for one that owns a kept
// volume, so such a deleted Sandbox keeps nothing. Under tmux, whose tree cleanup does nothing and
// which suspends no issue, P2's workspace-directory rule decides (workspaceKeep).
func podEndKeep(rt runtime.Runtime, stateDir string) renameKeep {
	pods, ok := rt.(issuePods)
	if !ok {
		return workspaceKeep(stateDir)
	}
	return func(issue string, _ []string) string {
		if pods.IssueKeepsVolume(issue, time.Now()) {
			return renameKeptVolume
		}
		return ""
	}
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
// alone when keys is not nil. Each pair's rows are read first (candidates), and a pair is decided
// (transferred) only where a row of them still names its `from`, at boot and at the end of a pod
// alike: for a pair no row names, the pass sends GitHub no request and logs no line of its own, so
// once no row names a pair's `from`, no pass reads GitHub for that pair. A pair whose transfer
// GitHub shows is written in GitHub's spelling of its `to`, and every other pair is left as it is,
// with one line naming it and why (renameLeftLine). It logs one line per table (renameTableLine)
// whenever it runs at boot, and at the end of a pod only where the pod's issues named a `from`, so
// that every pod end after the stored names are rewritten logs nothing.
func (p renamePass) run(ctx context.Context, keys []string) {
	pairs := p.renames.Pairs()
	if len(pairs) == 0 {
		return
	}
	tallies := make(map[record.RenameTable]*renameTally, len(record.RenameTables))
	for _, table := range record.RenameTables {
		tallies[table] = &renameTally{left: map[string]int{}}
	}
	named := false
	for _, pair := range pairs {
		from := pair.From.String()
		candidates, err := p.candidates(ctx, from, keys)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Warn(renameUnreadLine, "from", from, "to", pair.To.String(), "error", err)
			continue
		}
		if len(candidates) == 0 {
			continue
		}
		named = true
		to, left := p.transferred(ctx, pair)
		if ctx.Err() != nil {
			return
		}
		if left != "" {
			p.log.Warn(renameLeftLine, "from", from, "to", pair.To.String(), "reason", left)
			continue
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				return
			}
			p.rename(ctx, candidate, from, to.String(), tallies)
		}
	}
	if keys != nil && !named {
		return
	}
	for _, pair := range pairs {
		from := pair.From.String()
		left, err := p.candidates(ctx, from, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
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
}

// rename writes to over from on candidate's rows unless keep keeps them, counting what it did on
// tallies from what the write's transaction answered, never from a hook, so a hook the watcher runs
// later changes no count of the pass.
func (p renamePass) rename(ctx context.Context, candidate record.RenameCandidate, from, to string, tallies map[record.RenameTable]*renameTally) {
	names := candidate.Spellings
	if !slices.Contains(names, from) {
		names = append(slices.Clone(names), from)
	}
	if why := p.keep(candidate.Issue, names); why != "" {
		p.log.Info(renameKeptLine, "issue", candidate.Issue, "from", from, "to", to, "reason", why)
		return
	}
	written, err := p.write(ctx, candidate.Issue, from, to)
	if err != nil {
		p.log.Warn(renameFailedLine, "issue", candidate.Issue, "from", from, "to", to, "error", err)
		for table, rows := range candidate.Rows {
			tallies[table].failed += rows
		}
		return
	}
	for table, rows := range written.Rows {
		tallies[table].written += rows
	}
}

// candidates is each issue of the project, or of keys, whose rows name from
// (record.RenameCandidates), read in a read-only transaction of its own bounded by bootTimeout.
func (p renamePass) candidates(ctx context.Context, from string, keys []string) ([]record.RenameCandidate, error) {
	read, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	var candidates []record.RenameCandidate
	err := store.TransactWith(read, p.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		candidates, err = record.RenameCandidates(ctx, tx, p.project, from, keys)
		return err
	})
	return candidates, err
}

// write writes to over from on issue's rows (record.RenameIssueRepository) in one transaction
// under the lock every fact applies under (serializedTx), bounded by bootTimeout. The line of a
// pull request row it deleted, and the written hook, are hooks of its commit.
func (p renamePass) write(ctx context.Context, issue, from, to string) (record.RenameWrite, error) {
	attempt, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	var written record.RenameWrite
	err := serializedTx(attempt, p.pool, "serialize the rename of "+issue+" with every fact", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if written, err = record.RenameIssueRepository(ctx, tx, issue, from, to); err != nil {
			return err
		}
		wrote := written.Deleted != nil
		for _, rows := range written.Rows {
			wrote = wrote || rows > 0
		}
		if !wrote {
			return nil
		}
		deleted := written.Deleted
		store.OnCommit(ctx, func() {
			if deleted != nil {
				p.log.Warn(renameDeletedLine, "issue", deleted.Issue, "kept", deleted.Holder, "repo", deleted.Repo, "to", deleted.To,
					"branch", deleted.Branch, "number", deleted.Number)
			}
			p.written()
		})
		return nil
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
	return renamePass{pool: w.pool, project: w.dispatchProject, renames: renames, transferred: w.renameTransfer, keep: keep, written: w.wakeLive, log: log}
}

// renameTransfer is whether GitHub shows that pair's transfer has happened, the rename pass's
// question of a pair once it has read the pair's rows and a row still names the pair's `from`
// (renamePass.transferred, which run asks of no other pair): both role Apps answered installed on
// the pair's `to` and neither on its `from`, each name read under its own name and following no
// redirect (readInstallations), which is the rename rule's own question (servesLocked) once GitHub
// has answered both. The answers are the repository state's where it holds them (entryOf); a name
// it holds no answer for yet — at boot, where the pass runs before the refresher's first read of
// the pairs — the pass reads itself and keeps in the state as the refresher would (readPairNames).
//
// It answers GitHub's spelling of the `to`, which the pass writes on every column: a record's
// GitHub spelling is what GitHub intake builds its case-sensitive subjects from
// (record.Issue.GitHubRepo), and a pair compares names without case, so `legion.yaml` may spell the
// `to` as nothing publishes under. The spelling is the one the state records for the `to`
// (recordRepository), else GitHub's read of its name and default branch (readName), which mints an
// implement App token for the `to` owner.
//
// Otherwise it answers why the pair is left as it is: a read that did not answer, a role App not
// installed on the `to`, one still installed on the `from` — a pair configured before its transfer
// — or a read of the `to`'s name that did not answer or named another repository. A later pass
// asks again: the end of a pod whose issue's rows name the `from`, of what the state holds then,
// or the next boot.
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
		read, why, _ := w.readName(ctx, pair.To, false)
		switch {
		case why != "":
			return ghrepo.Repository{}, why
		case !strings.EqualFold(read.Name.String(), pair.To.String()):
			return ghrepo.Repository{}, fmt.Sprintf("GitHub names %s as %s, another repository", pair.To, read.Name)
		}
		spelled = read.Name
	}
	return spelled, ""
}

// stillInstalledOn is the sentence naming each role App GitHub answered is installed on from, a
// configured pair's `from`, where a transfer leaves none (appauth.AppsAre): "the implement and
// review Apps are still installed on sjawhar/legion", or one App alone.
func stillInstalledOn(apps appInstallations, from string) string {
	var installed []appauth.AppRole
	for _, role := range appauth.Roles {
		if apps.of(role).installed {
			installed = append(installed, role)
		}
	}
	return appauth.AppsAre(installed, "still installed on "+from)
}

// renameAtBoot runs the rename pass over every issue of the project at boot, under rt's live-pod
// rule (bootKeep): after supervision opened and the worker image passed its probe, so the runtime
// answers for the pods it holds, and before the consumers open, so no fact and no launch reads a
// record while the pass writes it. Each write that wrote a row asks the refresher to read the
// live records again at its commit (renamePass.written), so the cache set and the heard set name
// the new names.
func (w *workflowRuntime) renameAtBoot(ctx context.Context, rt runtime.Runtime) {
	w.renamePass(bootKeep(rt, w.stateDir)).run(ctx, nil)
}

// podEndsOn is what the end of issues' pods under rt runs (outbox.podEnded): it drops the
// repository kept for each of them (dropLaunchedRepository), so the next launch reads it again,
// and runs the rename pass over them under podEndKeep's rule, whose writes wake the refresher's
// read of the live records at their commit (renamePass.written).
func (w *workflowRuntime) podEndsOn(rt runtime.Runtime) func(context.Context, []string) {
	pass := w.renamePass(podEndKeep(rt, w.stateDir))
	return func(ctx context.Context, issues []string) {
		for _, issue := range issues {
			w.dropLaunchedRepository(issue)
		}
		pass.run(ctx, issues)
	}
}
