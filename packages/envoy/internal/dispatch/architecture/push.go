// push.go holds what a push of a branch starts: Pushed, which records the push against every
// enabled source on that repository and branch, and each source's push worker, which imports the
// source until no push it holds is newer than its last import's head lookup. The Importer fields
// that hold this state stay in importer.go with the rest of the type.

package architecture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// Every line push.go logs: each message is held once, here, for the code and the tests alike.
const (
	// pushAfterCloseMessage is Pushed's WARN line for a push that reached the importer once its
	// Close had begun, logged before Pushed reads a source.
	pushAfterCloseMessage = "dispatch architecture: a push reached the importer after it began to close; this process imports nothing for it, and a source on that repository and branch, if any, is imported again at its next five-minute pass"
	// pushRefusedMessage is recordPush's WARN line for a push of a source whose worker the count
	// refused, because Close had begun.
	pushRefusedMessage = "dispatch architecture: a push reached the importer after it began to close; this process does not import the source for it, and the source is imported again at its next five-minute pass"
	// importedAfterPushMessage is a push worker's INFO line for each import it made.
	importedAfterPushMessage = "dispatch architecture: imported after a push"
	// importAfterPushFailedMessage is a push worker's WARN line for each import that failed while
	// the process ran.
	importAfterPushFailedMessage = "dispatch architecture: import after a push failed"
	// pushesUncoveredMessage is a push worker's WARN line for the pushes no import covered before
	// the process stopped.
	pushesUncoveredMessage = "dispatch architecture: the process stopped before an import covered these pushes; the next process's five-minute import repairs them"
)

// pushedSource is the pushes of one source that no import has covered yet. Its entry in
// Importer.pushes exists exactly while the source's push worker runs: a push that finds the entry
// is that worker's to cover, and a push that finds none starts a worker.
type pushedSource struct {
	// branch is the branch the latest push named, which is the source's own; the worker's log
	// lines name it.
	branch string
	// at holds when each push not yet covered was recorded, oldest first. Each is stamped under
	// Importer.pushMu, so they are in order.
	at []time.Time
}

// Pushed starts an import of every enabled source, of every project, on repo and branch: a push
// of branch to repo as GitHub's push event names them. repo is `owner/name` in GitHub's case, and
// it matches a stored repository whatever the case, because a stored one is lower-cased; branch is
// the name without `refs/heads/`, and it matches exactly, because a git branch name is
// case-sensitive. A disabled source is not imported, as the five-minute import does not import it.
//
// Pushed records the push against each such source and returns without waiting for an import.
// The source's push worker, started when none runs, joins or starts an import whose head lookup
// was issued after the push, and looks again until no push it holds is newer than its last
// import's head lookup. So the pushes that arrive while an import runs, after its head lookup,
// start one more import between them, and a push that arrives before an import's head lookup is
// covered by that import and starts none: the rule Sync gives a caller.
//
// A nil importer and an importer with no App credentials start nothing and answer nil, with no
// line: architecture.Run logs once, as it starts, that the importer is idle. One whose Close has
// begun starts nothing either and answers nil before it reads a source, and logs one WARN line
// naming repo and branch (pushAfterCloseMessage). All Pushed knows there is that this process
// imports nothing for the push: a source on that repository and branch, if there is one, is
// imported again at its next five-minute pass, by whichever process runs it. Shutdown closes the
// importer before it waits for the intakes (cmd/dispatch/shutdown.go), so every push of a branch
// an intake hands over during that wait logs the line, a push to a repository no source is on
// included. A failed read of the sources is the error. The line and the error show repo and
// branch as githubapp.ShownBody shows them, because both are GitHub's text. The read is on the
// pool, so ctx must hold no connection of it.
func (i *Importer) Pushed(ctx context.Context, repo, branch string) error {
	if !i.HasApp() {
		return nil
	}
	if i.owner.Err() != nil {
		slog.Warn(pushAfterCloseMessage, "repo", githubapp.ShownBody(repo), "branch", githubapp.ShownBody(branch))
		return nil
	}
	rows, err := i.store.Pool.Query(ctx, `
		select project_key, repo from architecture_sources
		where enabled and lower(repo) = lower($1) and branch = $2
		order by project_key, id
	`, repo, branch)
	var sources []EnabledSource
	if err == nil {
		sources, err = pgx.CollectRows(rows, pgx.RowToStructByPos[EnabledSource])
	}
	if err != nil {
		return fmt.Errorf("read the architecture sources on %s at branch %s: %w",
			githubapp.ShownBody(repo), githubapp.ShownBody(branch), err)
	}
	for _, source := range sources {
		i.recordPush(source, branch)
	}
	return nil
}

// recordPush records a push of branch against source, now, and starts the source's push worker
// when none runs. The worker is counted in i.running before its goroutine starts, so Close waits
// for it; when the count refuses it, because Close has begun, no worker starts and nothing is
// recorded, and one WARN line names the source and branch (pushRefusedMessage): this process does
// not import the source for the push, and the source is imported again at its next five-minute
// pass, by whichever process runs it. A push that finds the worker running is that worker's,
// which names it if the process stops first (importPushes).
func (i *Importer) recordPush(source EnabledSource, branch string) {
	key := sourceKey(source.Project, source.Repo)
	i.pushMu.Lock()
	defer i.pushMu.Unlock()
	pending, running := i.pushes[key]
	if !running {
		if !i.running.begin() {
			slog.Warn(pushRefusedMessage,
				"project", source.Project, "repo", githubapp.ShownBody(source.Repo), "branch", githubapp.ShownBody(branch))
			return
		}
		pending = &pushedSource{}
		i.pushes[key] = pending
		go i.importPushes(source)
	}
	pending.branch = branch
	pending.at = append(pending.at, time.Now())
}

// importPushes is the push worker of source, whose entry in i.pushes is under its sourceKey. It
// runs on i.owner, as a flight does, because the caller that handed the push over does not wait
// for the import. Each round reads the newest push the entry holds, imports the source as a caller
// of Sync that asked at that push's moment, and drops every push the import covered
// (coverPushes). The worker starts another round while a push is left, and returns when none is,
// and when the process is stopping. A source deleted since the push answers ErrNoSource, which
// covers the pushes as any answer does while the process runs, so the worker returns, unless a
// push recorded after that answer names the source again (a source added again under the same
// project and repository), which it then imports.
//
// One INFO line is logged for each import and one WARN line for each failed one; the failure is
// already on the source row, as the timer's failures are, so the line is for the operator. A
// worker that returns because the process is stopping, with pushes no completed import covered
// (coverPushes says which answers cover none), logs one WARN line naming the source, its branch
// and how many pushes, which the next process's five-minute import repairs, and no failure line,
// since the error may be the stop's own. Whether the process is stopping is read once, by
// coverPushes under i.pushMu, and the lines follow that one reading: read again here, a Close
// that began after coverPushes counted a failed import's pushes as covered would skip the failure
// line as well, and nothing would be logged of an import that failed while the process ran.
func (i *Importer) importPushes(source EnabledSource) {
	defer i.running.done()
	key := sourceKey(source.Project, source.Repo)
	for {
		i.pushMu.Lock()
		pending := i.pushes[key]
		newest, branch := pending.at[len(pending.at)-1], pending.branch
		i.pushMu.Unlock()
		synced, servedAt, err := i.syncAfter(i.owner, source.Project, source.Repo, newest)
		covered, uncovered, stopping, again := i.coverPushes(key, servedAt, err == nil || errors.Is(err, ErrNoSource))
		switch {
		case err == nil:
			commit := ""
			if synced.LastCommit != nil {
				commit = *synced.LastCommit
			}
			slog.Info(importedAfterPushMessage, "project", source.Project,
				"repo", githubapp.ShownBody(source.Repo), "branch", githubapp.ShownBody(branch),
				"commit", commit, "pushes", covered)
		case errors.Is(err, ErrNoSource), stopping:
			// A deleted source has nothing to import, and a stopping process reports what it
			// left below rather than the error, which may be the stop's own.
		default:
			slog.Warn(importAfterPushFailedMessage, "project", source.Project,
				"repo", githubapp.ShownBody(source.Repo), "branch", githubapp.ShownBody(branch),
				"pushes", covered, "error", err)
		}
		if uncovered > 0 {
			slog.Warn(pushesUncoveredMessage,
				"project", source.Project, "repo", githubapp.ShownBody(source.Repo), "branch", githubapp.ShownBody(branch),
				"pushes", uncovered)
		}
		if !again {
			return
		}
	}
}

// coverPushes drops from key's entry every push recorded no later than servedAt, the moment the
// import just answered was served at, and answers how many it dropped. When no push is left, it
// removes the entry and answers that the worker returns. It reads once whether the process is
// stopping and answers that reading as stopping, which the worker's lines follow (importPushes).
// When the process is stopping it removes the entry too, whatever is left, and answers that the
// worker returns, beside how many pushes no completed import covered: those left, or, where whole
// is false, every push the entry held.
//
// whole reports that the import's answer was not cut short: the import answered nil, or
// ErrNoSource, which imported nothing but is the whole answer of a source that is gone. Any other
// error answered while the process stops may be Close's cancel, and such an answer covers no
// push. A flight that Close cut short, or one whose begin the count refused because Close had
// begun (flight), answers its startedAt or its head lookup's moment beside the cancel error, and
// syncAfter can hand the worker that answer, whose moment is no earlier than the newest push the
// worker read, so it would cover that push and every one before it although nothing was recorded.
// The worker's own wait, syncAfter on i.owner, answers a zero time when i.owner is already done
// as it is called or ends before the flight answers, which covers no push in any case. While the
// process runs whole changes nothing: a failed import's answer covers pushes as any answer does.
//
// The decision and a push's record (recordPush) are both taken under i.pushMu, so a push recorded
// after it finds no entry and starts a worker of its own, or, once Close has begun, is named by
// recordPush: no push falls between a worker's last look and its exit.
func (i *Importer) coverPushes(key string, servedAt time.Time, whole bool) (covered, uncovered int, stopping, again bool) {
	i.pushMu.Lock()
	defer i.pushMu.Unlock()
	pending := i.pushes[key]
	stopping = i.owner.Err() != nil
	if whole || !stopping {
		for covered < len(pending.at) && !pending.at[covered].After(servedAt) {
			covered++
		}
		pending.at = pending.at[covered:]
	}
	if !stopping && len(pending.at) > 0 {
		return covered, 0, false, true
	}
	delete(i.pushes, key)
	return covered, len(pending.at), stopping, false
}
