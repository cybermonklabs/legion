package sandbox

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A request whose context ends once the launcher holds the whole frame leaves the launcher's
// connection open: the frame is the launcher's, and the request that follows reaches the same
// launcher. The daemon's end of the connection ends the Start request's context the moment its
// write of the Start has returned, before WriteFrame returns to writeFrame, which is the narrowest
// window there is between delivery and the request's end. An end of the context there used to close
// the connection (context.AfterFunc(ctx, conn.Close)), so the Stop of what an unanswered Start may
// have begun found no launcher; the end now moves the write deadline, which a delivered write
// ignores.
func TestARequestWhoseContextEndsOnceItsFrameIsDeliveredKeepsTheConnection(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	spec := workerSpec(t)
	first := g.spawn(spec)
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	var armed atomic.Bool
	scripted := g.scriptedStartLauncherOver(spec.Claim, 1, func(shimwire.LauncherStart) *shimwire.LauncherStartResult { return nil },
		func(conn net.Conn) net.Conn {
			return &endsAfterWrite{Conn: conn, after: func() {
				if armed.CompareAndSwap(true, false) {
					cancel()
				}
			}}
		})
	g.eventually("the runtime to hold the scripted launcher's first state", func() bool {
		_, ready := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
		return ready
	})

	armed.Store(true)
	_, _, err := g.r.launchers.requestSession(ctx, scripted.session, shimwire.LauncherStart{ID: "start-2", Generation: 2}, "start-2")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the Start's request = %v, want its context's end while it waits for the answer", err)
	}
	if start := scripted.start(t, 2); start.Generation != 2 {
		t.Fatalf("Start = generation %d, want generation 2", start.Generation)
	}

	answer, _, err := g.r.launchers.requestSession(g.ctx, scripted.session, shimwire.LauncherStop{ID: "stop-2", Generation: 2}, "stop-2")
	if err != nil {
		t.Fatalf("the Stop after the delivered Start = %v, want it delivered on the same connection", err)
	}
	if result, ok := answer.(shimwire.LauncherStopResult); !ok || !result.OK {
		t.Fatalf("the Stop's answer = %+v, want OK", answer)
	}
	if stop := scripted.stop(t, 2); stop.Generation != 2 {
		t.Fatalf("Stop = generation %d, want generation 2", stop.Generation)
	}
}

// A request whose context is cancelled, with no deadline, while the launcher takes none of its frame
// ends with that cancel: the write is cut at once (writeFrame moves its deadline to the present),
// not held to apiTimeout, and the error is the context's end, which a caller telling its own bound
// from another reads (requestSession).
func TestARequestCancelledWhileItsFrameIsUntakenEndsWithTheCancel(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	spec := workerSpec(t)
	first := g.spawn(spec)
	// A launcher of the pod that reports its child and then reads nothing, in place of the rig's.
	g.r.launchers.mu.Lock()
	old := g.r.launchers.sessions[spec.Claim]
	g.r.launchers.mu.Unlock()
	if old != nil {
		old.close()
	}
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	session := g.r.launchers.accept(spec.Claim, launcherIssue(spec.Claim), g.keptSince(spec.Claim), shimwire.LauncherHello{
		LauncherID: "deaf-" + string(spec.Claim), PodUID: first.Sandbox.PodUID, ResumesBySession: g.resumesBySession,
	})
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	go func() {
		_ = shimwire.NewWriter(client).WriteFrame(shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
	}()
	g.eventually("the runtime to hold the deaf launcher's first state", func() bool {
		_, ready := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
		return ready
	})
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	_, _, err := g.r.launchers.requestSession(ctx, session, shimwire.LauncherStart{ID: "start-1", Generation: 1}, "start-1")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the request cancelled while the launcher took none of its frame = %v, want the cancel and the cut write's timeout", err)
	}
}

// endsAfterWrite is a connection whose Write runs after once the write has returned.
type endsAfterWrite struct {
	net.Conn
	after func()
}

func (c *endsAfterWrite) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.after()
	return n, err
}
