package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

var (
	registeredWorkspace = regexp.MustCompile(`(?i)already (?:registered|exists)`)
	commitID            = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// createWorkspace ports workspace.ts's createWorkspace. It resolves a bookmark before deleting a
// stale git worktree entry or adding a workspace: a conflicted bookmark must not leave a registered
// working copy behind.
//
// One read of the issue's bookmark legion/<KEY>, its local row and origin's (readBookmark),
// decides where the workspace starts:
//   - A local bookmark conflicted with a deletion whose origin row is tracked with no commit (GitHub
//     deleted the branch after the bookmark moved on without a push) is set aside when the move
//     added at least one commit beyond what it removed and none of them is described
//     (undescribedMove): jj pushes no undescribed commit, so the move was never meant to reach
//     GitHub, and this is the unmoved merged bookmark in intent. Once the default branch resolves,
//     the local bookmark is deleted, the commits' ids are logged oldest first (they stay visible,
//     the clone never abandoning unreachable commits), and the workspace starts at the default
//     branch. A move backwards, which added nothing, keeps the conflict's refusal.
//   - A conflicted local bookmark is refused by name, with its sides. That includes a conflict with
//     a deleted side, which `bookmarks(exact:)` resolves to the other side alone: a local deletion
//     never pushed and then origin's branch moving, or a local move never pushed and then the
//     branch deleted on GitHub. The refusal names two ways out: keep an added commit
//     (`jj bookmark set`, which jj refuses for the removed side or the default branch), or start
//     from the default branch with `jj bookmark delete`, after deleting the branch on GitHub when
//     origin has it: the GitHub deletion alone leaves the local side in conflict with a deletion
//     when both sides moved.
//   - A local bookmark on one commit is where the workspace starts, whether or not origin has the
//     branch yet.
//   - With no local bookmark, a conflicted origin row is refused: only concurrent fetches leave
//     one, and the next provisioning's fetch sets the row to origin's branch as it is then.
//   - With no local bookmark, an untracked origin row is the issue's own branch, made on GitHub
//     before this workspace existed: the branch the daemon creates at the default branch before any
//     role of the issue starts (the daemon's issue_branch outbox row), a repository's fixture, or
//     the branch an earlier pod of the issue pushed before its disk went with it. A fresh clone
//     tracks the default branch alone, so such a branch is only a remote row. It is tracked, which
//     creates the local bookmark
//     at its commit, and the workspace starts there, so its commits are there and the issue's next
//     push moves it. The bookmark is read again after the track: a fetch between the read and the
//     track leaves it on a newer commit, where a workspace at the listed one could never move it,
//     so that provisioning is refused and the next one starts at the bookmark.
//   - With no local bookmark, a tracked origin row is a local deletion (`jj bookmark delete` in the
//     shared clone) never pushed, where tracking changes nothing. It is refused by name, with the
//     operator's three ways out: restore the bookmark (`jj bookmark set`); cancel the deletion
//     (`jj bookmark forget`, which leaves the row untracked, so the next provisioning adopts
//     origin's branch); or start from the default branch instead, by deleting the branch on GitHub
//     (the pull request's Delete branch button, or `gh api -X DELETE`), after which the next
//     provisioning's fetch drops the row. A push of the deletion from the shared clone is not
//     offered: the clone authenticates through `legion git-credential`, which reads a role's gh
//     files under a LEGION_GH_OWNERS_DIR and GH_CONFIG_DIR no operator shell carries, and `jj git
//     push --deleted` would push every pending deletion in the shared clone, other issues'
//     branches with it.
//   - No row at all is an issue whose branch GitHub does not have, such as a merged branch GitHub
//     deleted. The workspace starts at the repository's default branch (workspace.DefaultBranch),
//     resolved to one commit first (baseCommit), with the bookmark created on it.
func createWorkspace(ctx context.Context, run Runner, workspace Workspace, log func(string)) error {
	cloneDir := workspace.Clone
	workspaceName := filepath.Base(workspace.Dir)
	remote := workspace.Bookmark + "@origin"
	rows, err := readBookmark(ctx, run, workspace, workspace.Bookmark)
	if err != nil {
		return err
	}
	local, origin := rows.local, rows.origin
	var revision string
	var setAside []string
	notCreated := "workspace " + workspace.Dir + " was not created"
	switch {
	case local.conflict:
		if local.hasDeletedSide() && origin.tracked && !origin.present {
			if setAside, err = undescribedMove(ctx, run, workspace, local); err != nil {
				return err
			}
		}
		if len(setAside) == 0 {
			return conflictRefusal(workspace, local, origin)
		}
		// Set aside once the default branch resolves, below.
	case local.present:
		revision = local.added[0]
	case origin.conflict:
		return conflictedOriginRefusal(workspace, origin, notCreated)
	case origin.present && origin.tracked:
		return deletedLocalBookmarkRefusal(workspace, origin, notCreated, "")
	case origin.present:
		if _, err := runCheckedOnClone(ctx, run, cloneDir, "bookmark", "track", originRow(workspace.Bookmark)); err != nil {
			return err
		}
		revision = origin.added[0]
		tracked, err := readBookmark(ctx, run, workspace, workspace.Bookmark)
		if err != nil {
			return err
		}
		if now := tracked.local; !now.present || now.conflict || now.added[0] != revision {
			return fmt.Errorf("Bookmark %s moved from %s to %s while it was being tracked; workspace %s was not created. Provision again: the next provisioning starts at origin's branch as it is then",
				remote, revision, listed(now.added), workspace.Dir)
		}
	}
	fromBase := revision == ""
	if fromBase {
		if revision, err = baseCommit(ctx, run, workspace); err != nil {
			return err
		}
		// After the default branch resolves, so the one line naming the set-aside commits is never
		// a start that did not happen.
		if len(setAside) > 0 {
			if _, err := runCheckedOnClone(ctx, run, cloneDir, "bookmark", "delete", exactName(workspace.Bookmark)); err != nil {
				return err
			}
			log(fmt.Sprintf("Bookmark %s was moved after its last push onto commits nobody described (%s), and GitHub deleted its branch: workspace %s starts at %s. Those commits stay visible in the shared clone %s (git.abandon-unreachable-commits is false), recoverable by id",
				workspace.Bookmark, listed(setAside), workspace.Dir, workspace.DefaultBranch, cloneDir))
		}
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Dir), 0o700); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	// Provision adds a workspace only when it finds no directory, so an entry git still registers
	// for this one is stale: the directory went while jj still registered the workspace (a crash
	// inside Remove), or jj forgot the workspace after its directory went, which leaves the
	// colocated worktree, and `jj workspace add` would stop at git's `missing but already registered
	// worktree`. Only that entry goes: a bare `git worktree prune` also deletes the entry of every
	// other workspace whose directory this process cannot see.
	if err := removeGitWorktree(cloneDir, workspace.Dir); err != nil {
		return err
	}

	// `jj workspace add` is the one jj command on an existing shared clone not built by onClone: it
	// refuses --ignore-working-copy (on 0.44 and 0.45, after registering the workspace and creating
	// its directory), so the add snapshots the clone's own working copy. The jj commands that make a
	// clone are not onClone's either (cloneSteps): `jj git clone` and `jj git init --colocate`, which
	// open no repository yet, and the clear and the abandon that leave an issue pod's directly filled
	// clone with no working tree, which must act on the working copy (issuePodClone). Every other
	// command provisioning and removal run on the clone is onClone's, and so is every command a
	// refusal prints (wayOut). The add registers the workspace with no files (--sparse-patterns
	// empty), so it is quick under the store lock; Provision writes the working copy after letting
	// the lock go (checkout).
	add := []string{
		"jj", "workspace", "add", workspace.Dir, "--name", workspaceName, "--revision", revision, "--sparse-patterns", "empty", "-R", cloneDir,
	}
	// The add and its retry run in the shared clone, the directory their own -R names: it is
	// there, since the clone is what this workspace is added to.
	result, err := runCommand(ctx, run, add, nil, cloneDir)
	if err != nil {
		return runFailure(add, result, err)
	}
	forgotten := result.ExitCode != 0
	if forgotten {
		if !registeredWorkspace.MatchString(result.Stderr) {
			return commandFailure(add, result)
		}
		// The add failed before creating a git worktree, and the stale entry went above.
		if _, err := runCheckedOnClone(ctx, run, cloneDir, "workspace", "forget", workspaceName); err != nil {
			return err
		}
		if _, err := RunChecked(ctx, run, add, nil, cloneDir); err != nil {
			return err
		}
	}
	// The checkout marker goes on the workspace first, the moment the add has made it and before
	// any of its files is written: a provisioning that fails or stops anywhere after this, up to
	// its checkout's end, leaves the workspace marked, so the next one clears whatever a cut-short
	// checkout wrote before checking out again (Provision, checkoutMarkerFile).
	if err := writeCheckoutMarker(workspace.Dir); err != nil {
		return err
	}
	// Recorded next, ahead of anything else that can still fail: a provisioning that failed after
	// the add finds the workspace there next time and adds it no more, so this is the record closest
	// to the add, and the next provisioning records its own moment only where a stop left none, or
	// one it cannot read (finishUnfinishedRegistration).
	if err := RecordCreated(workspace.Dir, time.Now()); err != nil {
		return err
	}
	if !fromBase {
		return nil
	}
	return setBookmarkOnWorkingCopy(ctx, run, workspace)
}

// setBookmarkOnWorkingCopy places workspace.Bookmark on the workspace's working-copy commit (`@`).
func setBookmarkOnWorkingCopy(ctx context.Context, run Runner, workspace Workspace) error {
	_, err := RunCheckedIn(ctx, run, workspace, []string{"jj", "bookmark", "set", jjString(workspace.Bookmark), "-r", "@"})
	return err
}

// undescribedMove is what the local bookmark's move added beyond what it removed
// (`<removed>..<added>`), oldest first, when none of it is described, and none when any is. jj
// pushes no undescribed commit, so such a move was never meant to reach GitHub. A move that added
// nothing (a bookmark moved backwards off its last push) returns none too, and keeps the conflict's
// refusal.
func undescribedMove(ctx context.Context, run Runner, workspace Workspace, local bookmarkRow) ([]string, error) {
	revset := "(" + strings.Join(local.removed, " | ") + ")..(" + strings.Join(local.added, " | ") + ")"
	result, err := runCheckedOnClone(ctx, run, workspace.Clone, "log", "-r", revset, "--reversed", "--no-graph", "-T", `commit_id ++ "|" ++ if(description, "1", "0") ++ "\n"`)
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range nonEmptyLines(result.Stdout) {
		id, described, ok := strings.Cut(line, "|")
		if !ok || !commitID.MatchString(id) || described != "0" && described != "1" {
			return nil, fmt.Errorf("Bookmark %s's move %s printed %q, not a commit id and 0 or 1; workspace %s was not created", workspace.Bookmark, revset, line, workspace.Dir)
		}
		if described == "1" {
			return nil, nil
		}
		commits = append(commits, id)
	}
	return commits, nil
}

// conflictRefusal refuses a conflicted local bookmark by name, with its sides and two ways out:
// keep an added commit, or start from the default branch, after deleting the branch on GitHub
// when origin has it.
func conflictRefusal(workspace Workspace, local, origin bookmarkRow) error {
	keep, commit := "its added commit", local.added[0]
	if len(local.added) > 1 {
		keep, commit = "one of its added commits", "<commit>"
	}
	startAtBase := wayOut(workspace.Clone, "bookmark", "delete", exactName(workspace.Bookmark))
	if origin.present {
		startAtBase = "delete the branch on GitHub (the pull request's Delete branch button, or " + deleteOnGitHub(workspace) + ") and run " + startAtBase
	}
	return fmt.Errorf("Bookmark %s is conflicted %s; workspace %s was not created. Keep %s: %s. Start from %s instead: %s, and the next provisioning starts at %s",
		workspace.Bookmark, local.sides(), workspace.Dir, keep, wayOut(workspace.Clone, "bookmark", "set", jjString(workspace.Bookmark), "-r", commit),
		workspace.DefaultBranch, startAtBase, workspace.DefaultBranch)
}

// conflictedOriginRefusal refuses a conflicted origin row of the issue's bookmark with no local
// bookmark beside it, with consequence, what the refusal leaves of the workspace: only concurrent
// fetches leave such a row, and the next provisioning's fetch sets it to origin's branch as it is
// then.
func conflictedOriginRefusal(workspace Workspace, origin bookmarkRow, consequence string) error {
	return fmt.Errorf("Remote bookmark %s is conflicted %s, which concurrent fetches leave; %s. Provision again: the next provisioning's fetch sets the row to origin's branch as it is then",
		workspace.Bookmark+"@origin", origin.sides(), consequence)
}

// deletedLocalBookmarkRefusal refuses the issue's bookmark deleted in the shared clone (`jj
// bookmark delete`) and never pushed while origin's row is tracked, with consequence, what the
// refusal leaves of the workspace, and the operator's three ways out: restore the bookmark; cancel
// the deletion (`jj bookmark forget`), which leaves origin's row untracked, so the next
// provisioning adopts origin's branch; or start from the default branch, by deleting the branch on
// GitHub, after which the next provisioning's fetch drops the row. then is what the restore and the
// cancel each need besides their command before the next provisioning starts at origin's branch:
// nothing ("") for a workspace not yet added, and the deletion of its directory for one added
// without the branch (finishUnfinishedRegistration).
func deletedLocalBookmarkRefusal(workspace Workspace, origin bookmarkRow, consequence, then string) error {
	cloneDir := workspace.Clone
	return fmt.Errorf("Bookmark %s was deleted in the shared clone %s and the deletion never pushed, while %s is tracked at %s; %s. "+
		"Restore it%s: %s. "+
		"Cancel the deletion%s, and the next provisioning adopts origin's branch: %s. "+
		"Start from %s instead: delete the branch on GitHub (the pull request's Delete branch button, or %s), and the next provisioning starts at %s",
		workspace.Bookmark, cloneDir, workspace.Bookmark+"@origin", origin.added[0], consequence,
		then, wayOut(cloneDir, "bookmark", "set", jjString(workspace.Bookmark), "-r", originRow(workspace.Bookmark)),
		then, wayOut(cloneDir, "bookmark", "forget", exactName(workspace.Bookmark)),
		workspace.DefaultBranch, deleteOnGitHub(workspace), workspace.DefaultBranch)
}

// baseCommit is the commit a workspace with no issue branch starts at: the repository's default
// branch's (workspace.DefaultBranch, as GitHub names it), resolved to one commit id as the issue
// bookmark is, since a `jj workspace add --revision <branch>` jj cannot resolve still registers
// the workspace, parented on the root commit, before it reports the error. Each default branch
// that does not resolve is refused by name before anything is added: a conflicted one, with
// origin's to keep (`--allow-backwards`, since jj refuses to move a bookmark sideways off two
// local moves). With no local bookmark of it: a conflicted <branch>@origin, which concurrent
// fetches leave and the next provisioning's fetch settles; one deleted in the shared clone while
// origin's is tracked, with origin's to restore; one forgotten (`jj bookmark forget <branch>`),
// which leaves <branch>@origin untracked, with origin's to track; and an absent one, a clone that
// does not have the branch GitHub names.
func baseCommit(ctx context.Context, run Runner, workspace Workspace) (string, error) {
	branch := workspace.DefaultBranch
	remote := branch + "@origin"
	rows, err := readBookmark(ctx, run, workspace, branch)
	if err != nil {
		return "", err
	}
	switch base, origin := rows.local, rows.origin; {
	case base.conflict:
		return "", fmt.Errorf("Bookmark %s is conflicted %s; workspace %s was not created. Keep origin's: %s, and the next provisioning starts there",
			branch, base.sides(), workspace.Dir, wayOut(workspace.Clone, "bookmark", "set", jjString(branch), "-r", originRow(branch), "--allow-backwards"))
	case base.present:
		return base.added[0], nil
	case origin.conflict:
		return "", fmt.Errorf("Remote bookmark %s is conflicted %s, which concurrent fetches leave, and %s is not in the shared clone %s; workspace %s was not created. Provision again: the next provisioning's fetch sets the row to origin's %s as it is then",
			remote, origin.sides(), branch, workspace.Clone, workspace.Dir, branch)
	case origin.present && origin.tracked:
		return "", fmt.Errorf("Bookmark %s was deleted in the shared clone %s while %s is tracked at %s; workspace %s was not created. Restore it: %s, and the next provisioning starts there",
			branch, workspace.Clone, remote, origin.added[0], workspace.Dir, wayOut(workspace.Clone, "bookmark", "set", jjString(branch), "-r", originRow(branch)))
	case origin.present:
		return "", fmt.Errorf("Bookmark %s is not tracked in the shared clone %s, where %s is at %s untracked; workspace %s was not created. Track it: %s, and the next provisioning starts there",
			branch, workspace.Clone, remote, origin.added[0], workspace.Dir, wayOut(workspace.Clone, "bookmark", "track", originRow(branch)))
	default:
		return "", fmt.Errorf("Bookmark %s is not in the shared clone %s; workspace %s was not created. An issue with no branch starts at the repository's default branch, which GitHub names %s",
			branch, workspace.Clone, workspace.Dir, branch)
	}
}

// wayOut is a command a refusal prints for an operator to run from a shell: the jj command on the
// shared clone that onClone builds, so it takes no snapshot of the clone's working copy, in
// backticks. A zero clone has no runnable command; the operational callers reject it before a
// refusal reaches this formatter.
func wayOut(cloneDir string, args ...string) string {
	argv, err := onClone(cloneDir, args...)
	if err != nil {
		return "`<invalid workspace clone directory>`"
	}
	return "`" + shellprefix.Command(argv) + "`"
}

// exactName is the jj name pattern that matches the bookmark name alone, whatever characters it
// holds: `jj bookmark list`, `delete` and `forget` take one, as does `jj git fetch --branch`.
func exactName(name string) string { return "exact:" + jjString(name) }

// originRow is the bookmark name's row on origin as jj reads it: `jj bookmark track`'s argument,
// and a revset naming origin's commit of it.
func originRow(name string) string { return jjString(name) + "@origin" }

// deleteOnGitHub is the printed command that deletes the issue's branch on GitHub.
func deleteOnGitHub(workspace Workspace) string {
	return "`" + shellprefix.Command([]string{"gh", "api", "-X", "DELETE", "repos/" + workspace.Repo.String() + "/git/refs/heads/" + workspace.Bookmark}) + "`"
}

// bookmarkRow is one row of `jj bookmark list --all-remotes`: whether the bookmark exists there,
// whether it is conflicted, whether a remote row is tracked, and the commits it adds and removes. A
// present row that is not conflicted adds one commit and removes none; an absent row adds none.
type bookmarkRow struct {
	present, conflict, tracked bool
	added, removed             []string
}

// sides is a conflicted row's sides for a refusal: its added and removed commits, and whether one
// side is a deletion, which jj leaves out of the added commits.
func (r bookmarkRow) sides() string {
	sides := fmt.Sprintf("(adds %s; removes %s)", listed(r.added), listed(r.removed))
	if r.hasDeletedSide() {
		sides += ", one side a deletion"
	}
	return sides
}

// hasDeletedSide is whether a conflicted row's sides include a deletion, which jj leaves out of
// its added commits.
func (r bookmarkRow) hasDeletedSide() bool {
	return len(r.added) <= len(r.removed)
}

// bookmarkRows is a bookmark's local row and origin's. A row jj does not list is the zero row:
// absent.
type bookmarkRows struct {
	local, origin bookmarkRow
}

// bookmarkRowTemplate prints one line per row, its fields separated by "|": where it is (local,
// origin, git), then present, conflict and tracked as 1 or 0, then the added and the removed commit
// ids, comma-separated.
const bookmarkRowTemplate = `if(remote, remote, "local") ++ "|" ++ if(present, "1", "0") ++ "|" ++ if(conflict, "1", "0") ++ "|" ++ if(tracked, "1", "0") ++ "|" ++ added_targets.map(|c| c.commit_id()).join(",") ++ "|" ++ removed_targets.map(|c| c.commit_id()).join(",") ++ "\n"`

// readBookmark is readBookmarkRows for a workspace about to be added: a bookmark it could not read
// refuses the add, so its refusal says the workspace was not created.
func readBookmark(ctx context.Context, run Runner, workspace Workspace, name string) (bookmarkRows, error) {
	rows, err := readBookmarkRows(ctx, run, workspace.Clone, name)
	var unreadable *unreadableBookmark
	if errors.As(err, &unreadable) {
		return bookmarkRows{}, unreadable.refusal("; workspace " + workspace.Dir + " was not created")
	}
	return rows, err
}

// readBookmarkRows reads the bookmark name in the shared clone at cloneDir (the issue's, or the
// default branch), its local row and origin's, in one `jj bookmark list`. A bookmark that exists
// nowhere lists nothing. Every row must have the template's shape, with 0/1 flags, full commit ids,
// one added commit on a present row that is not conflicted and at least one on a conflicted row,
// and one row per place: a commit jj cannot load prints an error value where its id goes, which a
// workspace add would take as a revision. A list jj failed, or printed out of that shape, is an
// *unreadableBookmark.
func readBookmarkRows(ctx context.Context, run Runner, cloneDir, name string) (bookmarkRows, error) {
	list, err := onClone(cloneDir, "bookmark", "list", "--all-remotes", exactName(name), "-T", bookmarkRowTemplate)
	if err != nil {
		return bookmarkRows{}, err
	}
	result, err := runCommand(ctx, run, list, nil, cloneDir)
	if err != nil {
		return bookmarkRows{}, runFailure(list, result, err)
	}
	if result.ExitCode != 0 {
		return bookmarkRows{}, &unreadableBookmark{name: name, failure: commandFailure(list, result)}
	}
	var rows bookmarkRows
	seen := map[string]bool{}
	for _, line := range nonEmptyLines(result.Stdout) {
		where, row, ok := parseBookmarkRow(line)
		if !ok || seen[where] {
			return bookmarkRows{}, &unreadableBookmark{name: name, row: line, list: strings.Join(list, " ")}
		}
		seen[where] = true
		switch where {
		case "local":
			rows.local = row
		case "origin":
			rows.origin = row
		}
	}
	return rows, nil
}

// unreadableBookmark is readBookmarkRows's refusal of a bookmark it could not read: jj's list
// failed (failure), or printed row, which is not the shape list prints.
type unreadableBookmark struct {
	name, row, list string
	failure         error
}

func (e *unreadableBookmark) Error() string { return e.refusal("").Error() }
func (e *unreadableBookmark) Unwrap() error { return e.failure }

// refusal is the refusal with its consequence, such as readBookmark's "; workspace <dir> was not
// created", after the bookmark's own clause.
func (e *unreadableBookmark) refusal(consequence string) error {
	if e.failure != nil {
		return fmt.Errorf("Bookmark %s could not be resolved%s: %w", e.name, consequence, e.failure)
	}
	return fmt.Errorf("Bookmark %s's row %q is not the shape %s prints%s", e.name, e.row, e.list, consequence)
}

// finishUnfinishedRegistration finishes the registration of a workspace Provision finds marked
// (checkoutMarkerFile) with no sparse pattern, under the store lock and before its checkout. Such a
// workspace's provision may have stopped anywhere after its `jj workspace add` (createWorkspace),
// and no later provision adds it again, so this one writes what that one left out:
//   - The creation record, where there is none (Created), at this moment. A record Created refuses
//     is replaced the same way, and the replacement logged with the refusal: a provision killed
//     between RecordCreated's truncation and its write leaves an empty one, and the workspace is
//     marked, so the record is Legion's own and no role has worked there. RecordCreated refuses a
//     linked .jj as Created does, and anything at the record but a regular file
//     (rolefile.OpenRegular), so a link, a FIFO or another special file there is not replaced,
//     followed, written to or waited on.
//   - The issue's bookmark, where neither the shared clone nor origin has it: the workspace was
//     added at the default branch (baseCommit), and its `jj bookmark set` did not finish. It is
//     set on the working-copy commit (setBookmarkOnWorkingCopy).
//
// A local bookmark, conflicted or not, is left where it is: a workspace added at the issue's
// branch starts on top of it, so the bookmark is on the parent of the working-copy commit, never
// on that commit. With no local bookmark, a row on origin is refused by name rather than checked
// out with no bookmark, before anything is set, tracked or checked out, each with its way out:
//   - A conflicted row, as createWorkspace refuses it (conflictedOriginRefusal): the next
//     provisioning's fetch sets the row to origin's branch as it is then.
//   - A tracked row, a local deletion never pushed, as createWorkspace refuses it
//     (deletedLocalBookmarkRefusal), but for the restore of the bookmark and the cancel of the
//     deletion: this workspace was added at the default branch, and neither moves it to origin's
//     branch (a restored bookmark is a local bookmark, left where it is, and the untracked row is
//     not adopted), so each is followed by the directory's deletion, after which the next
//     provisioning adds the workspace again at origin's branch.
//   - An untracked row, a branch GitHub has had since the workspace was added without it: the way
//     out is the directory's deletion, after which the next provisioning adds the workspace again
//     at origin's branch. A marked workspace never finished a checkout, so no role worked in it
//     and the deletion loses no work.
func finishUnfinishedRegistration(ctx context.Context, run Runner, workspace Workspace, log func(string)) error {
	if _, ok, unreadable := Created(workspace.Dir); unreadable != nil || !ok {
		switch err := RecordCreated(workspace.Dir, time.Now()); {
		case err != nil && unreadable != nil:
			return fmt.Errorf("%v, and it could not be replaced: %w", unreadable, err)
		case err != nil:
			return err
		case unreadable != nil:
			log(fmt.Sprintf("Workspace %s: replaced the creation record it could not read with this provisioning's moment: %v", workspace.Dir, unreadable))
		}
	}
	rows, err := readBookmarkRows(ctx, run, workspace.Clone, workspace.Bookmark)
	if err != nil {
		return fmt.Errorf("finish the registration of workspace %s: %w", workspace.Dir, err)
	}
	local, origin := rows.local, rows.origin
	notCheckedOut := "workspace " + workspace.Dir + " holds no checkout and was not checked out"
	switch {
	case local.present || local.conflict:
		return nil
	case origin.conflict:
		return conflictedOriginRefusal(workspace, origin, notCheckedOut)
	case origin.present && origin.tracked:
		return deletedLocalBookmarkRefusal(workspace, origin, notCheckedOut, ", then delete its directory, which holds no work")
	case origin.present:
		return fmt.Errorf("Bookmark %s is not in the shared clone %s while %s is at %s, and workspace %s holds no checkout: the provision that added the workspace stopped before it set the bookmark, and GitHub has had the branch since, so the workspace does not start at origin's branch and is not checked out. "+
			"Delete its directory, which holds no work, and the next provisioning adds it again at origin's branch as it is then",
			workspace.Bookmark, workspace.Clone, workspace.Bookmark+"@origin", origin.added[0], workspace.Dir)
	}
	return setBookmarkOnWorkingCopy(ctx, run, workspace)
}

// parseBookmarkRow reads one line bookmarkRowTemplate printed, reporting whether it has the shape.
func parseBookmarkRow(line string) (string, bookmarkRow, bool) {
	fields := strings.Split(line, "|")
	if len(fields) != 6 {
		return "", bookmarkRow{}, false
	}
	var flags [3]bool
	for i, field := range fields[1:4] {
		switch field {
		case "1":
			flags[i] = true
		case "0":
		default:
			return "", bookmarkRow{}, false
		}
	}
	row := bookmarkRow{present: flags[0], conflict: flags[1], tracked: flags[2], added: commitList(fields[4]), removed: commitList(fields[5])}
	for _, id := range slices.Concat(row.added, row.removed) {
		if !commitID.MatchString(id) {
			return "", bookmarkRow{}, false
		}
	}
	switch {
	case row.conflict && len(row.added) == 0:
		return "", bookmarkRow{}, false
	case row.present && !row.conflict && (len(row.added) != 1 || len(row.removed) != 0):
		return "", bookmarkRow{}, false
	}
	return fields[0], row, true
}

func commitList(field string) []string {
	if field == "" {
		return nil
	}
	return strings.Split(field, ",")
}

// listed is a row's commits for a refusal: comma-separated, or "nothing".
func listed(commits []string) string {
	if len(commits) == 0 {
		return "nothing"
	}
	return strings.Join(commits, ", ")
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// ownCommitsRevset ports workspace.ts's ownCommitsRevset: only commits unique to a closing
// workspace are abandoned; all commits reached by another working copy, a bookmark, a remote, or a
// tag survive.
func ownCommitsRevset(workspaceName string) string {
	return "::" + workspaceName + "@ ~ ::(working_copies() ~ " + workspaceName + "@) ~ ::(bookmarks() | remote_bookmarks() | tags())"
}

// removingSuffix marks a workspace directory Remove has renamed aside, about to delete: a sibling
// name in the same parent directory, so the rename is one atomic same-filesystem operation
// regardless of how large the directory is, unlike the removal that follows it
// (rolefile.RemoveTree), which a kill can interrupt partway through a large tree.
const removingSuffix = ".removing"

// removingPath is where Remove renames workspace.Dir to before deleting it.
func removingPath(dir string) string { return dir + removingSuffix }

// Remove ports workspace.ts's removeIssueWorkspace, with one addition: the workspace directory is
// renamed aside (removingSuffix) before the slower recursive delete that actually frees the
// space, so a kill partway through that delete leaves the renamed-aside name rather than a
// half-deleted tree at the original one. The rename goes first so a crash leaves the
// registered-but-missing state that Provision repairs with forget and add. jj's forget leaves the
// colocated worktree of a directory already gone, so Remove deletes that entry itself, also when
// the workspace is neither registered nor present (a crash after the forget). workspace is
// Location's, which names the clone; any other is refused before anything is removed.
//
// store is the repository's store lock, the one a provision is handed as Request.StoreLock (nil
// holds nothing). Remove holds it for every command on the shared clone, the rename aside and the
// git worktree entry's removal, and lets it go before it deletes the aside directory: that delete's
// time follows the workspace's size, and it touches nothing of the clone's, so the provisions of the
// repository's other issues do not wait for it. An aside directory a crash left before its delete
// is cleared by the issue's next Remove, before it takes the lock.
func Remove(ctx context.Context, run Runner, workspace Workspace, store sync.Locker) error {
	if !located(workspace) {
		return fmt.Errorf("workspace to remove (%#v) is not a workspace Location names", workspace)
	}
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return err
	}
	// A stale aside from an interrupted removal of this same directory name, never cleared (named
	// again later, or a previous pass's delete itself never ran): cleared before the rename so it
	// never refuses with "already exists", and before the store lock, since it holds nothing of the
	// clone's.
	aside := removingPath(workspace.Dir)
	if err := rolefile.RemoveTree(aside); err != nil {
		return fmt.Errorf("clear a stale removal-in-progress directory: %w", err)
	}
	held := &storeLock{locker: store}
	held.lock()
	defer held.unlock()
	cloneDir, workspaceName := workspace.Clone, filepath.Base(workspace.Dir)
	cloneExists, err := pathExists(filepath.Join(cloneDir, ".jj"))
	if err != nil {
		return err
	}
	registered := false
	var commits []string
	if cloneExists {
		listed, err := runCheckedOnClone(ctx, run, cloneDir, "workspace", "list", "-T", `name ++ "\n"`)
		if err != nil {
			return err
		}
		for _, name := range nonEmptyLines(listed.Stdout) {
			if name == workspaceName {
				registered = true
				break
			}
		}
	}
	if registered {
		own, err := runCheckedOnClone(ctx, run, cloneDir, "log", "-r", ownCommitsRevset(workspaceName), "--no-graph", "-T", `commit_id ++ "\n"`)
		if err != nil {
			return err
		}
		commits = nonEmptyLines(own.Stdout)
	}
	if present, err := pathExists(workspace.Dir); err != nil {
		return err
	} else if present {
		if err := os.Rename(workspace.Dir, aside); err != nil {
			return fmt.Errorf("rename workspace directory aside before removing it: %w", err)
		}
	}
	if registered {
		if len(commits) > 0 {
			if _, err := runCheckedOnClone(ctx, run, cloneDir, "abandon", "-r", strings.Join(commits, " | ")); err != nil {
				return err
			}
		}
		if _, err := runCheckedOnClone(ctx, run, cloneDir, "workspace", "forget", workspaceName); err != nil {
			return err
		}
	}
	if cloneExists {
		if err := removeGitWorktree(cloneDir, workspace.Dir); err != nil {
			return err
		}
	}
	held.unlock()
	if err := rolefile.RemoveTree(aside); err != nil {
		return fmt.Errorf("remove workspace directory: %w", err)
	}
	return nil
}
