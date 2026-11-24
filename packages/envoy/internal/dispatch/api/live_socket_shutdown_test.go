package api

import (
	"fmt"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
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
// liveSocketCloseWait more, waiting for the answer, so the set read before the page lets it go
// finds it there.
func TestSocketsOpeningAsDispatchShutsDownCloseWithServiceRestart(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)

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
	dialed := make(chan struct{}, pages)
	runs := make(chan pageRun, pages)
	for range pages {
		go func() {
			var run pageRun
			<-start
			for last := false; !last; {
				select {
				case <-waited:
					last = true
				default:
				}
				conn, err := dialLiveSocketAsAlice(rig.url)
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
					_ = conn.Close()
				}
			}
			runs <- run
		}()
	}

	returned := make(chan struct{})
	go func() {
		rig.served.liveSockets.wait()
		close(returned)
	}()
	close(start)
	// Every page has opened a socket beside the wait before the signal closes the ones it serves.
	for range pages {
		<-dialed
	}
	rig.stop()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown's wait for the sockets had not returned 10s after the signal")
	}
	waitReturned()

	opened, last := 0, make([]*gws.Conn, 0, pages)
	for range pages {
		run := <-runs
		opened += run.opened
		if run.last != nil {
			last = append(last, run.last)
		}
		for _, err := range run.failures {
			t.Error(err)
		}
	}
	defer func() {
		for _, conn := range last {
			_ = conn.Close()
		}
	}()
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
		if code, _ := client.closeCode(t, 5*time.Second); code != gws.CloseServiceRestart {
			t.Errorf("socket %d, open at the signal, closed with %d, want 1012", i, code)
		}
	}
}

// dialLiveSocketAsAlice opens the live socket at url as alice.
func dialLiveSocketAsAlice(url string) (*gws.Conn, error) {
	conn, response, err := gws.DefaultDialer.Dial(url, asUser("alice"))
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		return nil, fmt.Errorf("open %s: %v (%s)", url, err, status)
	}
	return conn, nil
}

// endsWithServiceRestart reads conn until Dispatch ends it, answering nil for a close with 1012
// (service restart) and an error for any other end.
func endsWithServiceRestart(conn *gws.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if gws.IsCloseError(err, gws.CloseServiceRestart) {
				return nil
			}
			return fmt.Errorf("a socket opened as Dispatch shut down ended with %v, want 1012", err)
		}
	}
}
