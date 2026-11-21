package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// TestTheProjectAccessClientReusesItsConnectionsRoundAfterRound: the client the broker reads
// Dispatch's project access with (projectAccessHTTPClient) asks requests.MaySeeAtOnce questions at
// once, the most one row or one list asks, twice over, and the second round opens no connection:
// each of the first round's is kept and taken again. A stand-in Dispatch holds every answer until
// the whole round has arrived, so each round is that many questions at once. Go's default client
// keeps two, so its second round opens all but two again, which the control row shows.
func TestTheProjectAccessClientReusesItsConnectionsRoundAfterRound(t *testing.T) {
	for _, c := range []struct {
		name   string
		client *http.Client
		opened int
	}{
		{"the broker's client", projectAccessHTTPClient(), 0},
		{"Go's default transport, the control", &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}, requests.MaySeeAtOnce - 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			var opened atomic.Int32
			arrived := make(chan struct{})
			var mu sync.Mutex
			release := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				wait := release
				mu.Unlock()
				arrived <- struct{}{}
				<-wait
				_, _ = io.WriteString(w, `{}`)
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					opened.Add(1)
				}
			}
			server.Start()
			t.Cleanup(server.Close)
			t.Cleanup(c.client.CloseIdleConnections)

			round := func() int {
				before := opened.Load()
				var wg sync.WaitGroup
				for range requests.MaySeeAtOnce {
					wg.Go(func() {
						res, err := c.client.Get(server.URL)
						if err != nil {
							t.Errorf("GET: %v", err)
							return
						}
						_, _ = io.Copy(io.Discard, res.Body)
						_ = res.Body.Close()
					})
				}
				for range requests.MaySeeAtOnce {
					<-arrived
				}
				mu.Lock()
				close(release)
				release = make(chan struct{})
				mu.Unlock()
				wg.Wait()
				return int(opened.Load() - before)
			}
			if first := round(); first != requests.MaySeeAtOnce {
				t.Fatalf("the first round opened %d connections; want %d, one for each question at once", first, requests.MaySeeAtOnce)
			}
			if second := round(); second != c.opened {
				t.Errorf("the second round opened %d connections; want %d", second, c.opened)
			}
		})
	}
}
