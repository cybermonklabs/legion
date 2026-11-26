package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
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

	// leakCheckSetting is the deployment's shared public leak check (Options.LeakCheckSetting);
	// the zero Setting is a deployment that runs none, whose launchers are pushed nothing.
	// leakCheck reads the check a launcher's hello names (Options.LeakCheck); nil under a
	// configured setting is a daemon that runs with no GitHub App to read it with. leakCheckHop
	// bounds one push of it to a launcher, leakCheckHopBound outside the tests.
	leakCheckSetting leakguard.Setting
	leakCheck        LeakCheckReader
	leakCheckHop     time.Duration
	// beforeStart is set by tests alone: start runs it between the session serveLeakCheck chose
	// and the Start's write, the window in which a launcher that reconnects leaves the start on a
	// closed session (TestALaunchWhoseLauncherReconnectsBeforeItsStartIsWrittenFailsAndTheNextTryStarts).
	beforeStart func(ctx context.Context, token claim.Token)
	// keptStart is a role's start in a pod of a Sandbox that owns the issue's kept volume, which
	// records the pod with that volume (Options.KeptPodStart). A launchers built without it records
	// no pod.
	keptStart runtime.KeptPodStart
	// guardKilled takes a launcher's report of one process ended in an issue pod into the daemon's
	// fact path (Options.MemoryGuardKilled), and controllerKilled one ended in the controller's pod
	// (Options.ControllerMemoryGuardKilled): in a pod of either kind, one the memory guard ended in
	// gVisor, or one the pod's own kernel stopped in a microVM, with the report's Cause
	// (shimwire.MemoryKillCauseKernel, or shimwire.MemoryKillCauseKernelProtected for a process
	// that runs a role's session or the controller's). A launchers built without the one a report
	// needs reads the frame, logs that no receiver took it, and drops it (memoryKilled).
	guardKilled      runtime.MemoryGuardKilled
	controllerKilled runtime.ControllerMemoryGuardKilled
	// volume takes a cache trimmer's sweep report into the daemon's fact path
	// (Options.VolumeReported). A launchers built without it reads the frame, logs that no
	// receiver took it, and drops it (volumeReported).
	volume runtime.VolumeReported
	log    *slog.Logger
}

type launcherSession struct {
	owner *launchers
	token claim.Token
	// issue is the exact issue key this launcher's pod is of, as the role Secret's annotation
	// carried it to the hello's authentication (launcherCredential.issue). "" for a pod of no
	// issue, the project controller's.
	issue string
	// keptSince is when the API server created this launcher's pod's Sandbox, where that Sandbox
	// owns the issue's kept volume (ownsKeptVolume), and the zero time for a pod on a volume of its
	// own. Each role's start in such a pod records it with that volume (recordKeptPod).
	keptSince time.Time
	id        string
	podUID    string
	// resumesBySession is whether this launcher's hello said it can look a session up by its id
	// and resume the file it finds (shimwire.LauncherHello.ResumesBySession). A launcher built
	// before it could says nothing, and relaunch names it no session by its id alone.
	resumesBySession bool
	// readsWorkspace is whether this launcher's hello said it answers the workspace read on its
	// own connection (shimwire.LauncherHello.ReadsWorkspace). A launcher built before it closes
	// its connection on that frame, so the daemon asks only one that said so (workspaceReader).
	readsWorkspace bool

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

func newLaunchers(setting leakguard.Setting, leakCheck LeakCheckReader, keptStart runtime.KeptPodStart, guardKilled runtime.MemoryGuardKilled,
	controllerKilled runtime.ControllerMemoryGuardKilled, volume runtime.VolumeReported, log *slog.Logger) *launchers {
	return &launchers{sessions: map[claim.Token]*launcherSession{}, leakCheckSetting: setting, leakCheck: leakCheck, leakCheckHop: leakCheckHopBound,
		keptStart: keptStart, guardKilled: guardKilled, controllerKilled: controllerKilled, volume: volume, log: log}
}

// accept registers a launcher connection of the pod of issue, whose Sandbox owns the issue's kept
// volume since keptSince (the zero time for a pod on a volume of its own). The newest
// authenticated connection for a role wins: a half-open predecessor is closed so its pending
// requests fail instead of waiting forever.
func (d *launchers) accept(token claim.Token, issue string, keptSince time.Time, hello shimwire.LauncherHello) *launcherSession {
	s := &launcherSession{owner: d, token: token, issue: issue, keptSince: keptSince, id: hello.LauncherID, podUID: hello.PodUID,
		ready: make(chan struct{}), waiters: map[string]chan shimwire.Frame{},
		leakCheckTag: hello.LeakCheckTag, leakCheckInstalled: hello.LeakCheckInstalled, resumesBySession: hello.ResumesBySession,
		readsWorkspace: hello.ReadsWorkspace}
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
		case shimwire.WorkspaceWorkResult:
			s.deliver(value.ID, value)
		case shimwire.LauncherLeakCheckResult:
			s.deliver(sliceWaiter(value.ID, value.Index), value)
		case shimwire.LauncherRestored:
			s.owner.earlierRestoreReport(s, value)
		case shimwire.LauncherMemoryKill:
			s.owner.memoryKilled(s, value)
		case shimwire.LauncherVolumeReport:
			s.owner.volumeReported(s, value)
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

// resumesBySession is whether the launcher of token connected from podUID, the one state reads,
// said in its hello that it can look a session up by its id and resume the file it finds
// (launcherSession.resumesBySession). It is false where no launcher of that pod is connected, as
// for one built before it could: relaunch names a session by its id alone only to a launcher
// known to make the lookup. Any other would ignore the id and start Oh My Pi on the bare
// `--resume=<id>`, a launch the supervisor did not build as one (runtime.SpawnSpec.ResumeByAgent).
func (d *launchers) resumesBySession(token claim.Token, podUID string) bool {
	d.mu.Lock()
	session := d.sessions[token]
	d.mu.Unlock()
	if session == nil || session.podUID != podUID {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return !session.closed && session.resumesBySession
}

// workspaceReader is a connected, ready launcher session of issue's pod podUID whose hello said it
// reads the issue workspace (launcherSession.readsWorkspace), and nil where there is none. Every
// role of a pod has a launcher connection of its own, and each of them reads the one workspace the
// pod's roles share, so any of them answers the same; the one whose token sorts first is asked, so
// two reads of one pod go to the same launcher while it stays connected.
func (d *launchers) workspaceReader(issue, podUID string) *launcherSession {
	d.mu.Lock()
	candidates := make([]*launcherSession, 0, len(d.sessions))
	for _, session := range d.sessions {
		if session.issue == issue && session.podUID == podUID && session.readsWorkspace {
			candidates = append(candidates, session)
		}
	}
	d.mu.Unlock()
	slices.SortFunc(candidates, func(a, b *launcherSession) int { return strings.Compare(string(a.token), string(b.token)) })
	for _, session := range candidates {
		session.mu.Lock()
		closed := session.closed
		session.mu.Unlock()
		if closed {
			continue
		}
		select {
		case <-session.ready:
			return session
		default:
		}
	}
	return nil
}

var _ runtime.PodWorkspaceReader = (*Runtime)(nil)

// PodWorkspaceWork has the launcher of issue's pod read whether the issue workspace holds work that
// no branch or tag on origin holds, where no role process runs to read through
// (runtime.PodWorkspaceReader, R6 of #5285). The pod is the one the runtime holds
// for the issue now (IssuePod); the launcher is a connected one of that pod whose hello said it
// reads a workspace (launchers.workspaceReader). The read is bounded by the read's own budget
// (runtime.WorkspaceWorkTimeout) and one API call's time for the frame each way. The answer names
// the pod read (runtime.WorkspaceWork.PodUID), and so does the error of a read once it has chosen
// the pod. A pod the runtime cannot name or holds none of, a pod with no such launcher, a
// connection that closes before the answer and a launcher that refuses are each an error, which
// the caller reads as a read that could not be made, never as no work.
func (r *Runtime) PodWorkspaceWork(ctx context.Context, issue string) (runtime.WorkspaceWork, error) {
	pod, read := r.IssuePod(issue)
	switch {
	case !read:
		return runtime.WorkspaceWork{}, fmt.Errorf("read %s's workspace: the runtime cannot say which pod holds it", issue)
	case pod == "":
		return runtime.WorkspaceWork{}, fmt.Errorf("read %s's workspace: the runtime holds no pod of the issue", issue)
	}
	return r.launcherWorkspaceWork(ctx, issue, pod)
}

// launcherWorkspaceWork is PodWorkspaceWork's read of pod, the issue's pod the caller chose: every
// answer, an error's included, names pod.
func (r *Runtime) launcherWorkspaceWork(ctx context.Context, issue, pod string) (runtime.WorkspaceWork, error) {
	none := runtime.WorkspaceWork{PodUID: pod}
	session := r.launchers.workspaceReader(issue, pod)
	if session == nil {
		return none, fmt.Errorf("read %s's workspace: no launcher of pod %s that reads a workspace is connected", issue, pod)
	}
	id := rand.Text()
	frame := shimwire.WorkspaceWork{ID: id, Issue: issue, TimeoutMs: int(runtime.WorkspaceWorkTimeout.Milliseconds())}
	if err := frame.Validate(); err != nil {
		return none, fmt.Errorf("read %s's workspace: %w", issue, err)
	}
	bounded, cancel := context.WithTimeout(ctx, runtime.WorkspaceWorkTimeout+apiTimeout)
	defer cancel()
	answer, _, err := r.launchers.requestSession(bounded, session, frame, id)
	if err != nil {
		return none, fmt.Errorf("read %s's workspace through launcher %s of pod %s: %w", issue, session.token, pod, err)
	}
	result, ok := answer.(shimwire.WorkspaceWorkResult)
	if !ok {
		return none, fmt.Errorf("read %s's workspace through launcher %s: answered by a %s frame", issue, session.token, answer.FrameType())
	}
	work, refused, err := runtime.WorkspaceWorkOf(result)
	switch {
	case err != nil:
		return none, fmt.Errorf("read %s's workspace through launcher %s: %w", issue, session.token, err)
	case refused != "":
		return none, fmt.Errorf("read %s's workspace through launcher %s: the launcher refused: %s", issue, session.token, refused)
	}
	work.PodUID = pod
	return work, nil
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
	return d.requestSession(ctx, session, frame, id)
}

// requestSession sends one frame to session itself. A reconnect after the caller decided what that
// session receives is a refusal rather than a send to its successor: the successor may hold a
// different install, so it must make the decision and receive the push from the beginning.
func (d *launchers) requestSession(ctx context.Context, session *launcherSession, frame shimwire.Frame, id string) (shimwire.Frame, *launcherSession, error) {
	waiter := make(chan shimwire.Frame, 1)
	session.mu.Lock()
	if session.closed || session.write == nil {
		session.mu.Unlock()
		return nil, nil, errors.New("launcher connection closed")
	}
	session.waiters[id] = waiter
	// The write's deadline is apiTimeout's, or ctx's when ctx ends first (ctxBound).
	deadline := time.Now().Add(apiTimeout)
	ctxBound := false
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline, ctxBound = until, true
	}
	err := session.conn.SetWriteDeadline(deadline)
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
		// A write the launcher never took ends at its deadline with the connection's timeout
		// rather than ctx's error, and when that deadline is ctx's, ctx's own timer, set for the
		// same instant, may not have ended ctx yet. Such a write ended because ctx did: it is
		// reported as ctx's end, as the wait for the answer below reports it, so a caller telling
		// its own bound from another (serveLeakCheck's hop, boundRanOut) reads the same error
		// whichever of the two the bound caught.
		if ctxBound && errors.Is(err, os.ErrDeadlineExceeded) {
			<-ctx.Done()
			return nil, nil, fmt.Errorf("%w: %w", ctx.Err(), err)
		}
		return nil, nil, err
	}
	select {
	case answer, ok := <-waiter:
		if !ok {
			return nil, nil, errors.New("launcher connection closed before its answer")
		}
		d.mu.Lock()
		current := d.sessions[session.token]
		d.mu.Unlock()
		if current != session {
			return nil, nil, errors.New("launcher process changed before its answer was confirmed")
		}
		return answer, session, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (d *launchers) current(session *launcherSession) bool {
	d.mu.Lock()
	current := d.sessions[session.token]
	d.mu.Unlock()
	if current != session || session.podUID == "" {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return !session.closed
}

// startRefusal is a Start the launcher answered OK: false, without starting the generation: the one
// start error after which no child of that generation can be running (stopIssuedChild). A refusal
// over the session it was asked to resume, by its file or by its id alone, carries the launcher's
// own verdict as its kind (shimwire.SessionRefusal); this is the one place that kind becomes the
// sentinel the supervisor acts on, so a lost session and a store that could not be asked reach it
// as they do from the tmux runtime (runtime.ErrSessionFileMissing, runtime.ErrSessionStore). A
// session the launcher's table holds no row of under the id it was named by is as gone as a file
// the table does not hold, and the launcher answers both shimwire.SessionGone.
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

func (d *launchers) start(ctx context.Context, token claim.Token, session *launcherSession, command shimwire.LauncherStart) error {
	// serveLeakCheck chose this exact session and installed the check before the command was built.
	// The start stays on it: a replacement session has not received that push, so this launch
	// refuses and the supervisor retries from the new hello rather than starting a role without
	// the checker its environment names.
	if d.beforeStart != nil {
		d.beforeStart(ctx, token)
	}
	answer, _, err := d.requestSession(ctx, session, command, command.ID)
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
// a few slices of shimwire.LauncherSlicePayloadBytes, the launcher's unpack, its one `--help`
// run and its rename, and this is a slow connection's room. It runs under the start's own
// context, so the launch's own bound needs no term for it. A hop that
// outruns it refuses the start; the launcher's install goes on under its own bound, and the next
// start is answered Installed at once.
const leakCheckHopBound = 2 * time.Minute

// serveLeakCheck reads, records and pushes the shared public leak check for token's exact
// launcher session. prepare decides the digest the role will be told from the served body after
// that session has connected and before its first slice goes over; it may persist that decision.
// A replacement session at any point refuses the launch, so no role starts on a session the
// decision did not cover.
func (d *launchers) serveLeakCheck(ctx context.Context, token claim.Token, podUID string,
	prepare func(*launcherSession, leakguard.Served) (string, error),
) (*launcherSession, error) {
	refuse := func(what string, err error) error {
		return runtime.LeakCheckRefusal{Cause: leakguard.Cause(what, err)}
	}
	// A launcher that never arrives is the launch's own failure, not the check's: a deployment
	// that names no check refuses nothing for it, and the deadline it ran out on stays in the
	// error's chain, which relaunch notes as the try's bound (boundRanOut). No digest is decided
	// before the session that would receive the push exists.
	session, err := d.await(ctx, token, podUID)
	if err != nil {
		return nil, fmt.Errorf("launcher %s did not connect: %w", token, err)
	}
	tag := session.leakCheckTag
	session.mu.Lock()
	installed := session.leakCheckInstalled
	session.mu.Unlock()
	if tag == "" || installed || !d.leakCheckSetting.Configured() {
		return session, nil
	}
	if d.leakCheck == nil {
		return nil, refuse("the daemon runs with no GitHub App, so it cannot read "+d.leakCheckSetting.Repository.String(), nil)
	}
	served, err := d.leakCheck.Get(ctx, tag)
	if err != nil {
		return nil, refuse(err.Error(), nil)
	}
	if served.ManifestSHA256 == "" {
		return nil, refuse("the check the daemon read at "+tag+" has no manifest digest", nil)
	}
	if !d.current(session) {
		return nil, refuse("the pod's launcher reconnected before the shared public leak check could be pushed", nil)
	}
	checker, err := prepare(session, served)
	if err != nil {
		return nil, refuse("the daemon could not record the shared public leak check it will push", err)
	}
	if checker != served.ManifestSHA256 {
		return nil, refuse(fmt.Sprintf("the check the daemon served at %s has manifest digest %s, but the role would be told %s",
			tag, served.ManifestSHA256, checker), nil)
	}
	hop, cancel := context.WithTimeout(ctx, d.leakCheckHop)
	defer cancel()
	for _, frame := range served.Frames {
		answer, confirmed, err := d.requestSession(hop, session, frame, sliceWaiter(frame.ID, frame.Index))
		if err != nil {
			if hop.Err() != nil && ctx.Err() == nil {
				return nil, refuse(fmt.Sprintf("the pod's launcher did not answer the install of %s within %s", tag, d.leakCheckHop), nil)
			}
			return nil, refuse(fmt.Sprintf("the pod's launcher did not answer the install of %s", tag), err)
		}
		result, ok := answer.(shimwire.LauncherLeakCheckResult)
		if !ok {
			return nil, refuse(fmt.Sprintf("the pod's launcher answered slice %d of %s with a %s frame", frame.Index, tag, answer.FrameType()), nil)
		}
		if err := result.Validate(); err != nil {
			return nil, refuse(fmt.Sprintf("the pod's launcher answered slice %d of %s with a malformed result", frame.Index, tag), err)
		}
		if !result.OK {
			return nil, refuse(result.Error, nil)
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
			"launcher", token, "pod", podUID, "repository", d.leakCheckSetting.Repository.String(), "tag", tag,
			"manifest_sha256", served.ManifestSHA256, "configured_manifest_sha256", d.leakCheckSetting.ManifestSHA256,
			"bytes", len(served.Body), "slices", len(served.Frames))
		return session, nil
	}
	return nil, refuse(fmt.Sprintf("the pod's launcher took every slice of %s without installing it", tag), nil)
}

// keptPodRecordBound is how long the record of one pod of a kept volume may take (recordKeptPod):
// one row, which the store writes again while it fails for a reason that passes, so this is room
// for a store slow to answer rather than for the common case.
const keptPodRecordBound = 2 * time.Minute

// recordKeptPod records the launcher session's pod, whose Sandbox owns the issue's kept volume
// (launcherSession.keptSince), with that volume at each role's start in it (keptStart,
// store.StartKeptPod), before the role's start is sent (relaunch, before launchers.start). A kept
// volume outlives its pod, so what the roles of this pod leave unpushed stays on the volume for
// the issue's next pod of it, and an end of the update round over that later pod reads the record
// for whom it names (store.PodHoldsEarlierWork). A pod on a volume of its own, the project
// controller's pod, and a launchers built without keptStart record nothing.
//
// The store runs a write that failed for a reason that passes again, until a run commits or the
// call's context ends: the record's bound (keptPodRecordBound) or the start's own deadline,
// whichever comes first. A call that still fails is answered as a refusal of the role's start
// (runtime.KeptPodUnrecorded), which relaunch returns before the Start frame is sent and the
// supervisor logs and charges as a launch failure; the next start asks again. A role that ran in
// a pod the store does not record could leave work on the volume that the round over a later pod
// of it would name no role for, and the record is in the store so that a daemon restart keeps it
// too.
func (d *launchers) recordKeptPod(ctx context.Context, session *launcherSession) error {
	if d.keptStart == nil || session.issue == "" || session.keptSince.IsZero() {
		return nil
	}
	recording, cancel := context.WithTimeout(ctx, keptPodRecordBound)
	defer cancel()
	if err := d.keptStart(recording, session.issue, session.podUID, session.keptSince); err != nil {
		return runtime.KeptPodUnrecorded{PodUID: session.podUID, KeptSince: session.keptSince, Err: err}
	}
	return nil
}

// earlierRestoreReport reads and drops one launcher_restored frame (shimwire.LauncherRestored).
// A launcher of an earlier release sends it, unasked, after it restored a released pod's saved
// work into the pod that replaced it, and again on each later connection until one carries it.
// Such a launcher is one an earlier daemon started, still running after this daemon took over.
// This daemon saves no work and restores none, so the report confirms nothing it holds. A frame
// the loop does not know ends the connection (ServeLauncher's `default: return`), which would cost
// that launcher a redial and every answer pending on the connection, so the frame is decoded,
// logged in one line naming the launcher, and dropped: a report of a restore that failed at Warn,
// since the issue's workspace then holds only what was pushed, and any other at Info.
func (d *launchers) earlierRestoreReport(s *launcherSession, report shimwire.LauncherRestored) {
	level := slog.LevelInfo
	if !report.OK {
		level = slog.LevelWarn
	}
	d.log.Log(context.Background(), level, "launcher: an earlier launcher reported a restore of a released workspace's saved work, which this daemon does not keep; the report is dropped and the connection stays open",
		"issue", s.issue, "launcher", s.token, "pod", s.podUID, "sequence", report.ID, "release", report.Release, "ok", report.OK, "error", report.Error)
}

// memoryKilled hands the daemon one process of a role that was stopped because the pod was out
// of memory, by the pod's memory guard or, in a microVM pod, by the pod's own kernel, as the
// launcher reported it on the connection of the role whose process it was
// (shimwire.LauncherMemoryKill). The launcher sends it unasked, and only on a connection whose
// acknowledgement carried guard settings (a gVisor pod's launcher) or KernelKills (a microVM
// pod's), which is a daemon that knows the frame and its cause. The report goes on whole, its
// cause included, which chooses the note the role is told (workflow's memoryGuardKill).
//
// The issue, the role and the pod the kill is credited to are the session's own, as the hello's
// authentication bound them (launcherCredential), and never anything the frame says: a launcher
// cannot tell another role, issue or pod of a kill. The report's guard and sequence are what
// apply it once however often it arrives (the event id the daemon applies it under).
//
// A session of no issue is the controller's pod, whose one role is the project's controller: its
// report goes to the controller's receiver (Options.ControllerMemoryGuardKilled), which tells the
// controller of the kill, with the session's token and pod. Every other session is an issue
// pod's, and its report goes to the issue's receiver (Options.MemoryGuardKilled) with the
// session's issue as well.
//
// A report the frame's own rule refuses (LauncherMemoryKill.Validate), and one whose receiver
// this runtime was not given, are logged naming the launcher and dropped; the second names the
// line that sets the missing receiver. None of the drops closes the connection: closing costs the
// launcher a redial a second later (ServeLauncher's `default: return`) over a frame the daemon
// could not have used anyway, and the launcher's other frames would go with it.
//
// It runs on the launcher's own frame loop rather than in a goroutine of its own, under a bounded
// context of its own (apiTimeout): two reports of one launcher reach the fact path in the order
// the guard made them, and the cost is the launcher's other frames waiting behind one fact's
// transaction.
func (d *launchers) memoryKilled(s *launcherSession, report shimwire.LauncherMemoryKill) {
	if err := report.Validate(); err != nil {
		d.log.Warn("launcher memory guard: the pod's launcher sent a kill report the daemon cannot apply, which is dropped; the connection stays open",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID, "error", err)
		return
	}
	controller := s.issue == ""
	if (controller && d.controllerKilled == nil) || (!controller && d.guardKilled == nil) {
		// A runtime built with no receiver for a pod kind drops every report of every pod of that
		// kind, and nothing else shows it: nobody is told why the process is gone. So the drop
		// names the line that sets the receiver.
		setter := "opts.MemoryGuardKilled"
		if controller {
			setter = "opts.ControllerMemoryGuardKilled"
		}
		d.log.Warn("launcher memory guard: the pod's launcher reported a kill and this runtime has no receiver for it, which is dropped; "+
			"the daemon sets one in internal/daemon/kubernetes.go's sandboxRuntime ("+setter+")",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	var err error
	if controller {
		err = d.controllerKilled(ctx, s.token, s.podUID, report)
	} else {
		err = d.guardKilled(ctx, s.issue, s.token, s.podUID, report)
	}
	if err != nil {
		d.log.Warn("launcher memory guard: the pod's launcher reported a kill the daemon could not record, which is dropped",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID, "error", err)
	}
}

// volumeReported hands the daemon one sweep of the pod's volume as that pod's cache trimmer
// reported it (shimwire.LauncherVolumeReport). The launcher sends one unasked per sweep that
// ended with the volume still above its low mark, unless the settings stopped that sweep, and
// one when the trimmer has switched itself off; a sweep that reached the low mark sends none, so
// every frame that arrives here is one the issue's architect may need to hear about, and the
// daemon's own bounds decide which of them it is told (workflow's volumeNearFull).
//
// The pod a report is of is the one this connection authenticated for, so the uid is the
// session's and never the report's: a launcher cannot name another pod's volume. The trimmer's
// own identifier is inside the report, which is what counts a restarted roles container apart
// from the first episode.
//
// A store that does not answer is logged and the report is dropped here. The launcher cannot
// hold it again once the daemon has read it — nothing acknowledges this frame — and it is not
// asked to: the trimmer goes on sweeping, and its next sweep that ends above the low mark reports
// the volume again under the same trimmer, and under the same episode unless a recovery or a rise
// raised it in between, so what a dropped sweep's report would have said reaches the architect
// from a later one, within the notice bounds. A dropped report of a trimmer that switched itself
// off is not sent again: that trimmer sends nothing more.
//
// A report the frame's own rule refuses (LauncherVolumeReport.Validate) — one naming no trimmer,
// which is the identity the notice bounds count a restarted roles container apart by, one whose
// episode is below the 1 the trimmer counts from, and one holding more entries in a list than
// that list's cap, every entry of which the notice would carry — is logged with the reason and
// dropped. The connection is not closed on it: closing costs a redial a second of
// the one role's connection that carries the pod's reports (ServeLauncher's `default: return`),
// and it would not have the report sent again: nothing acknowledges this frame, and a launcher
// holds a report only where its send failed (internal/launcher's deliver).
//
// It runs on the launcher's own frame loop rather than in a goroutine of its own, as
// memoryKilled does and under the same bounded context: two reports of one launcher then
// reach the fact path in the order the trimmer made them, which is what the notice bounds count
// episodes by, and the cost is the one memoryKilled already accepts — the launcher's other
// frames wait behind one row's write.
func (d *launchers) volumeReported(s *launcherSession, report shimwire.LauncherVolumeReport) {
	if err := report.Validate(); err != nil {
		d.log.Warn("launcher volume: the pod's cache trimmer sent a report the daemon refuses, which is dropped; the connection stays open",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "trimmer", report.Trimmer, "episode", report.Episode, "error", err)
		return
	}
	if d.volume == nil {
		// A runtime built with no receiver drops every report of every pod, and nothing else shows
		// it: no architect is told and the trimmer goes on sweeping. So the drop is logged naming
		// the line that sets the receiver.
		d.log.Warn("launcher volume: the pod's cache trimmer reported a sweep and this runtime has no receiver for it, which is dropped; "+
			"the daemon sets one in internal/daemon/kubernetes.go's sandboxRuntime (opts.VolumeReported = reports.report)",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "trimmer", report.Trimmer, "episode", report.Episode)
		return
	}
	if s.issue == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	if err := d.volume(ctx, s.issue, s.podUID, report); err != nil {
		d.log.Warn("launcher volume: the pod's cache trimmer reported a sweep the daemon could not record, which is dropped",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "trimmer", report.Trimmer, "episode", report.Episode, "error", err)
	}
}

// sliceWaiter names the request one slice's answer belongs to, of a launcher_leak_check sequence.
// A sequence is answered slice by slice, so its id alone would name as many answers as it has
// slices.
func sliceWaiter(sequence string, index int) string { return sequence + "/" + strconv.Itoa(index) }

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

// bindLauncherSecrets binds the Secret of each of roles, launcher roles of s's pod, to that pod's
// uid, so only that pod's launchers authenticate with it, and holds each binding it wrote as its
// role's credential (cacheLauncherCredential). A launch binds every role of the new pod it made
// (relaunch); a hello binds the roles of a pod no launch made that no held credential binds yet
// (bindAtHello). A write the API server refuses as a conflict — the gh-credential refresher
// rewrote the Secret between this read and this write (refreshGitHubCredential) — is retried once
// over a fresh read, so the refresher's gh files and the binding both land; a launch that failed
// here instead would leave the new pod running, unbound, until the supervisor's next attempt
// replaced it. Any other failure, or a second conflict, is returned.
func (r *Runtime) bindLauncherSecrets(ctx context.Context, s *sandbox, roles []claim.Role, uid string) error {
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	for _, role := range roles {
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
// trusted. A pod the Agent Sandbox controller recreated outside a launch is bound once one of its
// launchers says hello with its role's current token (bindAtHello), so a launch after that hello
// keeps it. Any other failure to read a Secret, such as the client's timeout, or a throttled request
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
// launch credentials and files it writes into its private directory, and the session a resume
// continues: its file (ResumeFile), which the launcher looks the session up by whether or not the
// agent's argv resumes it by id (agentArgv), or for a claim that records no file the session's id
// (ResumeSession), which the launcher looks up in the session table and resumes the file it finds
// in place of the agent argv's one `--resume=<id>` (agentArgv). A launch that has the agent's own
// command resolve the id (ResumeByAgent), which supervise makes after relaunch refused to name the
// id to a launcher that has not said it can look it up, names neither: whatever launcher receives
// it looks nothing up and starts the child as it starts any child with no ResumeFile, and that
// child's Oh My Pi resolves the `--resume=<id>` itself. Every launch's Files carry its composed
// system prompt as runtime.SystemPromptFile, which the launcher writes before the child starts and
// whose path the agent's argv names, Oh My Pi's and Claude Code's alike (systemPromptPath says
// where it is and who can read it; runtime.SystemPromptFile why it is a file).
//
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
// and never the binary), and adds one more file to the generation's: the MCP document its agent
// command reads (claudeMCPFile), which the launcher writes and removes as it does the prompt. Both
// travel in Files and never in spec.Secrets, which would be two wrongs: every secret name also
// gets a `<NAME>_FILE` pointer in the worker's environment (mainEnvironment), and
// ValidateSpawnSpec refuses a credential-shaped name outright.
//
// The agent-secrets flags are the one part of the command built from the pod the role will run in
// rather than from the configuration as it now reads (launch.enrolls): the shim stats the key
// directory and the token file those flags name and exits 1 when either is absent
// (shim.ReadAgentSecretsFlags), and a live pod a launch keeps may have been built before the
// runtime enrolled, so it carries neither. Such a role is started without them and runs without
// the broker until the update round replaces its pod (INFRA-484, keptPod) — once every worker of
// the pod has done the steps of its update message (supervise.UpdateTask) and answered, no turn
// runs, no take whose turn ended still owes its report and no claim's activity report holds it
// (internal/daemon's roundEnd.holding), or at the round's bound — and a process that refused to
// start would push nothing and answer nothing.
// The environment that start carries says the same thing: the AGENT_SECRETS_URL and
// AGENT_SECRETS_KEY_DIR an agent reads to decide it holds a broker are set from this one decision
// too (mainEnvironment, enrollsIn), so such a role reports a resource it cannot reach rather than
// a secrets problem it does not have.
//
// An Oh My Pi start names the agent's executable apart from the argv (AgentExecutable): the first
// word of the agent's own argv (agentArgv), the executable the shim's child runs once any start
// wrapper has exec'd it. The argv itself cannot say it, since the wrapper above makes the child's
// first word /bin/sh. A launcher whose session database names a writer audience knows the
// generation's Oh My Pi process by that executable, and admits it alone of the role's processes to
// the generation's session socket (internal/launcher's sessionPeer). A Claude Code start names
// none: its agent opens no session socket of its own.
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
	// as itself, or as the service account it impersonates — so a deployment with no provider key
	// and no providers secret has no such directory.
	if r.mountsProviders() {
		shim = append(shim, "--provider-env-dir", ProvidersDir)
	}
	if l.enrolls(r) {
		shim = append(shim, "--agent-secrets-key-dir", AgentSecretsKeyDir, "--pod-token-file", AgentSecretsTokenDir+"/"+AgentSecretsTokenFile, "--"+shimwire.AgentSecretsBinFlag, r.tools.AgentSecrets)
	}
	_, values := r.launchEnvironment(l)
	values = append(values, bootTokenKey+"_FILE="+dir+"/"+bootTokenKey)
	files := maps.Clone(l.secrets)
	if files == nil {
		files = map[string]string{}
	}
	files[bootTokenKey] = l.spec.BootToken
	files[runtime.SystemPromptFile] = l.prompt
	claudeDir := ""
	if l.runsClaude() {
		claudeDir = claudeConfigDir(l.spec.Role)
		files[claudeMCPFile] = l.claude.mcp
	}
	agent := l.agentArgv(r.agent)
	agentArgv := startArgv(roleStateHome(l.spec.Role), l.spec.Generation, l.spec.Plugins, claudeDir, r.setup,
		agent, installLockWait(r.bootTimeout, r.registrationIntervals))
	command := shimwire.LauncherStart{
		ID: "start-" + fmt.Sprint(l.spec.Generation), Generation: l.spec.Generation,
		Argv: append(shim, append([]string{"--"}, agentArgv...)...), Env: values, Files: files,
		ResumeFile: l.spec.ResumeSessionFile,
	}
	if !l.spec.ResumeByAgent {
		command.ResumeSession = l.spec.ResumeSession
	}
	if !l.runsClaude() {
		command.AgentExecutable = agent[0]
	}
	return command
}

// checkStartFrame refuses command, the start l's role would be sent, when the frame that carries
// it would be longer than a launcher reads (shimwire.FrameLen, shimwire.MaxFrameBytes): the one
// bound a prompt still meets in a pod (runtime.PromptTooLarge, which supervise charges no launch
// failure). prepare runs it on the start a pod built now would receive, before anything of the pod
// is touched, and relaunch again on the start it sends, which carries what the launch learned from
// the cluster since — the gh bundle's default owner, and for a pod it keeps, the overlays and
// enrollment that pod was built with.
func (l launch) checkStartFrame(command shimwire.LauncherStart) error {
	frame, err := shimwire.FrameLen(command)
	if err != nil {
		return err
	}
	if frame > shimwire.MaxFrameBytes {
		return &runtime.PromptTooLarge{Role: l.spec.Role, Bytes: len(l.prompt), Frame: frame}
	}
	return nil
}
