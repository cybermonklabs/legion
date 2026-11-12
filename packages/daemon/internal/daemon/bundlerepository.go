package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// tellBundleRepositoryRefused is the store's take of an issue's saved workspace bundle
// (store.Store.TakeWorkspaceBundleForRestore) with the one thing the daemon owes a person added
// to it: the bundle the store refused because it is of a repository the issue has left is told
// on the issue itself (bundleRepositoryMessage, written through write). Every other outcome
// passes through untouched, and the refusal passes through as it was: the launcher still logs
// what it was not sent, and the pod still starts on what its own fetch provisioned.
//
// The issue is told once per bundle row, not once per pod that asks. The refusal is the bundle's
// one restore attempt and the store records it in the same statement that answers it, so the
// next pod of the issue is answered runtime.WorkspaceBundleNone and reaches no message at all.
//
// It is this wrapper and not the launcher's warning because only a person can act on it: the
// work is a bundle of commits of the old repository, no pod of the issue is a clone of that
// repository any more, and nothing in the daemon can ever put the work back. A log line reaches
// an operator who reads the daemon's logs. The message reaches the agent that did the work.
//
// write is the plan's own (plan.writeOnIssue), which refuses the write in a deployment with no
// workflow: such a daemon has no Dispatch issue to write on, and the refusal is logged with what
// the message would have said.
func tellBundleRepositoryRefused(take runtime.SavedWorkspaceBundle,
	write func(ctx context.Context, issue, body string) error, log *slog.Logger) runtime.SavedWorkspaceBundle {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
		bundle, outcome, err := take(ctx, issue, podUID, podCreated)
		if err != nil || outcome != runtime.WorkspaceBundleRefusedRepository {
			return bundle, outcome, err
		}
		body := bundleRepositoryMessage(bundle)
		if write == nil {
			log.Warn("launcher bundle: the saved workspace bundle is of a repository this issue has left, and this deployment has no workflow to tell the issue through",
				"issue", issue, "pod", podUID, "bundleRepository", bundle.Repo, "head", bundle.Head, "saved", bundle.At)
			return bundle, outcome, nil
		}
		if err := write(ctx, issue, body); err != nil {
			if ctx.Err() == nil {
				log.Error("supervise: write on the issue that its saved workspace bundle is of the repository it has left; the work stays in the database and nothing tells the issue again",
					"issue", issue, "pod", podUID, "bundleRepository", bundle.Repo, "head", bundle.Head, "saved", bundle.At, "error", err)
			}
			return bundle, outcome, nil
		}
		log.Warn("supervise: the saved workspace bundle of this issue is of the repository it has left, so no pod restores it; the issue is told and the work stays in the daemon's database",
			"issue", issue, "pod", podUID, "bundleRepository", bundle.Repo, "head", bundle.Head, "saved", bundle.At, "release", bundle.Release)
		return bundle, outcome, nil
	}
}

// bundleRepositoryMessage is what the issue is told when the daemon refuses to restore its saved
// work: the repository the work is of, the commit the saved working copy sat on, each bookmark
// that pointed into the work, the moment the release saved it, and that the work stays in the
// daemon's database. Another agent reads it, so it follows ASD-STE100.
//
// It names the old repository and not the new one on purpose. The work is in the old
// repository's history, and a reader sent to the new repository would look for commits that are
// not there. The moment of the save is the reader's other handle on the work: it says how much
// of the issue's work the bundle can hold.
func bundleRepositoryMessage(b runtime.WorkspaceBundle) string {
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(b.Bookmarks)) {
		lines = append(lines, fmt.Sprintf("- `%s` at commit `%s`", name, b.Bookmarks[name]))
	}
	bookmarks := "The saved work has no bookmark."
	if len(lines) > 0 {
		bookmarks = fmt.Sprintf("The saved work has these bookmarks:\n\n%s", strings.Join(lines, "\n"))
	}
	return fmt.Sprintf(`The daemon did not restore the saved work of this issue.

A release of this issue's pod saved the work that no remote had. The daemon saved that work at %s. The work is of the repository `+"`%s`"+`. This issue is on a different repository now. The daemon does not put work of the old repository into a clone of the new repository. Each new pod of this issue is a clone of the new repository.

The saved work has the head commit `+"`%s`"+`. %s

The daemon keeps the saved work in its database. The daemon sends it to no pod. Ask an operator for the work if you need it.`,
		b.At.UTC().Format(time.RFC3339), b.Repo, b.Head, bookmarks)
}
