package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testLeakCheckTag is the pin a fake launcher's hello names when a test gives it one.
const testLeakCheckTag = "leak-check-v3"

// fakeLeakCheck stands in for the daemon's reader of the shared public leak check
// (leakguard.Source): it answers body at any tag, sliced as the reader slices it, or refuses every
// read with err, and records the tag of each read.
type fakeLeakCheck struct {
	body []byte
	err  error

	mu   sync.Mutex
	tags []string
}

func (f *fakeLeakCheck) Get(_ context.Context, tag string) (leakguard.Served, error) {
	f.mu.Lock()
	f.tags = append(f.tags, tag)
	f.mu.Unlock()
	if f.err != nil {
		return leakguard.Served{}, f.err
	}
	frames, err := shimwire.SplitLeakCheck(tag, f.body)
	if err != nil {
		return leakguard.Served{}, err
	}
	return leakguard.Served{Tag: tag, Body: f.body, Frames: frames}, nil
}

func (f *fakeLeakCheck) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tags)
}

// servedLeakCheck is a reader whose packed check is more than one
// shimwire.LauncherBundlePayloadBytes, so the push is a sequence of slices and not one frame.
func servedLeakCheck() *fakeLeakCheck {
	return &fakeLeakCheck{body: bytes.Repeat([]byte("leak-check\x00"), shimwire.LauncherBundlePayloadBytes/11+4096)}
}

// leakCheckStore is a store holding the issue's saved workspace bundle, as a released issue's
// does when its next pod starts: the bundle's one restore attempt is what a refused start must
// not spend.
func leakCheckStore(t *testing.T) (*store.Store, runtime.WorkspaceBundle) {
	t.Helper()
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	return st, saved
}

// withLeakCheckAndStore hands the runtime source as its reader of the leak check and st's saved
// workspace bundles.
func withLeakCheckAndStore(source LeakCheckReader, st *store.Store) rigOption {
	return withOptions(func(o *Options) {
		o.LeakCheck = source
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = st.TakeWorkspaceBundleForRestore, st.DeleteRestoredWorkspaceBundle
	})
}

// countOf is how many of frames are of the frame type of want.
func countOf(frames []shimwire.Frame, want string) int {
	n := 0
	for _, frame := range frames {
		if frame.FrameType() == want {
			n++
		}
	}
	return n
}

// A launcher whose hello names its pin is sent the check the daemon read at that pin before
// anything else of the start: every slice of it, in order, before the first slice of the saved
// workspace bundle and before the role's start, since the launcher installs the check once for
// the pod and every role's shim refuses to start its agent without that install.
func TestALauncherNamingItsPinIsSentTheLeakCheckBeforeTheBundleAndTheStart(t *testing.T) {
	st, saved := leakCheckStore(t)
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withLeakCheckAndStore(source, st))
	spec := workerSpec(t)
	g.spawn(spec)

	frames := g.sent(spec.Claim)
	start := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok })
	firstBundle := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherBundle); return ok })
	if start < 0 || firstBundle < 0 {
		t.Fatalf("the launcher was sent %v, want the check's slices, the bundle's slices and then the role's start", frameTypes(frames))
	}
	var body []byte
	var sequence string
	slicesSent := 0
	for i, frame := range frames[:firstBundle] {
		slice, ok := frame.(shimwire.LauncherLeakCheck)
		if !ok {
			t.Fatalf("the launcher was sent %v, want the check's slices before anything else", frameTypes(frames))
		}
		if i == 0 {
			sequence = slice.ID
		}
		if slice.ID != sequence || slice.Index != i || slice.Count != firstBundle || slice.Tag != testLeakCheckTag {
			t.Fatalf("slice %d of %d of %s under %q arrived at position %d of the %d sent before the bundle",
				slice.Index, slice.Count, slice.Tag, slice.ID, i, firstBundle)
		}
		body = append(body, slice.Payload...)
		slicesSent++
	}
	if slicesSent < 2 {
		t.Fatalf("the check crossed in %d slice(s), want the sequence of the %d bytes read", slicesSent, len(source.body))
	}
	if !bytes.Equal(body, source.body) {
		t.Fatalf("the check crossed as %d bytes, want the %d the daemon read", len(body), len(source.body))
	}
	if got := countOf(frames, shimwire.TypeLauncherLeakCheck); got != slicesSent {
		t.Fatalf("the launcher was sent %v: a slice of the check crossed after the bundle began", frameTypes(frames))
	}
	if firstBundle > start {
		t.Fatalf("the launcher was sent %v, want the bundle's slices before the role's start", frameTypes(frames))
	}
	if reads := source.reads(); !slices.Equal(reads, []string{testLeakCheckTag}) {
		t.Fatalf("the daemon read the check at %v, want once at the launcher's pin %s", reads, testLeakCheckTag)
	}
	// The bundle went over as it does with no check at all, and the launcher's report of its
	// restore dropped it.
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("after the launcher reported its restore the store holds the bundle = %t (%v), want it deleted (%d bytes saved)", held, err, len(saved.Bundle))
	}
}

// A launcher whose hello names no pin takes no check from the daemon — an image older than the
// push, or a pod whose image carries no pin — and its role starts as it always has: the daemon
// reads nothing and sends it no slice.
func TestALauncherNamingNoPinIsSentNoLeakCheckAndStarts(t *testing.T) {
	source := servedLeakCheck()
	g := newRig(t, nil, withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != spec.Generation {
		t.Fatalf("the launch left %+v, want generation %d", loc, spec.Generation)
	}
	frames := g.sent(spec.Claim)
	if countOf(frames, shimwire.TypeLauncherLeakCheck) != 0 || countOf(frames, shimwire.TypeLauncherStart) != 1 {
		t.Fatalf("the launcher was sent %v, want the role's start and no slice of the check", frameTypes(frames))
	}
	if reads := source.reads(); len(reads) != 0 {
		t.Fatalf("the daemon read the check at %v for a launcher that names no pin", reads)
	}
}

// A launcher whose hello says it holds its pin installed — its pod's launcher process took the
// check before the daemon restarted — is sent nothing of it, and the daemon does not read
// GitHub for a running pod.
func TestALauncherHoldingItsPinInstalledIsSentNothingAndStarts(t *testing.T) {
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, true), withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	g.spawn(spec)
	frames := g.sent(spec.Claim)
	if countOf(frames, shimwire.TypeLauncherLeakCheck) != 0 || countOf(frames, shimwire.TypeLauncherStart) != 1 {
		t.Fatalf("the launcher was sent %v, want the role's start and no slice of the check", frameTypes(frames))
	}
	if reads := source.reads(); len(reads) != 0 {
		t.Fatalf("the daemon read the check at %v for a launcher that holds it installed", reads)
	}
}

// A check that could not be put in place refuses the role's start before anything of the start
// reaches the launcher: no start frame, so no stop of one either, and no slice of the saved
// workspace bundle, which stays in the store for the start that follows (the bundle's one restore
// attempt is not spent on a pod no role ran in). The error carries the cause sentence, which the
// claim reads (runtime.LeakCheckCause), wrapped as every start error of the launch is.
func TestARefusedLeakCheckStartsNothingAndLeavesTheSavedBundle(t *testing.T) {
	podsLauncher := leakguard.Cause("the pod's launcher could not unpack the check the daemon served", errors.New("gzip: invalid header"))
	for name, tc := range map[string]struct {
		source  LeakCheckReader
		options []rigOption
		want    string
	}{
		"the daemon holds no credential": {
			want: leakguard.Cause("the daemon runs with no GitHub App, so it cannot read theorem-labs/agent-infra", nil),
		},
		"the daemon's read is refused": {
			source: &fakeLeakCheck{err: errors.New(leakguard.Cause("GitHub answered the read of theorem-labs/agent-infra at "+testLeakCheckTag+" with HTTP 404: Not Found", nil))},
			want:   leakguard.Cause("GitHub answered the read of theorem-labs/agent-infra at "+testLeakCheckTag+" with HTTP 404: Not Found", nil),
		},
		"the pod's launcher refuses the install": {
			source:  servedLeakCheck(),
			options: []rigOption{withRefusedLeakCheck(podsLauncher)},
			want:    podsLauncher,
		},
		"the pod's launcher refuses in words of its own": {
			source:  servedLeakCheck(),
			options: []rigOption{withRefusedLeakCheck("the staging directory could not be made: no space left on device")},
			want:    leakguard.RefusalPrefix + "the staging directory could not be made: no space left on device",
		},
		"the pod's launcher refuses with no cause": {
			source:  servedLeakCheck(),
			options: []rigOption{withCauselessLeakCheck()},
			want: leakguard.Cause("the pod's launcher answered slice 0 of "+testLeakCheckTag+" with a malformed result",
				shimwire.LauncherLeakCheckResult{ID: "x"}.Validate()),
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, saved := leakCheckStore(t)
			options := append([]rigOption{withLeakCheckHello(testLeakCheckTag, false), withLeakCheckAndStore(tc.source, st)}, tc.options...)
			g := newRig(t, nil, options...)
			spec := workerSpec(t)
			g.launcher(spec.Claim)
			_, err := g.r.Spawn(g.ctx, spec)
			if err == nil {
				t.Fatalf("the launch started the role, want it refused with %q", tc.want)
			}
			cause := runtime.LeakCheckCause(err)
			if cause != tc.want || !strings.HasPrefix(cause, leakguard.RefusalPrefix) {
				t.Fatalf("the launch failed with %q carrying the cause %q, want %q", err, cause, tc.want)
			}
			if want := "launch " + string(spec.Claim) + ": start through its authenticated launcher: " + tc.want; err.Error() != want {
				t.Fatalf("the launch failed with %q, want %q", err, want)
			}
			frames := g.sent(spec.Claim)
			for _, sent := range []string{shimwire.TypeLauncherStart, shimwire.TypeLauncherStop, shimwire.TypeLauncherBundle} {
				if countOf(frames, sent) != 0 {
					t.Fatalf("the launcher was sent %v, want no %s after the refusal", frameTypes(frames), sent)
				}
			}
			pod := g.pod(SandboxName(spec.Claim))
			if pod == nil {
				t.Fatal("the refused launch left no pod")
			}
			bundle, take, err := st.TakeWorkspaceBundleForRestore(g.ctx, testTree, string(pod.UID), pod.CreationTimestamp.Time)
			if err != nil {
				t.Fatal(err)
			}
			if take != runtime.WorkspaceBundleSend || !bytes.Equal(bundle.Bundle, saved.Bundle) {
				t.Fatalf("after the refusal the store answers the pod %d bytes (take %v), want the %d saved still to send",
					len(bundle.Bundle), take, len(saved.Bundle))
			}
		})
	}
}

// The push has its own bound under the start's: a launcher that takes the check and answers
// nothing within it refuses the start with the sentence that names the bound, and no start is
// sent. The bound runs on the runtime's clock (withClock), which the row moves past it only once
// the first slice has reached the launcher and the hop's bound is armed on that clock, so a
// loaded machine that is slow to carry the slice ends no hop the row did not end.
func TestALeakCheckHopThatRunsPastItsBoundRefusesTheStart(t *testing.T) {
	fake := clocktest.New(rigNow)
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withSilentLeakCheck(),
		withOptions(func(o *Options) { o.LeakCheck = servedLeakCheck() }), withClock(fake))
	const bound = 50 * time.Millisecond
	g.r.launchers.leakCheckHop = bound
	spec := workerSpec(t)
	g.launcher(spec.Claim)
	launched := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, spec)
		launched <- err
	}()
	testwait.Eventually(t, "the first slice of the leak check to reach the launcher", func() bool {
		return countOf(g.sent(spec.Claim), shimwire.TypeLauncherLeakCheck) > 0
	})
	// The hop arms its bound before it sends the first slice, and nothing moves the fake but this
	// row, so the bound stands at its whole length from where the fake stands.
	if hopEnds, armed := fake.Now().Add(bound), fake.Armed(); !slices.Contains(armed, hopEnds) {
		t.Fatalf("with a slice of the check at the launcher the runtime's clock holds %v armed, want the hop's %s bound at %v", armed, bound, hopEnds)
	}
	fake.Advance(bound)
	err := testwait.Receive(t, "the launch to end once the hop ran past its bound", launched)
	want := leakguard.Cause("the pod's launcher did not answer the install of "+testLeakCheckTag+" within 50ms", nil)
	if cause := runtime.LeakCheckCause(err); cause != want {
		t.Fatalf("the launch failed with %v carrying the cause %q, want %q", err, cause, want)
	}
	frames := g.sent(spec.Claim)
	if countOf(frames, shimwire.TypeLauncherStart) != 0 || countOf(frames, shimwire.TypeLauncherStop) != 0 {
		t.Fatalf("the launcher was sent %v, want no start and no stop after the hop ran out", frameTypes(frames))
	}
}

// lateHop is the runtime's clock with one difference: a hop's bound (WithTimeout) is a context
// whose Deadline is the bound's end while the context itself ends only when the row calls end. It
// is a context.WithDeadline read in the instant after its deadline and before its timer has run:
// the order a scheduler may give the two things due at the bound's end, the write deadline a
// request takes from that Deadline and the context's own timer.
type lateHop struct {
	clock.Clock

	mu   sync.Mutex
	ends []context.CancelFunc
}

func (c *lateHop) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.ends = append(c.ends, cancel)
	c.mu.Unlock()
	return lateDeadline{Context: ctx, at: c.Now().Add(d)}, cancel
}

// end ends every hop the clock gave.
func (c *lateHop) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, end := range c.ends {
		end()
	}
}

// lateDeadline is a context that reports at as its deadline and ends only when it is cancelled.
type lateDeadline struct {
	context.Context
	at time.Time
}

func (c lateDeadline) Deadline() (time.Time, bool) { return c.at, true }

// A hop whose bound ends while a slice is still being written is refused by the bound, whichever
// of the two things due at the bound's end runs first: the connection's write deadline, which the
// request takes from the hop's Deadline once less than apiTimeout of the hop is left, or the hop's
// own timer. The row holds the second back (lateHop), so the write's timeout comes back while the
// hop still reads as live: the order that refused a start with `write pipe: i/o timeout` under
// load (dispatch://INFRA-3149, plan line L24). Its hop's bound is below apiTimeout, as what is
// left of a production hop is when its last slices are written. It runs in a synctest bubble,
// whose clock the pipe's deadline and the row share, against a launcher that sends its state and
// never reads a slice.
func TestALeakCheckSliceWhoseWriteOutlivesTheHopIsRefusedByTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		launchers := testLaunchers()
		launchers.leakCheck = servedLeakCheck()
		const bound = 50 * time.Millisecond
		launchers.leakCheckHop = bound
		late := &lateHop{Clock: clock.Real()}
		launchers.timers = late
		session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "launcher", LeakCheckTag: testLeakCheckTag})
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
		go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
		if err := shimwire.NewWriter(peer).WriteFrame(shimwire.LauncherState{}); err != nil {
			t.Fatal(err)
		}
		refused := make(chan error, 1)
		go func() { refused <- launchers.serveLeakCheck(context.Background(), workerToken, "pod") }()
		// Past the bound on the bubble's clock, and short of apiTimeout: the first slice's write
		// has timed out on the hop's Deadline, and nothing has ended the hop yet.
		time.Sleep(bound + time.Second)
		synctest.Wait()
		late.end()
		err := <-refused
		want := leakguard.Cause(fmt.Sprintf("the pod's launcher did not answer the install of %s within %s", testLeakCheckTag, bound), nil)
		if cause := runtime.LeakCheckCause(err); cause != want {
			t.Fatalf("the hop refused with %v carrying the cause %q, want %q: the write the hop's deadline ended is the bound's end", err, cause, want)
		}
	})
}

// A launcher that installed the check on this connection is pushed it no more: the next start
// of its role sends the role's start alone.
func TestTwoStartsOnOneConnectionPushTheLeakCheckOnce(t *testing.T) {
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	first := g.spawn(spec)
	pushed := countOf(g.sent(spec.Claim), shimwire.TypeLauncherLeakCheck)
	if pushed < 2 {
		t.Fatalf("the first start pushed %d slice(s) of the check, want its sequence", pushed)
	}
	spec.Generation = first.Sandbox.Generation + 1
	second := g.spawn(spec)
	if second.Sandbox.PodUID != first.Sandbox.PodUID {
		t.Fatalf("the second start ran in pod %s, want the first's %s on the same connection", second.Sandbox.PodUID, first.Sandbox.PodUID)
	}
	frames := g.sent(spec.Claim)
	if got := countOf(frames, shimwire.TypeLauncherLeakCheck); got != pushed || countOf(frames, shimwire.TypeLauncherStart) != 2 {
		t.Fatalf("the launcher was sent %v, want the check's %d slices once and two starts", frameTypes(frames), pushed)
	}
}
