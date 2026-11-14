package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
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

// LeakCheckReader reads the shared public leak check at a tag, packed for a pod's launcher:
// *leakguard.Source, whose every error is one cause sentence (leakguard.Cause).
type LeakCheckReader interface {
	Get(ctx context.Context, tag string) (leakguard.Served, error)
}

type launchers struct {
	mu       sync.Mutex
	sessions map[claim.Token]*launcherSession

	// leakCheck reads the shared public leak check a launcher's hello names (Options.LeakCheck);
	// nil is a daemon that runs with no GitHub App to read it with. leakCheckHop bounds one push
	// of it to a launcher, leakCheckHopBound outside the tests.
	leakCheck    LeakCheckReader
	leakCheckHop time.Duration
	// saved is the take of whatever an issue holds of its workspace for a new pod, which chooses
	// between the issue's two copies and spends their attempts in one transaction, and restored
	// the confirmation that keeps the copy a launcher put back as the running copy of that
	// launcher's pod: the daemon's store behind both (Options.SavedWorkspaceBundle,
	// WorkspaceBundleRestored). A launchers built without the pair restores nothing.
	saved    runtime.SavedWorkspaceBundle
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
	// pod again (store.TakeWorkspaceCopyForRestore). The pod the work was taken off is told
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
	// leakCheckTag is the tag of the shared public leak check this launcher's hello named, its
	// own pin ("" for a launcher that takes no check from the daemon), and leakCheckInstalled
	// whether this launcher process holds that tag installed: as its hello said, or as its answer
	// to the push on this connection did (serveLeakCheck).
	leakCheckTag       string
	leakCheckInstalled bool
}

func newLaunchers(leakCheck LeakCheckReader, saved runtime.SavedWorkspaceBundle, restored runtime.WorkspaceBundleRestored, log *slog.Logger) *launchers {
	return &launchers{sessions: map[claim.Token]*launcherSession{}, leakCheck: leakCheck, leakCheckHop: leakCheckHopBound,
		saved: saved, restored: restored, log: log}
}

// accept registers a launcher connection of the pod of issue, created at podCreated. The newest
// authenticated connection for a role wins: a half-open predecessor is closed so its pending
// requests fail instead of waiting forever.
func (d *launchers) accept(token claim.Token, issue string, podCreated time.Time, hello shimwire.LauncherHello) *launcherSession {
	s := &launcherSession{owner: d, token: token, issue: issue, podCreated: podCreated, id: hello.LauncherID, podUID: hello.PodUID,
		ready: make(chan struct{}), waiters: map[string]chan shimwire.Frame{},
		leakCheckTag: hello.LeakCheckTag, leakCheckInstalled: hello.LeakCheckInstalled}
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
		case shimwire.LauncherLeakCheckResult:
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
// start error after which no child of that generation can be running (stopIssuedChild). A refusal
// over the session it was asked to resume carries the launcher's own verdict as its kind
// (shimwire.SessionRefusal); this is the one place that kind becomes the sentinel the supervisor
// acts on, so a lost session and a store that could not be asked reach it as they do from the tmux
// runtime (runtime.ErrSessionFileMissing, runtime.ErrSessionStore).
type startRefusal struct {
	token      claim.Token
	generation uint64
	reason     string
	session    shimwire.SessionRefusal
}

func (e startRefusal) Error() string {
	return fmt.Sprintf("launcher %s start generation %d: %s", e.token, e.generation, e.reason)
}

func (e startRefusal) Unwrap() error {
	switch e.session {
	case shimwire.SessionGone:
		return runtime.ErrSessionFileMissing
	case shimwire.SessionStoreUnreachable:
		return runtime.ErrSessionStore
	}
	return nil
}

func (d *launchers) start(ctx context.Context, token claim.Token, podUID string, command shimwire.LauncherStart) error {
	// The shared public leak check goes over before anything else, and a launcher that could not
	// install it starts nothing: every write of the role resolves the check first and its shim
	// refuses to start the agent without the install (leakguard.Installed), so a start sent
	// anyway would be a role that fails at once. The refusal leaves the issue's saved copies of
	// its workspace in the store, since a copy taken here would spend its one restore attempt on a
	// pod no role of which ran.
	if err := d.serveLeakCheck(ctx, token, podUID); err != nil {
		return err
	}
	// The newer of the two copies of this issue's workspace the daemon holds goes over next: the
	// launcher holds it and restores it into the workspace before it runs the role's command, so
	// the restore has to be in its hands before any generation of the pod is started, this one
	// included (internal/launcher's bundleRestore). It never stops the start.
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
		return startRefusal{token: token, generation: command.Generation, reason: result.Error, session: result.Session}
	}
	// An OK answer naming another generation says the launcher started something, and not what it
	// was asked: the Start's outcome is unknown, never a refusal, so the issued generation is stopped.
	if result.RunningGeneration != command.Generation {
		return fmt.Errorf("launcher %s start generation %d: answered OK naming generation %d", token, command.Generation, result.RunningGeneration)
	}
	return nil
}

// leakCheckHopBound is how long the push of the shared public leak check to one launcher may take:
// its slices and the launcher's answer to the last, which it gives once it has unpacked, verified
// and installed the check. The daemon already holds the check when the hop begins, so the hop is
// a few slices of shimwire.LauncherBundlePayloadBytes, the launcher's unpack, its one `--help`
// run and its rename, and this is a slow connection's room. It runs under the start's own
// context, as the bundle's hop does, so the launch's own bound needs no term for it. A hop that
// outruns it refuses the start; the launcher's install goes on under its own bound, and the next
// start is answered Installed at once.
const leakCheckHopBound = 2 * time.Minute

// serveLeakCheck pushes the shared public leak check to token's launcher in podUID and waits for
// the launcher to install it, before any start frame or bundle slice reaches that launcher
// (start). The tag is the launcher's own pin, which its hello named; the daemon reads the check
// at that tag (Options.LeakCheck) and the launcher installs it once for its pod, for every role's
// shim to read.
//
// Nothing is sent to a launcher whose hello named no tag: it takes no check from the daemon,
// and its roles start as they always have. Nothing is sent either to one that holds the tag
// installed, as its hello said or as its answer on this connection did, so a later start of the
// role and a daemon that restarts push nothing to a running pod.
//
// Every other outcome but an install is a runtime.LeakCheckRefusal, whose cause sentence
// (leakguard.Cause) says why: the daemon holds no credential to read the check, its read was
// refused, the launcher refused a slice or the install, the connection failed, or the hop ran
// past leakCheckHopBound. No start frame has been sent when it refuses.
func (d *launchers) serveLeakCheck(ctx context.Context, token claim.Token, podUID string) error {
	// A launcher that never arrives is the start's own error to report, not this hop's.
	session, err := d.await(ctx, token, podUID)
	if err != nil {
		return nil
	}
	tag := session.leakCheckTag
	session.mu.Lock()
	installed := session.leakCheckInstalled
	session.mu.Unlock()
	if tag == "" || installed {
		return nil
	}
	refuse := func(what string, err error) error {
		return runtime.LeakCheckRefusal{Cause: leakguard.Cause(what, err)}
	}
	if d.leakCheck == nil {
		return refuse("the daemon runs with no GitHub App, so it cannot read "+leakguard.CheckSlug, nil)
	}
	served, err := d.leakCheck.Get(ctx, tag)
	if err != nil {
		return refuse(err.Error(), nil)
	}
	hop, cancel := context.WithTimeout(ctx, d.leakCheckHop)
	defer cancel()
	for _, frame := range served.Frames {
		answer, confirmed, err := d.request(hop, token, podUID, frame, bundleWaiter(frame.ID, frame.Index))
		if err != nil {
			if hop.Err() != nil && ctx.Err() == nil {
				return refuse(fmt.Sprintf("the pod's launcher did not answer the install of %s within %s", tag, d.leakCheckHop), nil)
			}
			return refuse(fmt.Sprintf("the pod's launcher did not answer the install of %s", tag), err)
		}
		result, ok := answer.(shimwire.LauncherLeakCheckResult)
		if !ok {
			return refuse(fmt.Sprintf("the pod's launcher answered slice %d of %s with a %s frame", frame.Index, tag, answer.FrameType()), nil)
		}
		if err := result.Validate(); err != nil {
			return refuse(fmt.Sprintf("the pod's launcher answered slice %d of %s with a malformed result", frame.Index, tag), err)
		}
		if !result.OK {
			return refuse(result.Error, nil)
		}
		if !result.Installed {
			continue
		}
		// Installed ends the sequence: the launcher holds the whole, digest-checked check at its
		// pin, installed for every role of its pod, so this connection is sent it no more.
		confirmed.mu.Lock()
		confirmed.leakCheckInstalled = true
		confirmed.mu.Unlock()
		d.log.Info("launcher leak check: the pod's launcher installed the shared public leak check the daemon read",
			"launcher", token, "pod", podUID, "tag", tag, "bytes", len(served.Body), "slices", len(served.Frames))
		return nil
	}
	return refuse(fmt.Sprintf("the pod's launcher took every slice of %s without installing it", tag), nil)
}

// bundleRestoreBound is how long the hop of one pod's saved bundle may take. The 40 MiB a release
// carries at most is 160 slices of shimwire.LauncherBundlePayloadBytes, each one round trip the
// launcher answers out of memory, so this is room for a slow connection rather than for the
// common case; a hop that outruns it costs the restore and never the start.
const bundleRestoreBound = 2 * time.Minute

// refusedOlderLine is the warning a pod that is not newer than a copy is logged with, by the
// copy's kind. The two kinds differ in what the refusal leaves behind, which is what an operator
// reads the line for: a release bundle no pod restored holds the issue's pod until the issue's
// work is over, and a running copy holds nothing, its work being on the disk it was taken off.
func refusedOlderLine(kind runtime.WorkspaceCopyKind) string {
	if kind == runtime.WorkspaceCopyReleaseBundle {
		return "launcher bundle: this pod was not created after the release's save, so the saved workspace bundle is refused and no pod is sent it again; the work stays in the daemon's database and holds the issue's pod"
	}
	return "launcher bundle: this pod was not created after the running copy was taken, so the copy is refused and no pod is sent it again; the work stays in the daemon's database and holds no pod, since the disk it was taken off still has it"
}

// refusedPushedLine is the warning a running copy older than the issue's newest recorded push is
// logged with (runtime.WorkspaceBundleRefusedPushed); a release bundle is never refused so. It is
// the line an operator reads to know why a pod started on what was pushed although the daemon
// held a copy, and what that copy would have done.
const refusedPushedLine = "launcher bundle: the saved workspace copy was taken before the newest push of the issue's branch the daemon recorded, so it is refused and no pod is sent it: restoring it would put the workspace back on work behind the pushed commits, which the roles would have to bring forward over them; its bytes stay in the daemon's database"

// supersededLine is the line the older of the issue's two copies is closed unsent with
// (runtime.WorkspaceBundleSuperseded). It names the newer copy, the one the decision offered
// (runtime.WorkspaceRestore.Offered), and what this pod's ask did with it, since that copy is not
// always sent here: its attempt can be spent already, this pod can be the one its work came off,
// and this pod can be older than it.
const supersededLine = "launcher bundle: the older of the issue's two saved workspace copies is closed unsent, since the newer one is the copy the issue's pods are offered and no pod may restore the older over its work; its bytes stay in the database"

// withheldBranchLine is the one line a copy that holds the issue's branch `legion/<KEY>`, its
// rescue branch `legion/<KEY>-rescue` or both is sent with. The restore sets every bookmark the
// copy holds but those two (bundleFrames, runtime.RestorableBookmarks), so each stays where the
// pod's own fetch put it, on what GitHub holds: the issue's branch so that the role's next push
// keeps the commits GitHub holds on it, and the rescue branch because it only moves forward, so a
// bookmark set back behind GitHub's tip would be pushed as a move backwards that drops a later
// rescue, or be left conflicted by the next rescue push, which jj then refuses to push. The line
// names each bookmark withheld with the commit the copy held it on, as the very call that left
// them out of the slices answered them.
const withheldBranchLine = "launcher bundle: the saved workspace copy's own values of the issue's branch and its rescue branch are withheld where it holds them, so the restore leaves each where the pod's fetch from GitHub put it; the copy's commits come back as heads and as the working copy"

// reachedLine is the line a copy the launcher holds whole is logged with, before the launcher
// applies it.
const reachedLine = "launcher bundle: the issue's saved workspace work reached the pod that restores it"

// restoreFailedLine is the warning a launcher's report of a restore that failed is logged with.
// The copy had its one attempt and stays in the daemon's database, but a running copy only until
// the next copy of the issue's workspace replaces it, which the line says so that an operator who
// wants its bytes reads them first (restoreReported).
const restoreFailedLine = "launcher bundle: the pod's launcher could not restore the issue's saved workspace work, which had its one attempt and is sent to no pod again; it stays in the daemon's database, a running copy only until the next copy of the issue's workspace replaces it"

// restoreWorkspace carries the newer of the two copies of this pod's issue workspace to the pod's
// launcher: the bundle the issue's last idle release saved, and the copy the daemon took while
// the pod that held the work was running. The launcher applies what it is sent to the workspace
// before the first role of the pod runs, so every call is from a start that has not been issued
// yet (start), and it reports what the restore did in a frame of its own, which is what the copy
// it put back is kept as this pod's own on (restoreReported).
//
// Which copy, if either, the pod is sent is the store's one decision, made in the transaction
// that spends the attempts (store.TakeWorkspaceCopyForRestore, runtime.DecideWorkspaceRestore):
// the newer copy is offered and the older closed unsent, and a running copy older than the newest
// push of the issue's branch the daemon recorded is refused, where a release bundle never is.
// This hop only carries out what that answer says and logs each copy it touched, so no state of
// the store depends on this hop finishing. Whatever copy it sends goes without its own values of
// the issue's branch and its rescue branch (bundleFrames), so no restore moves `legion/<KEY>` or
// `legion/<KEY>-rescue` off what GitHub holds.
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
// working and pushing from the workspace its own fetch provisioned. That refusal, and the refusal
// of a running copy older than a recorded push, are this hop's warnings an operator must read —
// the work is in the database and no pod will put it back on its own.
func (d *launchers) restoreWorkspace(ctx context.Context, token claim.Token, podUID string) {
	if d.saved == nil || d.restored == nil {
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
	restore, err := d.saved(restoring, issue, podUID, session.podCreated)
	if err != nil {
		d.log.Warn("launcher bundle: the issue's saved workspace copies could not be taken for this pod's one restore attempt, so the pod starts on what was pushed and every copy is left as it was",
			"issue", issue, "launcher", token, "pod", podUID, "error", err)
		return
	}
	// Each copy the take recorded an attempt on is logged, and the offered one, when its take is
	// a send, is the only one this hop goes on with: the decision sends no other. A copy the ask
	// recorded nothing on — the issue holds none, its attempt is spent, or its work came off this
	// very pod — is a take of WorkspaceBundleNone and logs nothing, the offered one included.
	offered := restore.Of(restore.Offered)
	for _, kind := range [...]runtime.WorkspaceCopyKind{runtime.WorkspaceCopyReleaseBundle, runtime.WorkspaceCopyRunning} {
		taken := restore.Of(kind)
		b := taken.Bundle
		switch taken.Take {
		case runtime.WorkspaceBundleRefusedOlder:
			d.log.Warn(refusedOlderLine(kind), "issue", issue, "launcher", token, "pod", podUID,
				"created", session.podCreated, "copy", kind, "taken", b.At,
				"bytes", taken.Size, "head", b.Head, "restore", b.Release)
		case runtime.WorkspaceBundleRefusedPushed:
			d.log.Warn(refusedPushedLine, "issue", issue, "launcher", token, "pod", podUID,
				"copy", kind, "taken", b.At, "bytes", taken.Size, "head", b.Head, "restore", b.Release,
				"pushed", restore.Push.At, "pushedHead", restore.Push.Head)
		case runtime.WorkspaceBundleSuperseded:
			d.log.Info(supersededLine,
				"issue", issue, "launcher", token, "pod", podUID, "copy", kind, "taken", b.At,
				"bytes", taken.Size, "newer", restore.Offered, "newerTake", offered.Take)
		}
	}
	if offered.Take != runtime.WorkspaceBundleSend || len(offered.Bundle.Bundle) == 0 {
		return
	}
	kind, bundle := restore.Offered, offered.Bundle
	frames, withheld, err := bundleFrames(issue, bundle)
	if err != nil {
		d.log.Warn("launcher bundle: the saved workspace copy could not be sliced, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "copy", kind, "taken", bundle.At, "error", err)
		return
	}
	if len(withheld) > 0 {
		d.log.Info(withheldBranchLine, "issue", issue, "launcher", token, "copy", kind,
			"withheld", withheld, "restore", bundle.Release)
	}
	for _, frame := range frames {
		answer, _, err := d.request(restoring, token, podUID, frame, bundleWaiter(frame.ID, frame.Index))
		if err != nil {
			d.log.Warn("launcher bundle: a slice of the saved workspace copy did not reach the launcher, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", kind, "slice", frame.Index, "slices", frame.Count, "error", err)
			return
		}
		result, ok := answer.(shimwire.LauncherBundleResult)
		if !ok {
			d.log.Warn("launcher bundle: the launcher answered a bundle slice with another frame, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", kind, "slice", frame.Index, "answer", answer.FrameType())
			return
		}
		if !result.OK {
			d.log.Warn("launcher bundle: the launcher refused a slice of the saved workspace copy, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", kind, "slice", frame.Index, "slices", frame.Count, "error", result.Error)
			return
		}
		if !result.Held {
			continue
		}
		// Held ends the sequence: the launcher has the whole, digest-checked body and applies it
		// before its first role runs. The daemon's copy stays as it is until the launcher reports
		// what the restore itself did (restoreReported) — a restore can still fail, and the work
		// is then in the daemon's database alone, which is where the launcher's notice says it is,
		// and where it stays: this send was that copy's one attempt.
		d.log.Info(reachedLine,
			"issue", issue, "launcher", token, "copy", kind, "taken", bundle.At,
			"bytes", len(bundle.Bundle), "slices", len(frames), "restore", bundle.Release)
		return
	}
	d.log.Warn("launcher bundle: the launcher took every slice of the saved workspace copy without holding the whole",
		"issue", issue, "launcher", token, "copy", kind, "taken", bundle.At, "slices", len(frames))
}

// restoreReported keeps the saved copy of this pod's issue workspace that a launcher's report
// names as this pod's own running copy, once that launcher reports that its restore put the work
// back. A restore that failed, and a pod no role ever started in, leave the copy as the send left
// it, with its one attempt recorded, so no pod is sent it again (restoreWorkspace). A release
// bundle no pod restored is ended by the issue's close or its tree's close (outbox's
// dropReleasedWork), or by a cancel of the release that saved it, which moves it into the issue's
// running copy (store.CancelIssueRelease). A running copy no pod restored is replaced by the next
// copy taken off any pod of the issue, the restoring pod's own included
// (store.PutRunningWorkspaceCopy).
//
// The confirmed copy is kept, not deleted, because from here on the restoring pod's disk is the
// one place its work is, and a pod that does nothing takes no copy of its own: an agent relaunched
// in it comes up Ready rather than ending a turn, the cadence copies only an issue in a turn, and
// the push copy needs a push. The store sources the kept copy to this session's pod, so that pod's
// own later asks are answered nothing and the pod after it is offered the work, and a second end
// from outside before any turn of this pod still restores it (store.KeepRestoredWorkspaceCopy).
// The session's pod is the pod the report is of: a launcher process lives in its pod, and one that
// restarts there holds no report.
//
// The restore id the slices carried is the whole of what the keep reads of the copy, and all this
// hop knows of it: a launcher keeps its report until some connection carries it (internal/launcher's
// bundleRestore.owed), so one can reach the daemon after the issue has saved again, and rebinding
// the issue's row on such a report would take newer work for this pod's. The id says both which
// kind of copy it is and which row of that kind, because the two kinds draw their ids from disjoint
// spaces (store.MinRunningWorkspaceCopyRestoreID); which of the two tables it names is read by the
// store that owns both (store.KeepRestoredWorkspaceCopy, which the daemon hands this package as
// runtime.WorkspaceBundleRestored), not here, and a report naming a row either table has replaced
// changes nothing.
//
// It runs on the launcher's own frame loop rather than in a goroutine of its own, so a pod whose
// restore is confirmed has its copy kept before any later frame of that launcher is read; the
// keep is one indexed row of each table.
func (d *launchers) restoreReported(s *launcherSession, report shimwire.LauncherRestored) {
	if d.restored == nil || s.issue == "" {
		return
	}
	if !report.OK {
		d.log.Warn(restoreFailedLine,
			"issue", s.issue, "launcher", s.token, "sequence", report.ID, "restore", report.Release, "error", report.Error)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	if err := d.restored(ctx, s.issue, s.podUID, report.Release); err != nil {
		d.log.Warn("launcher bundle: the restored workspace copy could not be kept as the restoring pod's own copy in the store",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "restore", report.Release, "error", err)
		return
	}
	d.log.Info("launcher bundle: the pod's launcher restored the issue's saved workspace work, which the database keeps as that pod's own copy",
		"issue", s.issue, "launcher", s.token, "pod", s.podUID, "sequence", report.ID, "restore", report.Release)
}

// bundleFrames are the slices that carry one saved copy of issue's workspace, with every bookmark
// of it but the issue's own branch and its rescue branch (runtime.RestorableBookmarks), and what
// that left out: withheld, each of the two the copy held with the commit it held it on, nil when
// it held neither, which is what the restore's withheldBranchLine names. Every send of either kind
// of copy passes here, so no restore sets `legion/<KEY>` or `legion/<KEY>-rescue`, wherever the
// copy held them. The sequence's id is the issue, the copy's own restore id and the digest of the
// body (shimwire.SplitWorkspaceBundle), so it is the same every time the same copy is sent, which
// is what a pod whose launcher reconnects needs: the launcher answers a slice it has already taken
// under an id it knows with the answer it gave, Held included, and refuses only a payload that
// differs from the one that id carried — which another copy of the same issue never is, since
// every copy of either kind has a restore id of its own and so a sequence of its own
// (internal/launcher's bundleRestore.receive).
func bundleFrames(issue string, b runtime.WorkspaceBundle) (frames []shimwire.LauncherBundle, withheld map[string]string, err error) {
	bookmarks, withheld := runtime.RestorableBookmarks(issue, b.Bookmarks)
	frames, err = shimwire.SplitWorkspaceBundle("restore-"+issue, b.Release, shimwire.WorkspaceBundle{
		Bundle: b.Bundle, Head: b.Head, Tips: b.Tips, Bookmarks: bookmarks, At: b.At,
	})
	return frames, withheld, err
}

// bundleWaiter names the request one slice's answer belongs to, of a launcher_bundle sequence or
// a launcher_leak_check one. A sequence is answered slice by slice, so its id alone would name as
// many answers as it has slices; the launcher keys its own results by the same pair.
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
// the broker until the update round replaces its pod (INFRA-484, keptPod) — once every worker of
// the pod has pushed its work and answered, or at the round's bound — and a process that refused
// to start would push nothing and answer nothing. The environment that start carries says the
// same thing: the AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR an agent reads to decide it holds a
// broker are set from this one decision too (mainEnvironment, enrollsIn), so such a role reports
// a resource it cannot reach rather than a secrets problem it does not have.
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
