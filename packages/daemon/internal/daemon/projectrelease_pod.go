package daemon

// An issue pod's path through a project release (projectReleaser.takePod): its reads, the ask to
// its running roles and the wait for their answers, then its stop and its delete, or its leaving
// with its claims held for an operator (leave, hold).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// readAll reads every pod the release reads at once and answers each pod's read by its place in
// pods: the dry run's reads, which ask nothing after them. A pod classified with no running role
// is read through its launcher (launcherRead), as the release reads it before its delete, and
// every other pod it reads through a running role, or through its launcher where no role process
// runs by then (readPod). The release reads each pod on that pod's own goroutine (takePod).
func (r *projectReleaser) readAll(ctx context.Context, run *releaseRun, pods []*releasePod) []releaseRead {
	reads := make([]releaseRead, len(pods))
	var wg sync.WaitGroup
	for i, pod := range pods {
		if pod.class == releaseLeaves {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := store.ForConcurrentWorker(ctx)
			if pod.class == releaseUnread {
				reads[i] = r.launcherRead(ctx, pod.sandbox.Issue)
			} else {
				reads[i] = r.readPod(ctx, pod.sandbox.Issue)
			}
			r.noteRead(run, pod, reads[i])
		}()
	}
	wg.Wait()
	return reads
}

// readPod is the dry run's one read of a read pod: through one running role's shim (read), or,
// where no role process runs there by then, through the pod's launcher (launcherRead), as the
// release reads at the end a pod no read through a role reached.
func (r *projectReleaser) readPod(ctx context.Context, issue string) releaseRead {
	if read := r.read(ctx, issue); read.read {
		return read
	}
	return r.launcherRead(ctx, issue)
}

// read reads issue's workspace through one running role's shim, the first by token, with what
// the shim tells the issue's own commits by (unpushedRead). A pod where no role process runs now
// is unread here; one no such read reaches is read through its launcher before its delete
// (launcherRead). A shim that refused, a read that failed, and a read the daemon could not make are
// a refusal, which asks the pod's roles, whose text keeps the start of the error's, and the log the
// whole of it, in parts that name the issue and the role read (keepWhole), at each read refused;
// the parts name no release, since a dry run's reads have none.
func (r *projectReleaser) read(ctx context.Context, issue string) releaseRead {
	running := runningClaims(r.issueViews(issue))
	if len(running) == 0 {
		return releaseRead{}
	}
	role := running[0].Role
	request, err := r.unpushedRead(ctx, issue)
	var work runtime.UnpushedWork
	if err == nil {
		work, err = r.pods.ReadUnpushed(ctx, request, role)
	}
	if err != nil {
		return releaseRead{read: true, refusal: r.keepWhole(err.Error(), "issue", issue, "role", role)}
	}
	return releaseRead{read: true, work: work}
}

// launcherRead reads issue's workspace through the pod's launcher (runtime.PodWorkspaceReader),
// the read of a pod no read through a role's shim reached: the launcher runs in the pod whether or
// not a role process does, and answers the shim's `workspace-work` frame, as it answers the idle
// release's check of a pod no claim owns. Its count is the commits under a working copy or a local
// bookmark that no remote branch or tag holds (workspace.WorkRevset), which is narrower than the
// release's own read through a role (internal/shim's unpushedRevset): that read also counts each
// commit with a change and no description, and, until the issue's pull request merges, each
// commit whose trailer names a session of the issue's claims or the issue. A launcher that does
// not answer or refuses, and a pod the runtime cannot name or holds none of, are a read not made,
// whose error is kept as a far side's error is (keepWhole), in parts that name the issue and the
// launcher.
func (r *projectReleaser) launcherRead(ctx context.Context, issue string) releaseRead {
	work, err := r.pods.PodWorkspaceWork(ctx, issue)
	if err != nil {
		return releaseRead{launcher: true, refusal: r.keepWhole(err.Error(), "issue", issue, "read", "launcher")}
	}
	return releaseRead{read: true, launcher: true, work: runtime.UnpushedWork{Commits: work.Count, WorkingCopyChanged: work.Changed}}
}

// unpushedRead is what the read of issue's workspace carries to the shim (runtime.UnpushedRead):
// the issue, whether the record holds its pull request merged, and each session name a claim of
// the issue holds (claim_sessions), sorted and each once, which is how the shim tells a side head
// of the issue's own by its `Omp-Session` trailer. Every claim of the issue is read, one a role,
// whatever its state: a retired claim's session made commits of the issue too. A name of kind
// `session` or `file-id` is a session id; a `file-name` is a file's base name, never a trailer's
// value, so it is left out, and so is a name that claim.SessionIDRule refuses, since `legion git`
// and jj's trailer writer give such an id no trailer. Claims that hold more names than the frame
// carries (shimwire.MaxReadUnpushedSessions) are an error, as a names or record read that failed
// is: the release reads nothing then, and the refusal asks the pod's roles, where a list cut to
// the bound could leave a commit of the issue uncounted and the pod deleted with it.
func (r *projectReleaser) unpushedRead(ctx context.Context, issue string) (runtime.UnpushedRead, error) {
	merged, err := r.store.PullRequestMerged(ctx, issue)
	if err != nil {
		return runtime.UnpushedRead{}, fmt.Errorf("read whether the pull request of %s merged, for the read of its workspace: %w", issue, err)
	}
	var sessions []string
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, issue, role)
		if err != nil {
			return runtime.UnpushedRead{}, fmt.Errorf("derive the %s claim of %s: %w", role, issue, err)
		}
		holds, err := r.store.ClaimHolds(ctx, r.project, token)
		if err != nil {
			return runtime.UnpushedRead{}, fmt.Errorf("read the session names %s holds, for the read of %s's workspace: %w", token, issue, err)
		}
		for _, h := range holds {
			if (h.Kind == supervise.HoldSessionID || h.Kind == supervise.HoldFileID) && claim.SessionIDRule.MatchString(h.Name) {
				sessions = append(sessions, h.Name)
			}
		}
	}
	slices.Sort(sessions)
	sessions = slices.Compact(sessions)
	if len(sessions) > shimwire.MaxReadUnpushedSessions {
		return runtime.UnpushedRead{}, fmt.Errorf("the claims of %s hold %d session names, more than the %d a read of its workspace carries, so the daemon did not read it",
			issue, len(sessions), shimwire.MaxReadUnpushedSessions)
	}
	return runtime.UnpushedRead{Issue: issue, Merged: merged, Sessions: sessions}, nil
}

// noteRead writes what read found on pod's line, as the line reports it at the end.
func (r *projectReleaser) noteRead(run *releaseRun, pod *releasePod, read releaseRead) {
	run.mu.Lock()
	defer run.mu.Unlock()
	writeRead(&run.result.Sandboxes[pod.line], read)
}

// writeRead sets line's count, flag and refusal from read, and clears what an earlier read left;
// the last read of a deleted pod that found no role process clears nothing (writeDeleted).
// Unpushed is decided by the count of commits alone: a role that pushed its working-copy commit
// itself reads no commit and a changed working copy, which is nothing unpushed, and the flag is
// reported as what the working copy held at that read. A read through the pod's launcher writes
// the count and the flag it found, or neither where it was not made, never a refusal, which is a
// shim's, and the line's reason names that read (launcherReason).
func writeRead(line *api.ReleasedSandbox, read releaseRead) {
	line.Commits, line.WorkingCopyChanged, line.Refusal = nil, nil, ""
	if read.launcher {
		line.Reason = launcherReason(read)
	}
	if !read.read {
		return
	}
	if read.refusal != "" {
		line.Refusal = read.refusal
		return
	}
	commits, changed := read.work.Commits, read.work.WorkingCopyChanged
	line.Commits, line.WorkingCopyChanged = &commits, &changed
}

// launcherReason is the reason of the line of a pod read through its launcher (launcherRead):
// that the launcher read it and counts the narrower set, or that its read was not made and why.
func launcherReason(read releaseRead) string {
	if !read.read {
		return releaseLauncherUnread + read.refusal
	}
	return releaseLauncherRead
}

// asks is whether read, with claims as they stand, is a pod the release asks: its workspace holds
// unpushed work, its shim refused, a role of it is in a turn, or a role of it holds the pod by its
// activity report (background), as an update round's replacement is held since
// cybermonklabs/legion#4556. It answers that hold too, read only of a pod that nothing else asks,
// so it names a role only where the hold is why the pod is asked.
func (r *projectReleaser) asks(ctx context.Context, read releaseRead, claims []supervise.ClaimView) (bool, backgroundHold) {
	if !read.read {
		return false, backgroundHold{}
	}
	if read.work.Commits > 0 || read.refusal != "" || inTurn(claims) {
		return true, backgroundHold{}
	}
	hold := r.background(ctx, claims)
	return hold.holds(), hold
}

// background is the background hold over an issue pod whose claims are claims, read now by the
// rule an update round's replacement reads its own pod's by since cybermonklabs/legion#4556
// (backgroundHold.weigh): each claim that records a process and is not gone, whose activity
// report names background work that runs, or that has sent no report since its last turn
// started. The report is read from the store at the launch the claim's view holds
// (projectReleaseStore.ClaimActivity), as roundGroup reads it: the activity route writes the
// store alone, and a machine holds the report only as it read it at its own boot. A gone claim
// holds nothing, so its report is not read. A report the store could not read is read as none,
// which holds the pod as a session that has sent no report holds it.
func (r *projectReleaser) background(ctx context.Context, claims []supervise.ClaimView) backgroundHold {
	var hold backgroundHold
	for _, view := range claims {
		if view.Locator == nil || view.Gone() {
			continue
		}
		c := view.Claim
		report, _, err := r.store.ClaimActivity(ctx, c.Token, c.Generation)
		if err != nil {
			r.log.Warn("project release: read the activity report of a claim of the pod; it holds the pod as a claim with no report",
				"claim", c.Token, "generation", c.Generation, "error", err)
			report = supervise.ActivityReport{}
		}
		c.ActivityAt, c.ActivityBackground, c.ActivityKinds = report.At, report.Background, report.Kinds
		hold.weigh(c)
	}
	return hold
}

// takePod takes one issue pod to its end. A pod left from the start has its claims held for an
// operator and stays. A pod with no running role goes to its stop and its delete, where its
// launcher reads it (stopAndDelete, launcherRead). A read pod is read first. A pod that
// read asks for is asked and waits. One that the first read does not ask for is read again just
// before its stop, and a turn, a commit, a refusal or a role whose activity report holds the pod
// found then makes it asked, since a turn can start after the first read without the daemon (an
// Envoy message, a background job's result), and that turn can start background work.
// Then its last read, its stop and its delete. A pod whose ask, the first or the one after the
// last read, could not be written by the bound is left instead, its claims held for an operator:
// nobody asked its roles to push, so the work on its disk is not given up (R6, R8). So is a pod
// whose ask was written only at or after the bound, since its roles then had no time to push, and
// one whose ask reached no session of any of its roles by the bound (wait), since no session was
// asked to push: a role that said ready holds its Envoy topic only from its session's first turn
// or the first-turn bound. A pod whose every role stopped running after a try of its ask failed
// and before the next try is not left: a pod with no running role is asked by no one (ask), so it
// goes on to its stop and its delete unasked.
func (r *projectReleaser) takePod(ctx context.Context, run *releaseRun, pod *releasePod) {
	switch pod.class {
	case releaseLeaves:
		r.leave(ctx, run, pod, pod.why)
		return
	case releaseUnread:
		r.stopAndDelete(ctx, run, pod, releaseRead{})
		return
	}
	first := r.read(ctx, pod.sandbox.Issue)
	r.noteRead(run, pod, first)
	firstAsks, hold := r.asks(ctx, first, r.issueViews(pod.sandbox.Issue))
	if firstAsks {
		asked, err := r.ask(ctx, run, pod, hold)
		if asked {
			err = r.wait(ctx, run, pod)
		}
		if err != nil {
			r.leave(ctx, run, pod, err.Error())
			return
		}
	}
	last := r.read(ctx, pod.sandbox.Issue)
	lastAsks := false
	if !firstAsks {
		lastAsks, hold = r.asks(ctx, last, r.issueViews(pod.sandbox.Issue))
	}
	if lastAsks {
		asked, err := r.ask(ctx, run, pod, hold)
		if asked || err != nil {
			r.noteRead(run, pod, last)
		}
		if asked {
			err = r.wait(ctx, run, pod)
		}
		if err != nil {
			r.leave(ctx, run, pod, err.Error())
			return
		}
		if asked {
			last = r.read(ctx, pod.sandbox.Issue)
		}
	}
	if ctx.Err() != nil {
		return
	}
	r.stopAndDelete(ctx, run, pod, last)
}

// ask asks each running role of pod (askOnce) and answers whether any role was asked, which is
// whether the pod waits for answers. A deployment with no outbox asks none, and neither does a pod
// where no role runs, at the first try or at any later one: a pod whose every role stopped running
// after a failed try is asked by no one. An ask whose rows could not be written is written again,
// retry after the first try and up to projectReleaseRetryMax apart, each try to the roles running
// then, until the bound: one that could not be written at the try at the bound reached no role,
// and its last error is answered after releaseAskUnwritten. An ask written only at or after the
// bound, at any try, left its roles no time to answer, and the bound is passed on the pod's line
// at once (atBound): a role the ask reached by the bound stays asked with the bound passed, and
// every other one is taken off the line, which takes every role of rows written at or after the
// bound, since no session can have had those by it. releaseAskAtBound is answered then. Either
// error is the reason the pod is left for (takePod). A release whose context ends meanwhile asks
// none. hold is the background hold the ask decision read (asks), which the line of a written ask
// names (askOnce).
func (r *projectReleaser) ask(ctx context.Context, run *releaseRun, pod *releasePod, hold backgroundHold) (bool, error) {
	wait := r.retry
	for {
		asked, err := r.askOnce(ctx, run, pod, hold)
		if err == nil {
			if asked && time.Until(run.bound) <= 0 {
				r.atBound(run, pod)
				r.log.Error("project release: enqueue the ask to the pod's roles; it was written only at the bound, which left its roles no time to answer, so the release leaves the pod",
					"release", run.id, "sandbox", pod.sandbox.Name, "bound", run.bound.Format(time.RFC3339))
				return false, errors.New(releaseAskAtBound)
			}
			return asked, nil
		}
		if ctx.Err() != nil {
			return false, nil
		}
		left := time.Until(run.bound)
		if left <= 0 {
			r.log.Error("project release: enqueue the ask to the pod's roles; it could not be written by the bound, so no role of the pod is asked and the release leaves the pod",
				"release", run.id, "sandbox", pod.sandbox.Name, "error", err)
			return false, fmt.Errorf("%s: %w", releaseAskUnwritten, err)
		}
		pause := min(wait, left)
		r.log.Warn("project release: enqueue the ask to the pod's roles; writing it again",
			"release", run.id, "sandbox", pod.sandbox.Name, "retryIn", pause.String(), "error", err)
		if backoff.Sleep(ctx, pause) != nil {
			return false, nil
		}
		wait = min(2*wait, projectReleaseRetryMax)
	}
}

// askOnce sends one ask to each role of pod running now, on the role's own Envoy topic
// (record.UpdateAsk under the release's round, which the outbox publishes without the claim-round
// read while the release runs), and records each asked role on the pod's line before the rows are
// written, so an answer that comes at once finds it. It answers whether any role was asked: none
// where no role runs or the deployment has no outbox. Rows that could not be written are answered
// by their error, and their roles are taken off the line and out of run.asked again, so no role
// they never reached reads as asked. Rows the outbox writes none of, those of a pod whose issue is
// another project's (supervision.enqueue), are answered as rows that could not be written
// (claim.ErrKeyOfAnotherProject), so such a pod is left at the bound rather than deleted with no
// role asked. A role whose row was written counts as asked once the outbox's publish of it reached
// a session of the role (reach), or once the role answered; one the ask reached no session of by
// the bound, and that had not answered, is taken off the line then (atBound). The line it logs for
// a written ask names the roles of hold (backgroundHold.attributes), the reports that asked a pod
// nothing else asked. Each row carries the text built for its role (supervise.ReleaseTask).
func (r *projectReleaser) askOnce(ctx context.Context, run *releaseRun, pod *releasePod, hold backgroundHold) (bool, error) {
	running := runningClaims(r.issueViews(pod.sandbox.Issue))
	if len(running) == 0 {
		return false, nil
	}
	if r.enqueue == nil {
		r.log.Info("project release: this deployment has no outbox, so no role of the pod is asked",
			"release", run.id, "sandbox", pod.sandbox.Name)
		return false, nil
	}
	round := projectReleaseRound(run.id)
	rows := make([]record.OutboxPayload, len(running))
	for i, c := range running {
		text := supervise.ReleaseTask(c.Role, round, pod.sandbox.Issue, run.bound)
		rows[i] = record.UpdateAsk{Role: c.Role, Round: round, Pod: pod.sandbox.PodUID, Text: text}
	}
	run.mu.Lock()
	line := &run.result.Sandboxes[pod.line]
	before := len(line.Asked)
	for _, c := range running {
		run.asked[c.Token] = releaseAsk{line: pod.line, role: c.Role, nudge: pod.nudge}
		line.Asked = append(line.Asked, api.ReleasedAsk{Role: c.Role})
	}
	run.mu.Unlock()
	written, err := r.enqueue(ctx, pod.sandbox.Issue, rows...)
	if err == nil && !written {
		err = fmt.Errorf("the asks to the roles of %s: %w", pod.sandbox.Issue, claim.ErrKeyOfAnotherProject)
	}
	if err != nil {
		run.mu.Lock()
		line = &run.result.Sandboxes[pod.line]
		line.Asked = line.Asked[:before]
		for _, c := range running {
			delete(run.asked, c.Token)
		}
		run.mu.Unlock()
		return false, err
	}
	r.log.Info("project release: asked each running role of the pod to push what it keeps and end its turn",
		append([]any{"release", run.id, "sandbox", pod.sandbox.Name, "roles", len(rows), "bound", run.bound.Format(time.RFC3339)},
			hold.attributes()...)...)
	r.record(ctx, run)
	return true, nil
}

// wait holds an asked pod until each asked role answered and no role of the pod is in a turn or
// holds the pod by its activity report (background), or until the bound, on a wall-clock timer, so
// a test whose machines run on a clock that never fires still sees the bound: the pod is read
// again every reread and at once on an answer. The report is read only once every asked role
// answered and no turn runs, so it is what holds a pod whose roles all answered and whose turns
// all ended while a subagent or a background job of a role may still run in it, or while a role
// has sent no report since its last turn started. A role whose ask reached no session of it yet
// has nothing to answer, so the pod waits for it until the bound: the outbox publishes that role's
// ask again until then, and the session may take its topic meanwhile. At the bound the bound is
// passed on the pod's line (atBound), whatever the reports say, and wait answers
// releaseAskUnreached when the line then keeps no role, since the ask reached none of the pod's
// roles by then and none answered, which leaves the pod (takePod), and nil otherwise. A wait that
// ends before the bound, on answers or on the context's end, answers nil.
func (r *projectReleaser) wait(ctx context.Context, run *releaseRun, pod *releasePod) error {
	bound := time.NewTimer(time.Until(run.bound))
	defer bound.Stop()
	reread := time.NewTicker(r.reread)
	defer reread.Stop()
	for {
		if run.answeredAll(pod) {
			claims := r.issueViews(pod.sandbox.Issue)
			if !inTurn(claims) && !r.background(ctx, claims).holds() {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-bound.C:
			kept := r.atBound(run, pod)
			r.record(ctx, run)
			if !kept {
				return errors.New(releaseAskUnreached)
			}
			return nil
		case <-reread.C:
		case <-pod.nudge:
		}
	}
}

// answeredAll is whether every role asked on pod's line has answered.
func (run *releaseRun) answeredAll(pod *releasePod) bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, asked := range run.result.Sandboxes[pod.line].Asked {
		if asked.AnsweredAt == nil {
			return false
		}
	}
	return true
}

// reach records that the ask to token's claim reached a session of its role at now, the hook the
// outbox calls once its publish of the ask did (releaseAskHook). A reach at or after the bound
// counts for nothing: it left the role no time to answer, and the pod's line names only the roles
// the ask reached by the bound (passBound). A claim the release has not asked, or has taken off
// its pod's line, is passed over.
func (run *releaseRun) reach(token claim.Token, now time.Time) {
	run.mu.Lock()
	defer run.mu.Unlock()
	asked, ok := run.asked[token]
	if !ok || !now.Before(run.bound) {
		return
	}
	asked.reached = true
	run.asked[token] = asked
}

// atBound passes the bound on pod's line (passBound) and logs each role it took off the line, one
// line each, and answers whether the line keeps any role: one the ask reached by the bound, or one
// that answered.
func (r *projectReleaser) atBound(run *releaseRun, pod *releasePod) bool {
	dropped, kept := run.passBound(pod)
	for _, token := range dropped {
		r.log.Warn("project release: the ask reached no session of the role by the bound, so the pod's line does not name it asked",
			"release", run.id, "sandbox", pod.sandbox.Name, "claim", token)
	}
	return kept
}

// passBound passes the bound on pod's line: each asked role whose ask reached no session of it by
// then, and that did not answer (releaseAsk.reached), is taken off the line and out of run.asked,
// since no session of it was asked, and each other one that has not answered records the bound.
// It answers the claims it took off, in the line's order, and whether any asked role is left on
// the line, which is whether the ask reached any role of the pod or any role answered.
func (run *releaseRun) passBound(pod *releasePod) (dropped []claim.Token, kept bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	onLine := map[claim.Role]claim.Token{}
	for token, asked := range run.asked {
		if asked.line == pod.line {
			onLine[asked.role] = token
		}
	}
	line := &run.result.Sandboxes[pod.line]
	line.Asked = slices.DeleteFunc(line.Asked, func(ask api.ReleasedAsk) bool {
		token := onLine[ask.Role]
		if ask.AnsweredAt != nil || run.asked[token].reached {
			return false
		}
		delete(run.asked, token)
		dropped = append(dropped, token)
		return true
	})
	for i := range line.Asked {
		if line.Asked[i].AnsweredAt == nil {
			line.Asked[i].Bound = true
		}
	}
	return dropped, len(line.Asked) > 0
}

// stopAndDelete ends one issue pod after its last read: each claim of the stop set in it is asked
// a project release's suspension, bounded by the release's bound, and the release waits until
// each is stored suspended, so no deletion of the pod reads as a role's death. A claim that failed
// or retired meanwhile holds no process and is settled as it stands (stopSettled). A claim stored
// suspended by an idle release (supervise.SuspendedByReleased) is asked once more, so the project
// release's suspension takes over that hold by rank and the boot starts the claim; no other
// source ranks below the project release's (supervise's suspensionRank), so none other is asked.
// Then one of two ends. A pod a claim of which an operator or the project's controller suspended
// meanwhile (heldBy), or whose claims were not all stored settled by the stop's deadline
// (stopDeadline), is left, each of its claims held for an operator so that no boot starts it.
// Otherwise a pod no read through a role's shim reached is read through its launcher
// (launcherRead, unreached), and that read is its last. Then the pod is deleted
// (IssueSuspender.SuspendIssue), with an authorize the runtime calls under its pod lock that holds
// only while the release runs and no suspension of the operator's or the controller's is over a
// claim of the pod, and whose false answer leaves the pod as well, and with the settle of the
// standing records of the replacements of the pod, which the runtime runs under the same lock
// before its first write (settleReplacements): a record it cannot settle leaves the pod, for a
// reason that names it. A deleted pod's line takes its last read and its outcome (writeDeleted),
// and the pod's hold streaks end: no hold stands on a pod that is gone, and the idle pass, which
// would end them at its next read, is held while the mark stands, so until the next boot the
// state would show the deleted pod's hold (store.StandingPodHoldsTx). A streak that cannot be
// ended is logged, and stands until the idle pass reads the issue again after the next boot.
func (r *projectReleaser) stopAndDelete(ctx context.Context, run *releaseRun, pod *releasePod, last releaseRead) {
	tokens := releaseStopTokens(pod.stop)
	pending := r.stopClaims(ctx, run, tokens, "the project release stops it to delete its pod")
	if ctx.Err() != nil {
		return
	}
	for _, token := range tokens {
		if _, waiting := pending[token]; waiting {
			continue
		}
		m, ok := r.machines.Machine(token)
		if !ok {
			continue
		}
		if v := m.View(); v.State == supervise.StateSuspended && v.SuspendedBy == supervise.SuspendedByReleased {
			if err := m.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByProjectRelease,
				Reason: "the project release takes over its suspension"}); err != nil {
				r.log.Warn("project release: take over a claim's suspension", "claim", token, "error", err)
			}
		}
	}
	if why := heldBy(r.issueViews(pod.sandbox.Issue)); why != "" {
		r.leave(ctx, run, pod, why+" during the release")
		return
	}
	if len(pending) > 0 {
		r.leave(ctx, run, pod, "its claims were "+r.stopWaitRanOut()+": "+pendingWords(pending))
		return
	}
	if !last.read && run.unreached(pod) {
		last = r.launcherRead(ctx, pod.sandbox.Issue)
	}
	authorized, unsettled := false, ""
	err := r.pods.SuspendIssue(ctx, pod.sandbox.Issue, pod.sandbox.Tree, func(context.Context) (bool, error) {
		authorized = ctx.Err() == nil && !r.mark.finished() && heldBy(r.issueViews(pod.sandbox.Issue)) == ""
		return authorized, nil
	}, func(ctx context.Context, read runtime.ActRead) error {
		if unsettled = r.settleReplacements(ctx, pod.sandbox.Issue, read); unsettled != "" {
			return errors.New(unsettled)
		}
		return nil
	})
	switch {
	case unsettled != "":
		r.leave(ctx, run, pod, unsettled)
		return
	case err != nil:
		r.leave(ctx, run, pod, "its delete failed: "+err.Error())
		return
	case !authorized:
		r.leave(ctx, run, pod, "an operator or the controller suspended one of its claims as its delete began")
		return
	}
	run.mu.Lock()
	line := &run.result.Sandboxes[pod.line]
	writeDeleted(line, last)
	outcome := line.Outcome
	run.mu.Unlock()
	r.log.Info("project release: deleted an issue pod", "release", run.id, "sandbox", pod.sandbox.Name, "outcome", outcome)
	if err := r.store.EndPodHoldStreaks(ctx, r.project, pod.sandbox.Issue); err != nil {
		r.log.Warn("project release: could not end a deleted pod's hold streaks", "release", run.id, "issue", pod.sandbox.Issue, "error", err)
	}
	r.record(ctx, run)
}

// unreached is whether no read of pod through a role's shim reached a role process: each such read
// writes a count or a refusal on the pod's line (writeRead), and a last read that found no role
// process writes nothing (writeDeleted), so a line with neither is a pod no such read reached.
func (run *releaseRun) unreached(pod *releasePod) bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	line := run.result.Sandboxes[pod.line]
	return line.Commits == nil && line.Refusal == ""
}

// settleReplacements settles each standing record of a replacement of issue's pod
// (store.PodReplacement) before the release deletes that pod, under the pod's launch turn the
// runtime holds for the delete (IssueSuspender's settle), from read, the runtime's read of the
// record's act under that turn (runtime.ActRead; ActReader would find the turn busy). Such a
// record is a replacement the daemon recorded before the runtime acted, which the release's mark
// left standing: a launch's replacement as the round's own whose launch the runtime refused for
// the mark once roundPodEndsAtLaunch had read it clear, or a round's call that failed otherwise,
// one that ran out waiting for the pod's launch turn among them (replaceForRound), whose record no
// sweep reads while the mark holds. Once the pod is gone, the next boot's sweep would read such an
// act made (finishReplacements) and tell the issue's architect of a replacement that took nothing.
//
//   - An act not made, its target still in use, is forgotten (store.Store.ForgetPodReplacement),
//     as the sweep forgets it, with one line naming the issue, the pod and the operation.
//   - An act made stands: it took an earlier pod, and the next boot's notice of it is true.
//   - An act the runtime cannot say was made, a read that fails and a store that does not answer
//     leave the pod: the release does not delete over a record it cannot settle.
//
// It answers "" once every record of the issue is settled, and otherwise why the pod is left,
// naming the record's operation and what was uncertain or failed. Every record of the issue is
// read before any is forgotten, so a pod left for one record forgets none.
func (r *projectReleaser) settleReplacements(ctx context.Context, issue string, read runtime.ActRead) string {
	standing, err := r.store.PodReplacements(ctx, r.project)
	if err != nil {
		return "the records of the replacements of its pod could not be read, and the release does not delete it over a record it cannot settle: " + err.Error()
	}
	var notMade []store.PodReplacement
	for _, recorded := range standing {
		if recorded.Issue != issue {
			continue
		}
		state, err := read(ctx, recorded.Act)
		switch {
		case err != nil:
			return fmt.Sprintf("whether the recorded replacement %s of its pod was made could not be read, and the release does not delete the pod over a record it cannot settle: %v",
				recorded.Operation, err)
		case state == runtime.ActUncertain:
			return fmt.Sprintf("the runtime cannot say whether the recorded replacement %s of its pod was made, and the release does not delete the pod over a record it cannot settle",
				recorded.Operation)
		case state == runtime.ActNotMade:
			notMade = append(notMade, recorded)
		}
	}
	for _, recorded := range notMade {
		if err := r.store.ForgetPodReplacement(ctx, r.project, issue, recorded.Operation); err != nil {
			return fmt.Sprintf("the record of the replacement %s of its pod, which took nothing, could not be forgotten, and the release does not delete the pod over it: %v",
				recorded.Operation, err)
		}
		r.log.Info("project release: forgot the record of a replacement of the issue's pod that took nothing, before it deletes the pod",
			"issue", issue, "pod", recorded.PodUID, "operation", recorded.Operation)
	}
	return ""
}

// writeDeleted writes on line the last read of a pod the release deleted, and the line's outcome
// (deletedOutcome). A last read that found no role process clears nothing: the count, the flag
// and the refusal an earlier read wrote are the last the release knows of the workspace, which
// went with the pod unread at the end, so the outcome is deleted_read_refused with
// releaseNoRoleAtLastRead as its reason. A pod no read through a role's shim reached was read
// through its launcher (launcherRead), and its line takes that read and its reason (writeRead).
func writeDeleted(line *api.ReleasedSandbox, last releaseRead) {
	if !last.read && !last.launcher && (line.Commits != nil || line.Refusal != "") {
		line.Outcome, line.Reason = api.ReleaseDeletedReadRefused, releaseNoRoleAtLastRead
		return
	}
	writeRead(line, last)
	line.Outcome = deletedOutcome(last)
}

// deletedOutcome is the outcome of a pod deleted after read, its one read or its last:
// deleted_unread where no read was made, which is the launcher's read of a pod no read through a
// role reached, not answered or refused (launcherRead); deleted_read_refused where a shim refused;
// deleted_lost where the read counted a commit; and deleted_clean otherwise.
func deletedOutcome(read releaseRead) api.ReleaseOutcome {
	switch {
	case !read.read:
		return api.ReleaseDeletedUnread
	case read.refusal != "":
		return api.ReleaseDeletedReadRefused
	case read.work.Commits > 0:
		return api.ReleaseDeletedLost
	}
	return api.ReleaseDeletedClean
}

// leave is a pod left with its disk, for why: each claim of the stop set in it is held for an
// operator, so no boot starts any of them (R6, R8), and every claim of the pod is taken out of its
// update round (hold). The release waits for each hold to be stored before it finishes, and a
// refusal of one is written on that claim's line. The line gives why as its first reason, and after
// it each round the release could not end. Why can carry a far side's error text — a delete the
// runtime refused, an ask the store did not take, a stop a machine refused — so the line, the
// hold's reason and the log line keep its start, and the log the whole of it (keepWhole).
func (r *projectReleaser) leave(ctx context.Context, run *releaseRun, pod *releasePod, why string) {
	kept := r.keepWhole(why, "release", run.id, "sandbox", pod.sandbox.Name)
	held := releaseStopTokens(pod.stop)
	var others []claim.Token
	for _, v := range r.issueViews(pod.sandbox.Issue) {
		if !slices.Contains(held, v.Token) {
			others = append(others, v.Token)
		}
	}
	unended := r.hold(ctx, run, held, others, "the project release leaves its pod: "+kept)
	run.mu.Lock()
	line := &run.result.Sandboxes[pod.line]
	reason := strings.Join(append([]string{kept}, unended...), reasonSeparator)
	line.Outcome, line.Reason = api.ReleaseLeft, reason
	run.mu.Unlock()
	r.log.Warn("project release: left an issue pod and its disk", "release", run.id, "sandbox", pod.sandbox.Name, "reason", reason)
	r.record(ctx, run)
}

// hold asks each of held an operator's suspension for reason, which takes over a project
// release's by rank and holds one still in flight, so no boot starts the claim, and adds it to the
// claims finish waits for (run.held), with a refusal written to run.heldRefused. Then each claim of
// held and of others that records an update round is taken out of it, as the round's end takes it
// (supervise.PodUpdateEnded): the release leaves no round open on any claim (R6), and a round left
// on a claim of a pod the release left could replace that pod, and the disk the release left it
// to keep, once an operator's resume lifts the last hold (podupdate.go's heldClaims). The
// round goes after the suspension, so the task the round held back is handed to no role the
// release is stopping. A claim of others that records no round has nothing done to it. Each
// claim's requests run on a goroutine of its own, under a store mark of its own, and hold returns
// once each was answered, with a sentence for each round it could not end, which the caller
// writes on the Sandbox's line: the sentence keeps the round end's error as the store keeps a far
// side's error text (storetext.FarSideError).
func (r *projectReleaser) hold(ctx context.Context, run *releaseRun, held, others []claim.Token, reason string) []string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var unended []string
	take := func(token claim.Token, holds bool) {
		m, ok := r.machines.Machine(token)
		if !ok {
			if holds {
				run.heldRefused.set(token, releaseNoMachine)
			}
			return
		}
		if !holds && m.View().UpdateRound == "" {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := store.ForConcurrentWorker(ctx)
			if holds {
				requestSuspend(ctx, m, token, supervise.RequestSuspend{By: supervise.SuspendedByOperator, Reason: reason}, run.heldRefused)
			}
			if round := m.View().UpdateRound; round != "" {
				if err := m.Handle(ctx, supervise.PodUpdateEnded{Claim: token, Round: round}); err != nil {
					r.log.Warn("project release: take a claim of a Sandbox it leaves out of its update round",
						"release", run.id, "claim", token, "round", round, "error", err)
					mu.Lock()
					unended = append(unended, fmt.Sprintf("%s is still in pod update round %s, which the release could not end: %s",
						token, round, storetext.FarSideError(err.Error())))
					mu.Unlock()
				}
			}
		}()
	}
	for _, token := range held {
		take(token, true)
	}
	for _, token := range others {
		take(token, false)
	}
	wg.Wait()
	slices.Sort(unended)
	run.mu.Lock()
	run.held = append(run.held, held...)
	run.mu.Unlock()
	return unended
}
