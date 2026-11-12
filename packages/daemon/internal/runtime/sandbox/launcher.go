package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// launcherPodUIDAnnotation binds a role Secret's launcher token to the pod it was issued for; a
// launcher of any other pod is refused. annotationIssue carries the exact issue key (labels are
// lowercased), from which the resolver derives the claim token.
const (
	launcherPodUIDAnnotation = "legion.dev/launcher-pod-uid"
	annotationIssue          = "legion.dev/issue"
)

type launchers struct {
	mu       sync.Mutex
	sessions map[claim.Token]*launcherSession

	// copies reads the facts of the two copies of an issue's workspace the daemon may hold,
	// bundle and running are the take of each, and restored is the confirmation that drops the
	// one a launcher put back, the daemon's store behind all four (Options.SavedWorkspaceCopies,
	// SavedWorkspaceBundle, SavedRunningWorkspaceCopy, WorkspaceBundleRestored). A launchers
	// built without the set restores nothing.
	copies   runtime.SavedWorkspaceCopies
	bundle   runtime.SavedWorkspaceBundle
	running  runtime.SavedWorkspaceBundle
	restored runtime.WorkspaceBundleRestored
	log      *slog.Logger
}

type launcherSession struct {
	owner *launchers
	token claim.Token
	// issue is the exact issue key this launcher's pod is of, as the role Secret's annotation
	// carried it to the hello's authentication (launcherCredential.issue): the key the release
	// saved that workspace's bundle under. "" for a pod of no issue, the project controller's.
	issue string
	// podCreated is when the API server stamped this launcher's pod created
	// (corev1.Pod's CreationTimestamp, as the resolver read it off the pod the connection was
	// accepted for). It is what says whether this pod is the one a release's saved work is for:
	// a pod created after the save is the pod the teardown's launch made and is sent the work,
	// and one that is not is refused, which spends the bundle's one attempt and sends it to no
	// pod again (store.TakeWorkspaceBundleForRestore). The pod the work was taken off is told
	// apart by its uid and not by this, so a role relaunched in the pod being released spends
	// nothing.
	podCreated time.Time
	id         string
	podUID     string

	mu      sync.Mutex
	conn    net.Conn
	write   *shimwire.Writer
	state   shimwire.LauncherState
	ready   chan struct{}
	waiters map[string]chan shimwire.Frame
	closed  bool
}

func newLaunchers(copies runtime.SavedWorkspaceCopies, bundle, running runtime.SavedWorkspaceBundle,
	restored runtime.WorkspaceBundleRestored, log *slog.Logger) *launchers {
	return &launchers{sessions: map[claim.Token]*launcherSession{}, copies: copies, bundle: bundle,
		running: running, restored: restored, log: log}
}

// accept registers a launcher connection of the pod of issue, created at podCreated. The newest
// authenticated connection for a role wins: a half-open predecessor is closed so its pending
// requests fail instead of waiting forever.
func (d *launchers) accept(token claim.Token, issue string, podCreated time.Time, hello shimwire.LauncherHello) *launcherSession {
	s := &launcherSession{owner: d, token: token, issue: issue, podCreated: podCreated, id: hello.LauncherID, podUID: hello.PodUID,
		ready: make(chan struct{}), waiters: map[string]chan shimwire.Frame{}}
	d.mu.Lock()
	old := d.sessions[token]
	d.sessions[token] = s
	d.mu.Unlock()
	if old != nil {
		old.close()
	}
	return s
}

func (s *launcherSession) ServeLauncher(conn net.Conn, reader *bufio.Reader, writer *shimwire.Writer) {
	s.mu.Lock()
	s.conn, s.write = conn, writer
	closed := s.closed
	s.mu.Unlock()
	defer s.close()
	if closed {
		return
	}
	frames := shimwire.NewReader(reader)
	for {
		line, err := frames.ReadLine()
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			return
		}
		switch value := frame.(type) {
		case shimwire.LauncherState:
			s.mu.Lock()
			s.state = value
			select {
			case <-s.ready:
			default:
				close(s.ready)
			}
			s.mu.Unlock()
		case shimwire.LauncherStartResult:
			s.mu.Lock()
			if value.OK && (s.state.LastExit == nil || s.state.LastExit.Generation < value.RunningGeneration) &&
				(s.state.Child == nil || s.state.Child.Generation < value.RunningGeneration) {
				s.state.Child = &shimwire.LauncherChild{Generation: value.RunningGeneration}
			}
			s.mu.Unlock()
			s.deliver(value.ID, value)
		case shimwire.LauncherStopResult:
			s.deliver(value.ID, value)
		case shimwire.LauncherBundleResult:
			s.deliver(bundleWaiter(value.ID, value.Index), value)
		case shimwire.LauncherRestored:
			s.owner.restoreReported(s, value)
		default:
			return
		}
	}
}

func (s *launcherSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, waiter := range s.waiters {
		close(waiter)
	}
	s.waiters = nil
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	s.owner.remove(s.token, s)
}

func (s *launcherSession) deliver(id string, frame shimwire.Frame) {
	s.mu.Lock()
	waiter := s.waiters[id]
	delete(s.waiters, id)
	s.mu.Unlock()
	if waiter != nil {
		waiter <- frame
		close(waiter)
	}
}

func (d *launchers) remove(token claim.Token, session *launcherSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions[token] == session {
		delete(d.sessions, token)
	}
}

func (d *launchers) state(token claim.Token, podUID string) (shimwire.LauncherState, bool) {
	d.mu.Lock()
	session := d.sessions[token]
	d.mu.Unlock()
	if session == nil || session.podUID != podUID {
		return shimwire.LauncherState{}, false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return shimwire.LauncherState{}, false
	}
	select {
	case <-session.ready:
	default:
		return shimwire.LauncherState{}, false
	}
	return session.state, true
}

func (d *launchers) await(ctx context.Context, token claim.Token, podUID string) (*launcherSession, error) {
	for {
		d.mu.Lock()
		session := d.sessions[token]
		d.mu.Unlock()
		if session != nil && session.podUID == podUID {
			select {
			case <-session.ready:
				session.mu.Lock()
				closed := session.closed
				session.mu.Unlock()
				if !closed {
					return session, nil
				}
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (d *launchers) request(ctx context.Context, token claim.Token, podUID string, frame shimwire.Frame, id string) (shimwire.Frame, *launcherSession, error) {
	session, err := d.await(ctx, token, podUID)
	if err != nil {
		return nil, nil, fmt.Errorf("launcher %s did not connect: %w", token, err)
	}
	waiter := make(chan shimwire.Frame, 1)
	session.mu.Lock()
	if session.closed || session.write == nil {
		session.mu.Unlock()
		return nil, nil, errors.New("launcher connection closed")
	}
	session.waiters[id] = waiter
	deadline := time.Now().Add(apiTimeout)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	err = session.conn.SetWriteDeadline(deadline)
	if err == nil {
		stopCancel := context.AfterFunc(ctx, func() { _ = session.conn.Close() })
		err = session.write.WriteFrame(frame)
		stopCancel()
		_ = session.conn.SetWriteDeadline(time.Time{})
	}
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		delete(session.waiters, id)
		session.mu.Unlock()
	}()
	if err != nil {
		session.close()
		return nil, nil, err
	}
	select {
	case answer, ok := <-waiter:
		if !ok {
			return nil, nil, errors.New("launcher connection closed before its answer")
		}
		d.mu.Lock()
		current := d.sessions[token]
		sameProcess := current != nil && current.podUID == podUID && current.id == session.id
		d.mu.Unlock()
		if !sameProcess {
			return nil, nil, errors.New("launcher process changed before its answer was confirmed")
		}
		return answer, session, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// startRefusal is a Start the launcher answered OK: false, without starting the generation: the one
// start error after which no child of that generation can be running (stopIssuedChild).
type startRefusal struct {
	token      claim.Token
	generation uint64
	reason     string
}

func (e startRefusal) Error() string {
	return fmt.Sprintf("launcher %s start generation %d: %s", e.token, e.generation, e.reason)
}

func (d *launchers) start(ctx context.Context, token claim.Token, podUID string, command shimwire.LauncherStart) error {
	// The newer of the two copies of this issue's workspace the daemon holds goes over first:
	// the launcher holds it and restores it into the workspace before it runs the role's
	// command, so the restore has to be in its hands before any generation of the pod is
	// started, this one included (internal/launcher's bundleRestore). It never stops the start.
	d.restoreWorkspace(ctx, token, podUID)
	answer, _, err := d.request(ctx, token, podUID, command, command.ID)
	if err != nil {
		return err
	}
	result, ok := answer.(shimwire.LauncherStartResult)
	if !ok {
		return fmt.Errorf("launcher %s start: got %T", token, answer)
	}
	if !result.OK {
		return startRefusal{token: token, generation: command.Generation, reason: result.Error}
	}
	// An OK answer naming another generation says the launcher started something, and not what it
	// was asked: the Start's outcome is unknown, never a refusal, so the issued generation is stopped.
	if result.RunningGeneration != command.Generation {
		return fmt.Errorf("launcher %s start generation %d: answered OK naming generation %d", token, command.Generation, result.RunningGeneration)
	}
	return nil
}

// bundleRestoreBound is how long the hop of one pod's saved bundle may take. The 40 MiB a release
// carries at most is 160 slices of shimwire.LauncherBundlePayloadBytes, each one round trip the
// launcher answers out of memory, so this is room for a slow connection rather than for the
// common case; a hop that outruns it costs the restore and never the start.
const bundleRestoreBound = 2 * time.Minute

// savedWorkspaceCopy is one of the two copies of an issue's workspace the daemon may hold, as
// this hop handles it: the facts the two were ordered by, the take that spends this copy's one
// restore attempt, and the name every line of the restore calls it by.
type savedWorkspaceCopy struct {
	kind  string
	facts runtime.WorkspaceCopyFacts
	take  runtime.SavedWorkspaceBundle
	// refused is the warning a pod that is not newer than this copy is logged with. The two
	// kinds differ in what the refusal leaves behind, which is what an operator reads the line
	// for: a release bundle no pod restored holds the issue's pod until the issue's work is
	// over, and a running copy holds nothing, its work being on the disk it was taken off.
	refused string
}

// The names the lines of this hop call the two copies by, so that an operator reading one knows
// which of the issue's rows it is about.
const (
	bundleCopyName  = "release bundle"
	runningCopyName = "running copy"
)

// heldWorkspaceCopies are the copies the issue holds, newest first, built from one read that
// carried no bundle bytes (store.SavedWorkspaceCopies). The order is the whole of the decision
// this hop makes: the pod is sent the first and every other is closed unsent.
//
// `at` orders them because it is the one thing both carry and the only one that says which work
// descends from which: a running copy is taken while the roles work, a release bundle after they
// stop, and either can be the newer, since a pod can be released minutes after a copy or run for
// hours after a cancelled release left one behind. A tie goes to the release bundle, which is
// taken from a workspace whose roles have stopped: a copy of the same second was taken before
// those roles ended, so it holds no more than the bundle does, and restoring the bundle also
// lifts the hold its row puts on the issue's pod (workflow.IdleHoldBundleUnrestored).
func (d *launchers) heldWorkspaceCopies(bundle, running runtime.WorkspaceCopyFacts) []savedWorkspaceCopy {
	held := make([]savedWorkspaceCopy, 0, 2)
	if bundle.Held {
		held = append(held, savedWorkspaceCopy{kind: bundleCopyName, facts: bundle, take: d.bundle,
			refused: "launcher bundle: this pod was not created after the release's save, so the saved workspace bundle is refused and no pod is sent it again; the work stays in the daemon's database and holds the issue's pod"})
	}
	if running.Held {
		held = append(held, savedWorkspaceCopy{kind: runningCopyName, facts: running, take: d.running,
			refused: "launcher bundle: this pod was not created after the running copy was taken, so the copy is refused and no pod is sent it again; the work stays in the daemon's database and holds no pod, since the disk it was taken off still has it"})
	}
	slices.SortStableFunc(held, func(a, b savedWorkspaceCopy) int { return b.facts.At.Compare(a.facts.At) })
	return held
}

// restoreWorkspace carries the newer of the two copies of this pod's issue workspace to the pod's
// launcher: the bundle the issue's last idle release saved, and the copy the daemon took while
// the pod that held the work was running. The launcher applies what it is sent to the workspace
// before the first role of the pod runs, so every call is from a start that has not been issued
// yet (start), and it reports what the restore did in a frame of its own, which is what the copy
// it put back is dropped on (restoreReported).
//
// Which copy is the newer is decided from facts and never by taking (store.SavedWorkspaceCopies):
// a take spends a copy's one restore attempt whether a byte of it is sent or not, so a restore
// that decided by taking would close the copy it did not send to every later pod.
//
// Both copies' attempts are spent by the pod that is offered the newer. The older is taken too
// and its body discarded, because a copy left with its attempt unspent is a copy some later pod
// can still be sent, and that pod's workspace holds the descendants of the work this pod is about
// to restore: the older copy would then be applied over everything pushed in between, which is
// `jj edit` back to its head and `jj bookmark set --allow-backwards` over the branch. The bytes
// stay in the database for an operator, as the refusal of a copy no pod can be sent does.
//
// Nothing here stops a role. A slice the launcher refuses, a body that did not arrive as it was
// sent, a store that cannot be read and a connection that fails are each logged and left: a pod
// that never starts is worse than one whose workspace holds only what was pushed, and the
// launcher writes the agent its own notice of what was lost (internal/launcher's restoreWorkspace).
//
// One attempt per copy, and the take is it: the store records the pod this send is for before it
// answers the body, and answers nothing for that copy afterwards. So a roles container that
// restarts, a pod that replaces this one and a third pod after an eviction are each sent nothing,
// and no copy is ever applied over work a running pod's roles have done since it was taken. The
// work itself stays in the database; a release bundle no pod restored holds the issue's pod
// (workflow.IdleHoldBundleUnrestored) until the issue's work is over, and a running copy holds
// none, the disk it was taken off being where its work is.
//
// Whose attempt it is the session's own pod decides, and the store decides it: the pod a copy was
// taken off goes on starting roles — a release stops them, and a cadence copies a pod that is
// working — and such a start reaches here too, for the very workspace the work came from. The
// store knows that pod by the uid the copy recorded and answers it nothing, leaving the attempt
// for the pod that replaces it. Every other pod spends the attempt the first time it asks, and a
// pod that is not newer than the copy (launcherSession.podCreated) spends it on a refusal:
// nothing is sent to it, and nothing is sent to the pod after it either, since its roles go on
// working and pushing from the workspace its own fetch provisioned. That refusal is this hop's
// one warning an operator must read — the work is in the database and no pod will put it back on
// its own.
func (d *launchers) restoreWorkspace(ctx context.Context, token claim.Token, podUID string) {
	if d.copies == nil || d.bundle == nil || d.running == nil || d.restored == nil {
		return
	}
	// The session names the issue its pod is of, as the Secret its launcher authenticated with
	// did, and when that pod was created, as the API server stamped it. A launcher that never
	// arrives is the start's own error to report, not this hop's.
	session, err := d.await(ctx, token, podUID)
	if err != nil || session.issue == "" {
		return
	}
	issue := session.issue
	restoring, cancel := context.WithTimeout(ctx, bundleRestoreBound)
	defer cancel()
	bundleFacts, runningFacts, err := d.copies(restoring, issue)
	if err != nil {
		d.log.Warn("launcher bundle: the issue's saved workspace copies could not be read, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "pod", podUID, "error", err)
		return
	}
	// Nothing was released and no copy was taken, or every copy of this issue has been restored
	// and dropped: the pod starts on what its fetch provisioned, having spent nothing.
	held := d.heldWorkspaceCopies(bundleFacts, runningFacts)
	if len(held) == 0 {
		return
	}
	newer := held[0]
	for _, older := range held[1:] {
		d.closeOlderWorkspaceCopy(restoring, token, issue, podUID, session.podCreated, older, newer.kind)
	}
	bundle, take, err := newer.take(restoring, issue, podUID, session.podCreated)
	if err != nil {
		d.log.Warn("launcher bundle: the saved workspace copy could not be taken for this pod's one restore attempt, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "pod", podUID, "copy", newer.kind, "taken", newer.facts.At, "error", err)
		return
	}
	if take == runtime.WorkspaceBundleRefusedOlder {
		d.log.Warn(newer.refused, "issue", issue, "launcher", token, "pod", podUID,
			"created", session.podCreated, "copy", newer.kind, "taken", bundle.At,
			"bytes", newer.facts.Size, "head", bundle.Head, "restore", bundle.Release)
		return
	}
	// This copy has had its one attempt, or this is the pod its work was taken off, whose own
	// workspace is where that work is.
	if take != runtime.WorkspaceBundleSend || len(bundle.Bundle) == 0 {
		return
	}
	frames, err := bundleFrames(issue, bundle)
	if err != nil {
		d.log.Warn("launcher bundle: the saved workspace copy could not be sliced, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "copy", newer.kind, "taken", bundle.At, "error", err)
		return
	}
	for _, frame := range frames {
		answer, _, err := d.request(restoring, token, podUID, frame, bundleWaiter(frame.ID, frame.Index))
		if err != nil {
			d.log.Warn("launcher bundle: a slice of the saved workspace copy did not reach the launcher, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", newer.kind, "slice", frame.Index, "slices", frame.Count, "error", err)
			return
		}
		result, ok := answer.(shimwire.LauncherBundleResult)
		if !ok {
			d.log.Warn("launcher bundle: the launcher answered a bundle slice with another frame, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", newer.kind, "slice", frame.Index, "answer", answer.FrameType())
			return
		}
		if !result.OK {
			d.log.Warn("launcher bundle: the launcher refused a slice of the saved workspace copy, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", newer.kind, "slice", frame.Index, "slices", frame.Count, "error", result.Error)
			return
		}
		if !result.Held {
			continue
		}
		// Held ends the sequence: the launcher has the whole, digest-checked body and applies it
		// before its first role runs. The daemon's copy stays until the launcher reports what
		// the restore itself did (restoreReported) — a restore can still fail, and the work is
		// then in the daemon's database alone, which is where the launcher's notice says it is,
		// and where it stays: this send was that copy's one attempt.
		d.log.Info("launcher bundle: the issue's saved workspace work reached the pod that restores it",
			"issue", issue, "launcher", token, "copy", newer.kind, "taken", bundle.At,
			"bytes", len(bundle.Bundle), "slices", len(frames), "restore", bundle.Release)
		return
	}
	d.log.Warn("launcher bundle: the launcher took every slice of the saved workspace copy without holding the whole",
		"issue", issue, "launcher", token, "copy", newer.kind, "taken", bundle.At, "slices", len(frames))
}

// closeOlderWorkspaceCopy spends the restore attempt of the copy this pod is not being sent and
// discards whatever body the take answered, which is the second half of restoring the newer one:
// the pods after this one hold the descendants of the work it is about to restore, so a copy left
// open for them would be applied over everything pushed in between. The bytes stay in the
// database for an operator to carry out by hand.
//
// A take that answers nothing is the pod the older copy's own work came off asking — its
// workspace is that work — or a copy already closed by an attempt of its own; either way nothing
// was recorded and the row is left as it was. A take that fails leaves the copy open, which is
// the one outcome a later pod can still be hurt by, so it is the warning here; the send of the
// newer copy goes on, since refusing to restore this pod would not close anything either.
func (d *launchers) closeOlderWorkspaceCopy(ctx context.Context, token claim.Token, issue, podUID string,
	podCreated time.Time, older savedWorkspaceCopy, newer string) {
	_, take, err := older.take(ctx, issue, podUID, podCreated)
	switch {
	case err != nil:
		d.log.Warn("launcher bundle: the older of the issue's two saved workspace copies could not be closed, so a later pod may still be sent work older than this pod's restore",
			"issue", issue, "launcher", token, "pod", podUID, "copy", older.kind, "taken", older.facts.At,
			"restoring", newer, "error", err)
	case take == runtime.WorkspaceBundleNone:
		d.log.Info("launcher bundle: the older of the issue's two saved workspace copies had no restore attempt to spend for this pod, so it was left as it was and only the newer is sent",
			"issue", issue, "launcher", token, "pod", podUID, "copy", older.kind, "taken", older.facts.At,
			"bytes", older.facts.Size, "restoring", newer)
	default:
		d.log.Info("launcher bundle: the older of the issue's two saved workspace copies is closed unsent, since this pod restores the newer and the pods after it hold that work's descendants; its bytes stay in the database",
			"issue", issue, "launcher", token, "pod", podUID, "copy", older.kind, "taken", older.facts.At,
			"bytes", older.facts.Size, "restoring", newer)
	}
}

// restoreReported drops the saved copy of this pod's issue workspace that a launcher's report
// names, once that launcher reports that its restore put the work back. It is the one
// confirmation a copy is deleted on: a restore that failed, and a pod no role ever started in,
// leave the work in the daemon's database, where it stays, since that copy's one attempt has been
// recorded and no pod is sent it again (restoreWorkspace). Nothing else deletes a release bundle,
// so the issue's close and its tree's close are what end one no pod restored (outbox's
// dropReleasedWork); a running copy the next cadence replaces.
//
// The restore id the slices carried is the whole of what the delete reads, and all this hop knows
// of the copy: a launcher keeps its report until some connection carries it (internal/launcher's
// bundleRestore.owed), so one can reach the daemon after the issue has saved again, and deleting
// the issue's row on such a report would take work no pod holds any more. The id says both which
// kind of copy it is and which row of that kind, because the two kinds draw their ids from
// disjoint spaces (store.MinRunningWorkspaceCopyRestoreID); which of the two tables it names is
// the daemon's to decide (runtime.WorkspaceBundleRestored), not this package's, and a report
// naming a row either table has replaced deletes nothing.
//
// It runs on the launcher's own frame loop rather than in a goroutine of its own, so a pod whose
// restore is confirmed has its copy gone before any later frame of that launcher is read; the
// delete is one indexed row.
func (d *launchers) restoreReported(s *launcherSession, report shimwire.LauncherRestored) {
	if d.restored == nil || s.issue == "" {
		return
	}
	if !report.OK {
		d.log.Warn("launcher bundle: the pod's launcher could not restore the issue's saved workspace work, which had its one attempt and stays in the daemon's database",
			"issue", s.issue, "launcher", s.token, "sequence", report.ID, "restore", report.Release, "error", report.Error)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	if err := d.restored(ctx, s.issue, report.Release); err != nil {
		d.log.Warn("launcher bundle: the restored workspace copy could not be dropped from the store",
			"issue", s.issue, "launcher", s.token, "restore", report.Release, "error", err)
		return
	}
	d.log.Info("launcher bundle: the pod's launcher restored the issue's saved workspace work, which is dropped from the database",
		"issue", s.issue, "launcher", s.token, "sequence", report.ID, "restore", report.Release)
}

// bundleFrames are the slices that carry one saved copy of issue's workspace. The sequence's id is
// the issue, the copy's own restore id and the digest of the body (shimwire.SplitWorkspaceBundle),
// so it is the same every time the same copy is sent, which is what a pod whose launcher
// reconnects needs: the launcher answers a slice it has already taken under an id it knows with
// the answer it gave, Held included, and refuses only a payload that differs from the one that id
// carried — which another copy of the same issue never is, since every copy of either kind has a
// restore id of its own and so a sequence of its own (internal/launcher's bundleRestore.receive).
func bundleFrames(issue string, b runtime.WorkspaceBundle) ([]shimwire.LauncherBundle, error) {
	return shimwire.SplitWorkspaceBundle("restore-"+issue, b.Release, shimwire.WorkspaceBundle{
		Bundle: b.Bundle, Head: b.Head, Tips: b.Tips, Bookmarks: b.Bookmarks, At: b.At,
	})
}

// bundleWaiter names the request one slice's answer belongs to. A sequence is answered slice by
// slice, so its id alone would name as many answers as it has slices; the launcher keys its own
// results by the same pair.
func bundleWaiter(sequence string, index int) string { return sequence + "/" + strconv.Itoa(index) }

func (d *launchers) stop(ctx context.Context, token claim.Token, podUID string, command shimwire.LauncherStop) error {
	answer, session, err := d.request(ctx, token, podUID, command, command.ID)
	if err != nil {
		return err
	}
	result, ok := answer.(shimwire.LauncherStopResult)
	if !ok {
		return fmt.Errorf("launcher %s stop: got %T", token, answer)
	}
	if !result.OK {
		return fmt.Errorf("launcher %s stop generation %d: %s", token, command.Generation, result.Error)
	}
	// The launcher answers only after the child has exited; its own state frame may follow the
	// answer, so the confirmed stop is recorded now.
	if session != nil {
		session.mu.Lock()
		if child := session.state.Child; child != nil && child.Generation == command.Generation {
			session.state.Child = nil
			session.state.LastExit = &shimwire.LauncherExit{Generation: command.Generation}
		}
		session.mu.Unlock()
	}
	return nil
}

// bindLauncherSecrets binds each launcher role's Secret of the pod l launches to the new pod's uid,
// so only that pod's launchers authenticate with it. A write the API server refuses as a conflict
// — the gh-credential refresher rewrote the Secret between this read and this write
// (refreshGitHubCredential) — is retried once over a fresh read, so the refresher's gh files and the
// binding both land; a launch that failed here instead would leave the new pod running, unbound,
// until the supervisor's next attempt replaced it. Any other failure, or a second conflict, is
// returned.
func (r *Runtime) bindLauncherSecrets(ctx context.Context, s *sandbox, l launch, uid string) error {
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	for _, role := range l.roles {
		name := roleSecretName(s.Name, role)
		read := func() (*corev1.Secret, error) {
			secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("bind launcher secret %s: %w", name, err)
			}
			if !ownedBySandbox(secret.OwnerReferences, s.UID) {
				return nil, fmt.Errorf("bind launcher secret %s: it is not owned by Sandbox %s", name, s.Name)
			}
			return secret, nil
		}
		secret, err := read()
		if err != nil {
			return err
		}
		for retried := false; ; retried = true {
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}
			secret.Annotations[launcherPodUIDAnnotation] = uid
			_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
			if err == nil {
				break
			}
			if !apierrors.IsConflict(err) || retried {
				return fmt.Errorf("bind launcher secret %s to pod UID: %w", name, err)
			}
			if secret, err = read(); err != nil {
				return err
			}
		}
		r.cacheLauncherCredential(s, secret)
	}
	return nil
}

// launcherBound reports whether pod is one whose launchers this runtime bound: the Secret of every
// role of l's pod, its launcher roles, exists, is the Sandbox's, binds pod by uid, and records its
// gh bundle's default owner (GitHubDefaultKey, empty for a controller whose project has no GitHub
// owner), which every role of one pod shares and l takes as its gitHubDefault, every agent's
// GH_CONFIG_DIR. A running pod an older runtime made (one worker container, no launchers, or one
// gh directory where a bundle now stands) or one whose binding never landed is replaced rather than
// trusted. Any other failure to read a Secret, such as the client's timeout, or a throttled request
// or a server error the launch's deadline for it outlasted (resend), proves nothing about the pod:
// it is returned, so only the launch that read it fails, and the caller stops that role's previous
// process (runtime.Runtime's Spawn and Resume); the pod and every other role running in it are left
// as they are. Each Secret it finds bound to pod, holding a launcher token, is held as its role's
// credential (cacheLauncherCredential), so the launcher a kept pod's launch waits for is not
// refused for a read of its own that failed earlier (credentialRetry).
func (r *Runtime) launcherBound(ctx context.Context, s *sandbox, pod *corev1.Pod, l *launch) (bool, error) {
	for _, role := range l.roles {
		name := roleSecretName(s.Name, role)
		secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("read launcher secret %s: %w", name, err)
		case !ownedBySandbox(secret.OwnerReferences, s.UID) || secret.Annotations[launcherPodUIDAnnotation] != string(pod.UID):
			return false, nil
		}
		recorded, ok := secret.Data[GitHubDefaultKey]
		if !ok {
			return false, nil
		}
		l.gitHubDefault = string(recorded)
		if len(secret.Data[LauncherTokenFile]) > 0 {
			r.cacheLauncherCredential(s, secret)
		}
	}
	return true, nil
}

// launcherCommand is the start command a role's launcher receives for one generation: the shim's
// argv (its flags, then `--`, then the agent's own argv), the generation's plain environment, the
// launch credentials it writes into its private directory, and the session a resume continues.
// The shim is told the pod's stop grace, the same TerminationGrace its launcher runs with, so the
// wait it makes for an in-flight CodeGraph warm-up on its way out stays inside the grace after
// which the launcher kills it. A tree role's shim is told --warm-codegraph: once its Oh My Pi has
// started it builds the CodeGraph index of the workspace LEGION_WORKSPACE names, which
// launchEnvironment carries for every role but the controller (issuePod.agentEnv, podkind.go),
// whose pod has no workspace. When the launch installs a stale-plugin relaunch's pair (l.spec.Plugins
// set, LEGION-164) or the deployment names a setup command (runtime.kubernetes.setup, r.setup), the
// agent's own argv is itself wrapped in the start script (startArgv): the shim's child is then
// `/bin/sh -c '<script>' legion-start <n> <setup…> <agent argv…>`, which installs the pair, runs
// the setup command, and execs the agent, so the shim's own preparation (--pod-safety) has
// already run and no new shim flag is needed; after the script's `exec` the child is the harness
// itself, so the shim's pid is unchanged. The setup command runs at every start of the role,
// a Spawn's, a Resume's and a replaced pod's alike. A Claude Code launch is wrapped whatever else
// it carries, for the `mkdir -p` of its configuration directory (claudeConfigDir) and the install
// of the pinned Claude Code its agent command names (claudeCodeBlock: the image carries the pin
// and never the binary), and adds one file to the generation's credentials: the MCP document its
// agent command reads
// (claudeMCPFile), which the launcher writes 0600 into the 0700 private directory it makes for
// the generation and removes with the rest when the generation ends (internal/launcher). It
// travels in Files and never in spec.Secrets, which would be two wrongs: every secret name also
// gets a `<NAME>_FILE` pointer in the worker's environment (mainEnvironment), and
// ValidateSpawnSpec refuses a credential-shaped name outright.
//
// The agent-secrets flags are the one part of the command built from the pod the role will run in
// rather than from the configuration as it now reads (launch.enrolls): the shim stats the key
// directory and the token file those flags name and exits 1 when either is absent
// (shim.ReadAgentSecretsFlags), and a live pod a launch keeps may have been built before the
// runtime enrolled, so it carries neither. Such a role is started without them and runs without
// the broker until the update round replaces its pod (INFRA-484, keptPod) — which it can only do
// once every worker of the pod has pushed its work and answered, and a process that refused to
// start would never answer at all. The environment that start carries says the same thing: the
// AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR an agent reads to decide it holds a broker are set
// from this one decision too (mainEnvironment, enrollsIn), so such a role reports a resource it
// cannot reach rather than a secrets problem it does not have.
func launcherCommand(l launch, r *Runtime) shimwire.LauncherStart {
	dir := generationDir(l.spec.Generation)
	shim := []string{r.tools.Legion, "worker-shim", connectFlag, r.streamURL, "--boot-token-file", dir + "/" + bootTokenKey, "--pod-safety", "--stop-grace", r.terminationGrace.String()}
	if l.spec.Role != claim.RoleController {
		shim = append(shim, "--warm-codegraph")
	}
	// The shim is told the providers directory only where the pod mounts the providers Secret there,
	// which it does when something needs it (mountsProviders: a provider key, a providers secret, or
	// a session database URL key); a directory it is told and cannot read refuses its start
	// (shim.ReadProviderEnv). Under cloudsql-iam no URL key is held — each pod signs in to Cloud SQL
	// as itself — so a deployment with no provider key and no providers secret has no such directory.
	if r.mountsProviders() {
		shim = append(shim, "--provider-env-dir", ProvidersDir)
	}
	if l.enrolls(r) {
		shim = append(shim, "--agent-secrets-key-dir", AgentSecretsKeyDir, "--pod-token-file", AgentSecretsTokenDir+"/"+AgentSecretsTokenFile, "--agent-secrets-bin", r.tools.AgentSecrets)
	}
	_, values := r.launchEnvironment(l)
	values = append(values, bootTokenKey+"_FILE="+dir+"/"+bootTokenKey)
	files := maps.Clone(l.secrets)
	if files == nil {
		files = map[string]string{}
	}
	files[bootTokenKey] = l.spec.BootToken
	claudeDir := ""
	if l.runsClaude() {
		claudeDir = claudeConfigDir(l.spec.Role)
		files[claudeMCPFile] = l.claude.mcp
	}
	agentArgv := startArgv(roleStateHome(l.spec.Role), l.spec.Generation, l.spec.Plugins, claudeDir, l.skillsInstall(r), r.setup,
		l.agentArgv(r.agent), installLockWait(r.bootTimeout, r.registrationIntervals))
	return shimwire.LauncherStart{
		ID: "start-" + fmt.Sprint(l.spec.Generation), Generation: l.spec.Generation,
		Argv: append(shim, append([]string{"--"}, agentArgv...)...), Env: values, Files: files,
		ResumeFile: l.spec.ResumeSessionFile,
	}
}
