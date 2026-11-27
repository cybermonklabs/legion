package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A write Dispatch answers with an outage status - a 408 a proxy in front of it answered, a 429, a
// 5xx - is retried (callRetry), and the retry's answer is the call's: the first attempt said
// nothing of the write (RFC 9110 sections 15.5.9 and 15.6, RFC 6585 section 4). A 400 is
// Dispatch's answer and is not asked again.
func TestCallRetryAsksAgainAfterAnOutage(t *testing.T) {
	for _, tc := range []struct {
		first    int
		want     int
		attempts int32
	}{
		{http.StatusRequestTimeout, http.StatusCreated, 2},
		{http.StatusTooManyRequests, http.StatusCreated, 2},
		{http.StatusBadGateway, http.StatusCreated, 2},
		{http.StatusBadRequest, http.StatusBadRequest, 1},
	} {
		t.Run(fmt.Sprint(tc.first), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if attempts.Add(1) == 1 {
					w.WriteHeader(tc.first)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			res := newAPI(server.URL, "token", 1).callRetry(context.Background(), "POST", "/api/v1/issues", who{}, map[string]any{})
			if res.status != tc.want || attempts.Load() != tc.attempts {
				t.Fatalf("callRetry after a %d = %d in %d attempts, want %d in %d", tc.first, res.status, attempts.Load(), tc.want, tc.attempts)
			}
		})
	}
}

// TestOnlyAnAnswerThatIsNoOutageMovesAnAnchor drives anchored, the seed's anchored comment or ask,
// against a stand-in Dispatch that answers every try with the row's status and records the quote
// each try named. A 4xx that is no outage refuses the logged quote, and anchored tries the logged
// block's sentences in its place, at once. A 408 or a 429 that outlasted write's retries says
// nothing of the quote, so the seed stops on that answer, as it does on a 5xx and on a write its
// context ended, and names no other sentence. pause records write's waits instead of sleeping them.
func TestOnlyAnAnswerThatIsNoOutageMovesAnAnchor(t *testing.T) {
	const (
		loggedQuote = "The logged quote sits in this block."
		other       = "Another sentence fills the same block."
		block       = loggedQuote + " " + other
	)
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		canceled bool
		moves    bool
	}{
		{name: "400", status: http.StatusBadRequest, body: `{"code":"ANCHOR_MISSING","error":"quote not found"}`, moves: true},
		{name: "413", status: http.StatusRequestEntityTooLarge, body: `{"code":"REQUEST_TOO_LARGE","error":"too large"}`, moves: true},
		{name: "408", status: http.StatusRequestTimeout, body: `<html>408 Request Timeout</html>`},
		{name: "429", status: http.StatusTooManyRequests, body: `{"code":"RATE_LIMITED","error":"too many requests; try again in 1s"}`},
		{name: "503", status: http.StatusServiceUnavailable, body: `<html>503 Service Unavailable</html>`},
		{name: "a write its context ended", status: http.StatusCreated, body: `{"id":"c-local"}`, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				quotes []string
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Anchor struct {
						Quote string `json:"quote"`
					} `json:"anchor"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("the anchored write's body does not decode: %v", err)
				}
				mu.Lock()
				quotes = append(quotes, body.Anchor.Quote)
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var pauses []time.Duration
			s := &replaySeeder{
				b:       &replayBundle{State: replayState{BlockMaps: map[string]*replayBlockMap{"d1": {Blocks: []replayBlock{{ID: "p1", Text: block}}}}}},
				api:     newAPI(server.URL, "token", 1),
				lg:      log.New(io.Discard, "", 0),
				rngSeed: 1,
				pause:   func(d time.Duration) { pauses = append(pauses, d) },
				counts:  map[string]int{},
				marks:   map[string]int{},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			blockID := "p1"
			d := &issueDoc{a: &replayArtifact{ID: "d1", Kind: "doc"}, local: "local-d1", text: block + "\n"}
			_, err := s.anchored(ctx, "comment", "PRJ-1", d, "c1", &replayAnchor{ArtifactID: "d1", Quote: loggedQuote, BlockID: &blockID},
				who{}, "/api/v1/issues/PRJ-1/comments", map[string]any{"body": "x"})
			mu.Lock()
			defer mu.Unlock()
			var se *seedError
			switch {
			case tc.canceled:
				if !errors.Is(err, context.Canceled) || errors.As(err, &se) || len(quotes) != 0 {
					t.Fatalf("anchored under an ended context = %v after asking %q, want context.Canceled and no refusal, asking nothing", err, quotes)
				}
			case tc.moves:
				if err == nil || !strings.Contains(err.Error(), "every candidate was refused") || !errors.As(err, &se) || se.status != tc.status {
					t.Fatalf("anchored after a %d = %v, want every candidate refused with the %d", tc.status, err, tc.status)
				}
				if len(quotes) != 3 || quotes[0] != loggedQuote || !slices.Contains(quotes[1:], other) || len(pauses) != 0 {
					t.Fatalf("anchored after a %d asked %q and waited %v, want the logged quote and then the block's two sentences, at once", tc.status, quotes, pauses)
				}
			default:
				if !errors.As(err, &se) || se.status != tc.status || strings.Contains(err.Error(), "cannot be anchored") {
					t.Fatalf("anchored after a %d = %v, want that answer and no move to another sentence", tc.status, err)
				}
				if want := slices.Repeat([]string{loggedQuote}, 11); !slices.Equal(quotes, want) || len(pauses) != 10 {
					t.Fatalf("anchored after a %d asked %q and waited %d times, want the logged quote alone, 11 tries with 10 waits", tc.status, quotes, len(pauses))
				}
			}
		})
	}
}
