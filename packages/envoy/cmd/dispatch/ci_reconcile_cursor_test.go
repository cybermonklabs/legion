package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// ciReconcileCursorHeads is how many open pull requests the cursor stand-in holds. Ten heads
	// are twenty head reads, and at the fastest gap a pass may leave between two requests
	// (cireconcile.FastestRequestPace, 100ms) twenty reads take two seconds, so no pass covers
	// them within the one-second interval this test runs the loop on: every pass ends on its own
	// bound, and the passes take the order up from one another.
	ciReconcileCursorHeads = 10

	// ciReconcileRecordedHead is the head whose completed check run the pass has to record: the
	// last of the order, which no first pass reaches. The record it changes is the evidence that
	// a head sitting past where a pass stopped is not starved but read within the cycle.
	ciReconcileRecordedHead = 10

	// ciReconcileLimitedHead is the head GitHub answers a rate limit partway through: its check
	// runs are given, its check suites are refused, so the head was not read whole and the pass
	// after the limit's reset must read it again rather than carry on past it.
	ciReconcileLimitedHead = 6
)

// The cursor is a loop of the real envoy-dispatch binary, so this proves it there: the actual
// process against a loopback GitHub holding more heads than one bounded pass can reach, and the
// process's own log as the evidence. Every pass says the first and the last head it read; each
// pass begins at the head after the one the pass before it finished, and at the first head of
// the order again once a cycle is covered; the completed check run sitting at the last head of
// the order -- past where the first pass stopped -- is recorded within that one cycle; and a
// rate limit GitHub answers partway through a head's reads leaves the pass after its reset
// reading that same head, not the head after it.
//
// The stand-in refuses the pass's GraphQL query as GitHub refuses one for an App that may not
// read a repository's Checks that way, so what the process walks here is the REST fallback:
// the repository's open pull requests listed page by page and each head read with two requests
// of its own. That is the read a cursor is needed for, and the one this test bounds.
func TestTheCIReconcileCursorResumesInTheDispatchProcess(t *testing.T) {
	database := storetest.Open(t)
	mapCIReconcileRepository(t, database)
	_, natsURL := testnats.Start(t)
	makeCIStateBucket(t, natsURL)
	github := startCIReconcileCursorGitHub(t)
	_, pemText := githubapptest.Key(t)

	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL,
		Env: map[string]string{
			"DISPATCH_APP_CLIENT_ID":   "Iv1.cireconcile",
			"DISPATCH_APP_PEM_B64":     pemText,
			"DISPATCH_GITHUB_API_BASE": github.server.URL,
			"DISPATCH_REVIEWER_APP_ID": ciReconcileReviewerApp,
			// The pass's bound as well as the loop's period: five minutes is longer than any
			// test may wait for a pass.
			"DISPATCH_TEST_CI_RECONCILE_INTERVAL": "1s",
		},
	})
	process.waitHealthy(t)

	// The passes until the first that covers the whole order, which is the first cycle.
	cycle := waitForCIReconcilePasses(t, process, "a cycle of bounded passes covering every head",
		func(passes []map[string]string) bool {
			return cycleCompleted(passes) >= 0
		})
	cycle = cycle[:cycleCompleted(cycle)+1]
	if len(cycle) < 2 {
		t.Fatalf("one pass covered all %d heads, which is not the cycle this test is about: %v", ciReconcileCursorHeads, cycle)
	}

	// Every pass says where in the order it read, and the first of them ended on its own bound
	// rather than covering the order.
	for i, pass := range cycle {
		if _, named := pass["first_head"]; !named {
			t.Fatalf("pass %d logged no first_head: %v", i, pass)
		}
		if _, named := pass["last_head"]; !named {
			t.Fatalf("pass %d logged no last_head: %v", i, pass)
		}
	}
	if cycle[0]["deadline_reached"] != "true" {
		t.Errorf("the first pass reports deadline_reached = %q, want true: ten heads do not fit one second: %v",
			cycle[0]["deadline_reached"], cycle[0])
	}
	if cycle[0]["last_head"] == ciReconcileHeadName(ciReconcileRecordedHead) {
		t.Fatalf("the first pass reached the last head of the order, so nothing about this test's starvation is proved: %v", cycle[0])
	}
	requireCIReconcileCursorOrder(t, cycle)

	// The check run at the last head of the order, which the first pass never reached, is
	// recorded within the cycle: no head starves behind the heads the order holds before it.
	if cycle[0]["records"] != "0" {
		t.Errorf("the first pass changed %s records, want none: the one run to record sits at the last head of the order: %v",
			cycle[0]["records"], cycle[0])
	}
	recorded := false
	for _, pass := range cycle {
		if pass["records"] == "1" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("no pass of the cycle recorded the run at head %d:\n%s", ciReconcileRecordedHead, process.Output.String())
	}

	// A rate limit partway through one head's reads: GitHub gives that head's check runs and
	// refuses its check suites, so the head was not read whole.
	github.rateLimitNext(ciReconcileCursorSuitesPath(ciReconcileLimitedHead), 2*time.Second)
	before := len(ciReconcilePasses(process.Output.String()))
	after := waitForCIReconcilePasses(t, process, "a pass GitHub's rate limit stopped",
		func(passes []map[string]string) bool {
			for _, pass := range passes[min(before, len(passes)):] {
				if pass["rate_limited"] == "true" {
					return true
				}
			}
			return false
		})
	stopped := before
	for stopped < len(after) && after[stopped]["rate_limited"] != "true" {
		stopped++
	}
	// The limited head was not read whole, so the pass the limit stopped read up to the head
	// before it, and read no head at all where that pass began at the limited head itself --
	// which of the two it is depends on where the pass before it left the cursor, and that is
	// how many heads a one-second pass got through. Both say the one thing this is about;
	// `last_head` naming the limited head, or a head past it, would be the pass counting a
	// head whose second listing it never read.
	if last := after[stopped]["last_head"]; last != "" && ciReconcileHeadNumber(t, last) >= ciReconcileLimitedHead {
		t.Errorf("the pass the limit stopped read up to %q, want no head at or past %q: the limited head was not read whole: %v",
			last, ciReconcileHeadName(ciReconcileLimitedHead), after[stopped])
	}

	// Once GitHub's reset has passed, the pass that reads again starts at the head the limit cut
	// short, not at the head after it.
	resumed := waitForCIReconcilePasses(t, process, "a pass reading again after the limit's reset",
		func(passes []map[string]string) bool {
			for _, pass := range passes[min(stopped+1, len(passes)):] {
				if pass["first_head"] != "" {
					return true
				}
			}
			return false
		})
	for _, pass := range resumed[stopped+1:] {
		if pass["first_head"] == "" {
			continue
		}
		if want := ciReconcileHeadName(ciReconcileLimitedHead); pass["first_head"] != want {
			t.Fatalf("the pass after the limit's reset began at %q, want %q, the head the limit cut short: %v",
				pass["first_head"], want, pass)
		}
		break
	}
}

// requireCIReconcileCursorOrder requires that each pass began at the head after the one the pass
// before it finished, and at the first head of the order again after a pass that covered the
// cycle. A pass that finished no head -- one a limit stopped before it read one whole -- moves
// the cursor nowhere, so the pass after it is held to the same rule.
func requireCIReconcileCursorOrder(t *testing.T, passes []map[string]string) {
	t.Helper()
	finished, covered := "", false
	for i, pass := range passes {
		first := pass["first_head"]
		if first == "" {
			if pass["cycle_complete"] == "true" {
				finished, covered = "", true
			}
			continue
		}
		if finished != "" || covered {
			want := ciReconcileHeadName(1)
			if !covered {
				want = ciReconcileHeadName(ciReconcileHeadNumber(t, finished)%ciReconcileCursorHeads + 1)
			}
			if first != want {
				t.Errorf("pass %d began at %q, want %q: a pass takes the order up where the pass before it left it: %v",
					i, first, want, pass)
			}
		}
		finished, covered = pass["last_head"], pass["cycle_complete"] == "true"
	}
}

// cycleCompleted is the index of the first pass that covered the whole order, and -1 while no
// pass has.
func cycleCompleted(passes []map[string]string) int {
	for i, pass := range passes {
		if pass["cycle_complete"] == "true" {
			return i
		}
	}
	return -1
}

// ciReconcilePasses is every "ci reconcile pass" line the process has logged, oldest first, each
// as the fields of that line. The process logs in logfmt, and no value of this line holds a
// space, so a field is what is left and right of the first `=` of each word.
func ciReconcilePasses(output string) []map[string]string {
	var passes []map[string]string
	for line := range strings.Lines(output) {
		if !strings.Contains(line, `msg="ci reconcile pass"`) {
			continue
		}
		fields := map[string]string{}
		for _, word := range strings.Fields(line) {
			key, value, ok := strings.Cut(word, "=")
			if !ok {
				continue
			}
			fields[key] = strings.Trim(value, `"`)
		}
		passes = append(passes, fields)
	}
	return passes
}

// waitForCIReconcilePasses waits up to a minute for the pass lines the process has logged to
// satisfy enough, and answers them. The passes run a second apart, so a cycle of them is
// several seconds of waiting.
func waitForCIReconcilePasses(t *testing.T, process *dispatchProcess, what string, enough func([]map[string]string) bool) []map[string]string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		passes := ciReconcilePasses(process.Output.String())
		if enough(passes) {
			return passes
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process never logged %s:\n%s", what, process.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ciReconcileHeadName is how the pass line names one head of the stand-in: the repository GitHub
// itself names, and the pull request's number.
func ciReconcileHeadName(number int) string {
	return ciReconcileRepository + "#" + strconv.Itoa(number)
}

// ciReconcileHeadNumber is the pull request number of a head the pass line named.
func ciReconcileHeadNumber(t *testing.T, name string) int {
	t.Helper()
	_, text, ok := strings.Cut(name, "#")
	if !ok {
		t.Fatalf("the pass line named a head %q, which is no owner/repo#number", name)
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		t.Fatalf("the pass line named head %q, whose number does not read: %v", name, err)
	}
	return number
}

// ciReconcileCursorSHA is the head commit of one of the stand-in's pull requests: its number in
// the forty hex characters a commit has.
func ciReconcileCursorSHA(number int) string {
	return fmt.Sprintf("%040x", number)
}

// ciReconcileCursorSuitesPath is where the stand-in lists one head's check suites, the second of
// the two reads a head takes.
func ciReconcileCursorSuitesPath(number int) string {
	return "/repos/" + ciReconcileRepository + "/commits/" + ciReconcileCursorSHA(number) + "/check-suites"
}

// cursorGitHub is the loopback GitHub the cursor test's process calls: the installation lookup
// and the token mint, the repository's identity, its ten open pull requests and each head's two
// listings, and the one-shot rate limit a test arms on one of those reads.
type cursorGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// limits is the reset of the rate limit the next request to each armed path is answered
	// with, and the path is disarmed once it has been answered so.
	limits map[string]time.Duration
}

// rateLimitNext answers the next request to path as GitHub answers an exhausted limit, naming a
// reset that long away. Every Dispatch process reads the limit the client records out of the
// ledger they share until that reset, so the pass that met it, and every pass until the reset,
// stops where it stands.
func (g *cursorGitHub) rateLimitNext(path string, reset time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits[path] = reset
}

// limited answers one request as GitHub's own exhausted limit when a test armed its path, and
// reports whether it did.
func (g *cursorGitHub) limited(w http.ResponseWriter, r *http.Request) bool {
	g.mu.Lock()
	reset, armed := g.limits[r.URL.Path]
	delete(g.limits, r.URL.Path)
	g.mu.Unlock()
	if !armed {
		return false
	}
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(reset).Unix(), 10))
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	return true
}

// startCIReconcileCursorGitHub is that stand-in, serving on the loopback so no call leaves this
// machine. Every other path answers GitHub's 404, as GitHub answers a path the App cannot see,
// and the webhook redelivery sweep's own listing answers empty.
func startCIReconcileCursorGitHub(t *testing.T) *cursorGitHub {
	t.Helper()
	github := &cursorGitHub{t: t, limits: map[string]time.Duration{}}
	write := func(w http.ResponseWriter, status int, payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("encode the stand-in's answer: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{
			"id":          1,
			"app_slug":    "cireconcile",
			"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusCreated, map[string]any{
			"token":      "ghs_cireconcile",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		// This App may not read the repository's Checks through GraphQL, which GitHub answers
		// with a null repository and a FORBIDDEN entry rather than with an HTTP error. The
		// pass then reads that repository's open pull requests and every head of it over REST,
		// two requests a head, which is the read this test's cursor walks: a pass every query
		// of which answered would cover ten heads in one query and leave no cursor to prove.
		write(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"rateLimit":  map[string]any{"cost": 1, "remaining": 4999, "resetAt": time.Now().Add(time.Hour).Format(time.RFC3339)},
				"repository": nil,
			},
			"errors": []any{map[string]any{
				"type":    "FORBIDDEN",
				"path":    []any{"repository"},
				"message": "Resource not accessible by integration",
			}},
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"full_name": ciReconcileRepository, "id": 4242})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		// GitHub lists them newest first, which is not the order the pass reads them in: the
		// pass sorts a repository's heads by pull request number, so its cursor is a number in
		// an order that does not move between passes.
		pulls := make([]map[string]any, 0, ciReconcileCursorHeads)
		for number := ciReconcileCursorHeads; number >= 1; number-- {
			pulls = append(pulls, map[string]any{
				"number": number,
				"state":  "open",
				"head":   map[string]any{"sha": ciReconcileCursorSHA(number)},
			})
		}
		write(w, http.StatusOK, pulls)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if github.limited(w, r) {
			return
		}
		if r.PathValue("sha") != ciReconcileCursorSHA(ciReconcileRecordedHead) {
			write(w, http.StatusOK, map[string]any{"total_count": 0, "check_runs": []map[string]any{}})
			return
		}
		// The one completed run of the whole stand-in, at the last head of the order.
		write(w, http.StatusOK, map[string]any{"total_count": 1, "check_runs": []map[string]any{
			{
				"id": 11, "name": "build", "head_sha": ciReconcileCursorSHA(ciReconcileRecordedHead),
				"status": "completed", "conclusion": "success",
				"completed_at": "2026-01-02T03:04:05Z",
				"app":          map[string]any{"id": 777},
			},
		}})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-suites", func(w http.ResponseWriter, r *http.Request) {
		if github.limited(w, r) {
			return
		}
		write(w, http.StatusOK, map[string]any{"total_count": 0, "check_suites": []map[string]any{}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/hook/deliveries" {
			write(w, http.StatusOK, []any{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	github.server = githubtest.Serve(t, githubtest.Spec{Handle: mux.ServeHTTP})
	return github
}
