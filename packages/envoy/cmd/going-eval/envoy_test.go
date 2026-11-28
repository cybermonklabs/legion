package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/eval"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
)

// TestATryRunsOnTheSeedsClock: the seed writes its times for one day, its now, and the judge reads
// an answer as of that day, so a try runs on that clock too. Its server's agent is told the seed's
// now as the time now, under the server's test hooks (seedClock); and the fake Envoy answers each
// session last seen when the seed says, a live one too, on the seed's day at or before its now,
// never at the time of the read, which the agent would read as days after now. A gone session is
// answered by neither the roster nor a lookup.
func TestATryRunsOnTheSeedsClock(t *testing.T) {
	seed, err := eval.LoadSeed(eval.Files)
	if err != nil {
		t.Fatal(err)
	}
	clock := seedClock(seed.Now)
	if want := "DISPATCH_TEST_AGENT_NOW=" + seed.Now.UTC().Format(time.RFC3339Nano); !slices.Contains(clock, "DISPATCH_TEST_HOOKS=1") || !slices.Contains(clock, want) {
		t.Errorf("the try's server is started with %v, want DISPATCH_TEST_HOOKS=1 and %s", clock, want)
	}

	envoy, err := startFakeEnvoy(seed.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(envoy.close)
	read := func(t *testing.T, path string, into any) int {
		t.Helper()
		response, err := http.Get(envoy.url() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if err := json.NewDecoder(response.Body).Decode(into); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return response.StatusCode
	}
	var roster []dispatchenvoy.Session
	if status := read(t, "/v1/sessions?include=unresponsive", &roster); status != http.StatusOK {
		t.Fatalf("GET /v1/sessions: %d", status)
	}
	answered := map[string]int64{}
	for _, session := range roster {
		answered[session.SessionID] = session.LastSeen
	}
	live := 0
	for _, session := range seed.Sessions {
		seen, listed := answered[session.ID]
		if session.State == dispatchenvoy.StateGone {
			var lookup map[string]any
			if listed || read(t, "/v1/registry/"+session.ID+"?include=unresponsive", &lookup) != http.StatusNotFound {
				t.Errorf("the gone session %s is answered, want it on neither the roster nor a lookup", session.ID)
			}
			continue
		}
		if session.State == dispatchenvoy.StateLive {
			live++
		}
		at := time.UnixMilli(seen).UTC()
		if !listed || seen != session.LastSeen.UnixMilli() || at.After(seed.Now) || seed.Now.Sub(at) >= 24*time.Hour {
			t.Errorf("the roster answers the %s session %s last seen at %s (listed %t), want the seed's %s, on the seed's day at or before its now %s",
				session.State, session.ID, at.Format(time.RFC3339), listed, session.LastSeen.Format(time.RFC3339), seed.Now.Format(time.RFC3339))
		}
		var lookup dispatchenvoy.Session
		if status := read(t, "/v1/registry/"+session.ID+"?include=unresponsive", &lookup); status != http.StatusOK || lookup.LastSeen != session.LastSeen.UnixMilli() {
			t.Errorf("a lookup of %s answers %d last seen at %d, want 200 and the seed's %d", session.ID, status, lookup.LastSeen, session.LastSeen.UnixMilli())
		}
	}
	if live == 0 {
		t.Fatal("the seed holds no live session, so this test holds no live session's last seen to the seed's clock")
	}
}
