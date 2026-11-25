package api

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestSocketsOpeningAsDispatchShutsDownCloseWithServiceRestart: http.Server.Shutdown counts no
// hijacked connection, so a page's GET /ws/live upgrade can complete while Dispatch shuts down -
// after the shutdown's wait for every socket (waitBackground) has begun, and even after it has
// returned. Each such socket closes with 1012, as a restart closes an open one, and none joins the
// sockets that wait counts once it has begun: their count is a sync.WaitGroup, whose count may not
// rise from zero beside its Wait, and a socket counted after the wait returned would serve on while
// the database pool closes.
//
// The wait begins with sockets serving, as a deploy's does. Nothing the test does orders their
// handlers' count of them before that wait, so only the set's own lock can: without it the race
// detector reports the wait against the first socket's count. Pages open sockets throughout the
// shutdown, and each opens one more once the wait has returned, which it keeps open without
// answering its close: a server that counted that socket would hold it in its set for
// liveSocketCloseWait more, waiting for the answer. That wait runs on the sockets' read-deadline
// clock, a fake no one moves here (liveSocketClock), so such a socket stays in the set until its
// page lets it go, and the set read before then finds it there however late it comes.
//
// The pages dial and read on goroutines of their own, under a context the test's cleanup cancels:
// that closes every connection a page opened, the one a dial still waits on and the ones it has
// collected, and the cleanup returns once every page has, so a test that fails while a page waits
// leaves none behind.
func TestSocketsOpeningAsDispatchShutsDownCloseWithServiceRestart(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{liveSocketClock: clocktest.Held()}, nil)

	const pages = 16
	serving := make([]*socketClient, pages)
	for i := range serving {
		serving[i] = rig.dial(t, asUser("alice"))
	}

	type pageRun struct {
		opened   int
		failures []error
		// last is the socket the page opened once the wait had returned, still open.
		last *gws.Conn
	}
	start, waited := make(chan struct{}), make(chan struct{})
	// waitReturned tells the pages the wait has returned; a test that fails first tells them too, so
	// none dials on past its end.
	waitReturned := sync.OnceFunc(func() { close(waited) })
	defer waitReturned()
	pagesCtx, cancelPages := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancelPages()
		workers.Wait()
	})
	dialed := make(chan struct{}, pages)
	runs := make(chan pageRun, pages)
	for range pages {
		workers.Go(func() {
			var run pageRun
			defer func() { runs <- run }()
			select {
			case <-start:
			case <-pagesCtx.Done():
				return
			}
			for last := false; !last && pagesCtx.Err() == nil; {
				select {
				case <-waited:
					last = true
				default:
				}
				conn, release, err := dialLiveSocketAsAlice(pagesCtx, rig.url)
				run.opened++
				if run.opened == 1 {
					dialed <- struct{}{}
				}
				if err != nil {
					run.failures = append(run.failures, err)
					continue
				}
				if last {
					conn.SetCloseHandler(func(int, string) error { return nil })
					run.last = conn
				}
				if err := endsWithServiceRestart(conn); err != nil {
					run.failures = append(run.failures, err)
				}
				if !last {
					release()
				}
			}
		})
	}

	returned := make(chan struct{})
	go func() {
		rig.served.liveSockets.wait()
		close(returned)
	}()
	close(start)
	// Every page has opened a socket beside the wait before the signal closes the ones it serves.
	for range pages {
		testwait.Receive(t, "each page's first dial beside the shutdown's wait", dialed)
	}
	rig.stop()
	testwait.Receive(t, "the shutdown's wait for the sockets to return after the signal", returned)
	waitReturned()

	opened, last := 0, make([]*gws.Conn, 0, pages)
	for range pages {
		run := testwait.Receive(t, "each page's dials to end, every socket it opened ended by Dispatch", runs)
		opened += run.opened
		if run.last != nil {
			last = append(last, run.last)
		}
		for _, err := range run.failures {
			t.Error(err)
		}
	}
	t.Logf("%d sockets served at the signal; %d opened while Dispatch shut down, %d of them after its wait returned", pages, opened, len(last))
	if len(last) != pages {
		t.Fatalf("%d pages opened a socket after the wait returned, want every one (%d)", len(last), pages)
	}
	// Every socket the wait counted had ended when it returned, and it counted none of those opened
	// after, which are still open.
	if open := rig.served.liveSockets.all(); len(open) != 0 {
		t.Fatalf("%d sockets in the server's set after its wait returned", len(open))
	}
	for i, client := range serving {
		if code, _ := client.closeCode(t); code != gws.CloseServiceRestart {
			t.Errorf("socket %d, open at the signal, closed with %d, want 1012", i, code)
		}
	}
}

// dialLiveSocketAsAlice opens the live socket at url as alice, on a page's own goroutine, so it
// calls no t method, and answers the socket with release, which closes it. The handshake has no
// bound of its own (HandshakeTimeout 0, where gorilla's DefaultDialer gives it 45 s): the test's
// wait for the page's run bounds it, by the test binary's deadline. ctx ends the connect, and
// until release its end closes the connection, which ends a handshake still waiting for the
// upgrade's answer, as gorilla does not, and a read of the socket afterwards. release also drops
// that hold on ctx, so a page that opens many sockets keeps none of them past its own close.
func dialLiveSocketAsAlice(ctx context.Context, url string) (*gws.Conn, func(), error) {
	dialer := *gws.DefaultDialer
	dialer.HandshakeTimeout = 0
	stop := func() bool { return false }
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
		return conn, nil
	}
	conn, response, err := dialer.DialContext(ctx, url, asUser("alice"))
	if err != nil {
		stop()
		status := "no response"
		if response != nil {
			status = response.Status
		}
		return nil, nil, fmt.Errorf("open %s: %v (%s)", url, err, status)
	}
	return conn, func() {
		stop()
		_ = conn.Close()
	}, nil
}

// endsWithServiceRestart reads conn until Dispatch ends it, answering nil for a close with 1012
// (service restart) and an error for any other end. It sets no read deadline of its own: the
// test's wait for the page's run (testwait.Receive) bounds it by the test binary's deadline, and
// the page's context ending closes conn (dialLiveSocketAsAlice).
func endsWithServiceRestart(conn *gws.Conn) error {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if gws.IsCloseError(err, gws.CloseServiceRestart) {
				return nil
			}
			return fmt.Errorf("a socket opened as Dispatch shut down ended with %v, want 1012", err)
		}
	}
}
