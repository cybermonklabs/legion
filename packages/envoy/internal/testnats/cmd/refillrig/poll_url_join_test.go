package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/urljoin"
)

// TestAPollJoinsTheRouteOntoTheListenerURL: --url with a path and a query (path and query): each
// poll of a list asks the list's route joined onto that path with the query kept (urljoin.Join),
// the interests route's trailing slash kept. Where poll wrote the route after --url's text, the
// route went inside the query and the stand-in listener was asked /prefix. A plain origin (origin)
// is asked the route, as before. poll runs until it is signalled, as e2e-refill.sh stops it, so
// the row sends this process SIGINT once the stand-in has been asked.
func TestAPollJoinsTheRouteOntoTheListenerURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, prefix, query string }{
		{"path and query", "/prefix?key=k3y-made-up", "/prefix", "?key=k3y-made-up"},
		{"origin", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for list, route := range listPaths {
				var mu sync.Mutex
				var asked []string
				first := make(chan struct{}, 1)
				listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					asked = append(asked, r.Method+" "+r.URL.RequestURI())
					mu.Unlock()
					select {
					case first <- struct{}{}:
					default:
					}
					fmt.Fprint(w, "[]")
				}))
				given := listener.URL + c.suffix
				err := pollUntil(t, given, list, first, 10*time.Second)
				listener.Close()
				want := "GET " + c.prefix + route + c.query
				mu.Lock()
				if err != nil || len(asked) == 0 {
					t.Errorf("poll --url %s --list %s = %v, asked %q; want it to ask %s", given, list, err, asked, want)
				}
				for _, got := range asked {
					if got != want {
						t.Errorf("poll --url %s --list %s asked %s, want %s", given, list, got, want)
						break
					}
				}
				mu.Unlock()
			}
		})
	}
}

// TestAPollURLWithNoHostIsRefusedQuotingNoneOfIt: a --url that is no absolute URL with a host is
// refused as urljoin.ErrBase before any poll, in words that quote none of it, since the URL can
// carry a credential. Where poll wrote the route after --url's text, it polled on, writing
// net/http's error for the URL, which quotes it whole, as each poll's answer.
func TestAPollURLWithNoHostIsRefusedQuotingNoneOfIt(t *testing.T) {
	err := pollUntil(t, "127.0.0.1:9?key=k3y-made-up", "sessions", nil, 2*time.Second)
	if !errors.Is(err, urljoin.ErrBase) || strings.Contains(err.Error(), "k3y-made-up") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("poll of a --url with no scheme = %v; want urljoin.ErrBase, quoting none of the URL", err)
	}
}

// pollUntil runs refillrig poll of list at given, writing its observations in a directory of the
// test's, and signals it (SIGINT, which it stops on) once asked is sent on, or once wait has passed
// without it returning. The test holds SIGINT too while poll runs, so a signal that reaches the
// process after poll stopped holding it ends nothing.
func pollUntil(t *testing.T, given, list string, asked <-chan struct{}, wait time.Duration) error {
	t.Helper()
	held := make(chan os.Signal, 1)
	signal.Notify(held, syscall.SIGINT)
	defer signal.Stop(held)
	out := filepath.Join(t.TempDir(), "polls.jsonl")
	done := make(chan error, 1)
	go func() {
		done <- runPoll([]string{"--url", given, "--list", list, "--out", out, "--interval", "50ms"})
	}()
	select {
	case err := <-done:
		return err
	case <-asked:
		t.Logf("the stand-in was asked; signalling poll")
	case <-time.After(wait):
		t.Errorf("poll --url %s ran %s without returning; signalling it", given, wait)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal this process: %v", err)
	}
	return <-done
}
