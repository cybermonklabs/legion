package testnats

import (
	"errors"
	"net"
	"net/url"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A closed gate, and a terminated server's URL, refuse every dial while the test binary starts
// child processes, as this package's parallel tests do when they run the binary again
// (testwait.RunChild and the rows that run a child of their own). A child forked while the address
// listens holds a copy of the listener until its exec closes it. So a close that leaves such a child
// holding the copy leaves the address listening, and a dial made just after the close is answered:
// it connects, or is reset when the child's exec drops the listener. TestAClosedGateKeepsItsPort and
// TestTerminateEndsTheServerAndGivesUpItsPort fail that way when a sibling test starts a child
// between their close and their dial. Each closer has a control row with no child started, where
// every dial is refused.
func TestAClosedAddressRefusesEveryDialWhileTheProcessStartsChildren(t *testing.T) {
	t.Parallel()
	for _, closer := range []struct {
		name   string
		rounds int
		// start readies the closer in one subtest and returns its round: close one listening
		// address and return it, with the func that makes it listen again where the closer can be
		// undone, or a fresh address in each round where it cannot.
		start func(t *testing.T) func() (address string, reopen func())
	}{
		{"a gate closed", 300, func(t *testing.T) func() (string, func()) {
			server, _ := Start(t)
			gate := StartGate(t, server)
			return func() (string, func()) {
				gate.Close()
				return gate.address, gate.Open
			}
		}},
		{"a server terminated", 40, func(t *testing.T) func() (string, func()) {
			return func() (string, func()) {
				server, uri := Start(t)
				server.Terminate(t)
				u, err := url.Parse(uri)
				if err != nil {
					t.Fatalf("parse %s: %v", uri, err)
				}
				return u.Host, func() {}
			}
		}},
	} {
		for _, children := range []bool{false, true} {
			name := closer.name + ", no child starts (the control)"
			if children {
				name = closer.name + ", children start beside it"
			}
			t.Run(name, func(t *testing.T) {
				closeOne := closer.start(t)
				stop := make(chan struct{})
				var starting sync.WaitGroup
				if children {
					for range 4 {
						starting.Add(1)
						go func() {
							defer starting.Done()
							for {
								select {
								case <-stop:
									return
								default:
								}
								_ = exec.Command("true").Run()
							}
						}()
					}
				}
				defer func() {
					close(stop)
					starting.Wait()
				}()
				answered := 0
				for round := range closer.rounds {
					address, reopen := closeOne()
					conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(Context(t), "tcp", address)
					switch {
					case err == nil, errors.Is(err, syscall.ECONNRESET):
						if err == nil {
							_ = conn.Close()
						}
						answered++
						// The child that held the listener drops it at its exec; let it go before
						// the address listens again.
						time.Sleep(100 * time.Millisecond)
					case !errors.Is(err, syscall.ECONNREFUSED):
						t.Fatalf("round %d: a dial of the closed address %s: %v, want connection refused", round, address, err)
					}
					reopen()
				}
				if answered > 0 {
					t.Errorf("%d of %d dials of a closed address were answered (connected or reset), want every one refused", answered, closer.rounds)
				}
			})
		}
	}
}
