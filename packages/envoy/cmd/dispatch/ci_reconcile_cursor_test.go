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

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
	github := startCIReconcileCursorGitHub(t)
	process, natsURL := startCIReconcileCursorProcess(t, github)

	// The passes until the first that covers the whole order, which is the first cycle, and the
	// first of them that began reading GitHub (readingCIReconcilePass): a pass before it whose own
	// bound fell before it read anything -- a loaded machine's first second -- read no head and
	// moved no cursor, so the cycle's reading begins with the pass after it.
	cycle := waitForCIReconcilePasses(t, process, "a cycle of bounded passes covering every head",
		func(passes []map[string]string) bool {
			return cycleCompleted(passes) >= 0
		})
	cycle = cycle[:cycleCompleted(cycle)+1]
	first := readingCIReconcilePass(cycle, 0)
	if first < 0 {
		t.Fatalf("no pass of the cycle began reading GitHub (every pass line counts requests=0), yet the last of them covered all %d heads: %v", ciReconcileCursorHeads, cycle)
	}
	if first == len(cycle)-1 {
		t.Fatalf("one pass covered all %d heads, which is not the cycle this test is about: %v", ciReconcileCursorHeads, cycle)
	}

	// Every pass says where in the order it read, and the first that began reading ended on its
	// own bound rather than covering the order.
	for i, pass := range cycle {
		if _, named := pass["first_head"]; !named {
			t.Fatalf("pass %d logged no first_head: %v", i, pass)
		}
		if _, named := pass["last_head"]; !named {
			t.Fatalf("pass %d logged no last_head: %v", i, pass)
		}
	}
	if cycle[first]["deadline_reached"] != "true" {
		t.Errorf("the first pass that began reading GitHub reports deadline_reached = %q, want true: ten heads do not fit one second: %v",
			cycle[first]["deadline_reached"], cycle[first])
	}
	if cycle[first]["last_head"] == ciReconcileHeadName(ciReconcileRecordedHead) {
		t.Fatalf("the first pass that began reading GitHub reached the last head of the order, so nothing about this test's starvation is proved: %v", cycle[first])
	}
	requireCIReconcileCursorOrder(t, cycle)

	// The check run at the last head of the order, which the first pass that began reading never
	// reached, is in that head's record once the cycle has closed: no head starves behind the heads
	// the order holds before it. The record is read from the CI bucket itself, not from the
	// passes' records count: a write the pass's own bound cut off can land all the same, and the
	// pass that reads the head again then changes nothing (recordHead), so no pass line need count
	// that record.
	if cycle[first]["records"] != "0" {
		t.Errorf("the first pass that began reading GitHub changed %s records, want none: the one run to record sits at the last head of the order: %v",
			cycle[first]["records"], cycle[first])
	}
	requireCIReconcileRecordedRun(t, natsURL, process)

	// A rate limit partway through one head's reads: GitHub gives that head's check runs and
	// refuses its check suites, so the head was not read whole. The passes are counted before the
	// limit is armed: passes run back to back, so one can meet the limit the moment it is armed,
	// and a count taken after that would hold its line and leave the wait below a pass that never
	// comes once the reset has passed.
	before := len(ciReconcilePasses(process.Output.String()))
	github.rateLimitNext(ciReconcileCursorSuitesPath(ciReconcileLimitedHead), 2*time.Second)
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

// A pass of the process that lists every head and reads none whole -- GitHub slower than the
// one-second bound leaves, as on a loaded runner -- leaves the passes after it reading the order
// all the same. That pass learned no page of any head, so the gap the work sets would be the
// 500ms ceiling, and at 500ms a one-second pass sends three requests: the listing and the first
// of the two reads a head takes. Every pass after it would then end on its bound with no head
// read and the cursor where it was, and no cycle would ever close. The process paces the pass
// after such a pass at the floor instead, which reads heads whole, learns their pages and moves
// the cursor on.
//
// GitHub holds the first head's check suites, the second of its two reads, until the bound of
// the pass that asks for them gives the request up, so that pass lists the ten heads and reads
// none of them whole however fast the machine runs. The row reads the first pass after that one
// that began reading GitHub at all (readingCIReconcilePass), whichever passes of the process they
// are: in the second case GitHub also holds the process's first query until the first pass's
// bound gives it up, so the first pass lists nothing and the starved pass is never the first, as a
// machine too slow to start a pass within its second leaves it. A pass between the two whose own
// bound fell before it began reading -- before or inside its read of the repositories it covers,
// which a loaded machine can take a second over -- set no pace, and its line says 0s; the process
// keeps the floor across it, since its own bound ended it too (cireconcile.Reconciler.Pass), so
// the pass that begins reading next is the one whose pace the row is about.
func TestTheCIReconcileCursorResumesInTheDispatchProcessAfterAPassThatReadNoHead(t *testing.T) {
	for _, row := range []struct {
		name string
		// firstQuery holds the process's first GraphQL query until its pass's bound gives it up.
		firstQuery bool
	}{
		{name: "GitHub answers the first pass's query"},
		{name: "GitHub holds the first pass's query", firstQuery: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			github := startCIReconcileCursorGitHub(t)
			github.holdFirst(ciReconcileCursorSuitesPath(1))
			if row.firstQuery {
				github.holdFirst(ciReconcileCursorQueryPath)
			}
			process, _ := startCIReconcileCursorProcess(t, github)

			passes := waitForCIReconcilePasses(t, process, "a pass that listed every head and reached none, and a pass after it that began reading GitHub",
				func(passes []map[string]string) bool {
					starved := starvedCIReconcilePass(passes)
					return starved >= 0 && readingCIReconcilePass(passes, starved+1) >= 0
				})
			starved := starvedCIReconcilePass(passes)
			if row.firstQuery && starved == 0 {
				t.Fatalf("the first pass listed every head although GitHub held its query: nothing here is tested: %v", passes[0])
			}
			next := readingCIReconcilePass(passes, starved+1)
			if got, want := passes[next]["pace"], "100ms"; got != want {
				t.Fatalf("the pass after one that read no head whole paced itself at %s, want the floor %s: at the ceiling a one-second pass reads no head: starved %v, the next pass that began reading GitHub %v",
					got, want, passes[starved], passes[next])
			}
			waitForCIReconcilePasses(t, process, "a cycle of bounded passes covering every head",
				func(passes []map[string]string) bool {
					return cycleCompleted(passes) >= 0
				})
		})
	}
}

// starvedCIReconcilePass is the index of the first pass its own bound ended with every head of
// the stand-in listed and none reached, and -1 while no pass has. It reads the pass's fields, not
// its place among the passes: how many passes the process takes to list the heads within its
// second is the machine's.
func starvedCIReconcilePass(passes []map[string]string) int {
	for i, pass := range passes {
		if pass["deadline_reached"] == "true" && pass["rate_limited"] == "false" && pass["heads"] == "0" &&
			pass["heads_known"] == strconv.Itoa(ciReconcileCursorHeads) {
			return i
		}
	}
	return -1
}

// readingCIReconcilePass is the index of the first pass at or after from whose line counts a
// GitHub read, and -1 while none has. The line's `requests` is the queries and REST reads the
// pass asked of its GitHub client, each counted as it is asked -- a read a rate limit already held
// answers without sending counts too -- and the installation lookup and token mint are not among
// them. The pass asks for none before it has read the repositories it covers and set its pace
// (cireconcile.Reconciler.reconcile), so a count above zero is a pass that set its pace and began
// reading, and a pass whose own bound fell before then -- a second a loaded machine can spend
// binding the CI record writer or reading those repositories -- counts none, set no pace and says
// nothing of the order or of how it would have paced itself. This reads what the pass did, not
// where it sits among the passes.
func readingCIReconcilePass(passes []map[string]string, from int) int {
	for i := from; i < len(passes); i++ {
		if sent, err := strconv.Atoi(passes[i]["requests"]); err == nil && sent > 0 {
			return i
		}
	}
	return -1
}

// requireCIReconcileRecordedRun requires the record the CI bucket holds of the last head of the
// order to carry the stand-in's one completed check run.
func requireCIReconcileRecordedRun(t *testing.T, natsURL string, process *dispatchProcess) {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	bucket, err := js.KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	sha := ciReconcileCursorSHA(ciReconcileRecordedHead)
	entry, err := bucket.Get(cistore.Key(ciReconcileOwner, ciReconcileName, strconv.Itoa(ciReconcileRecordedHead), sha))
	if err != nil {
		t.Fatalf("the CI bucket holds no record of head %d once the cycle has closed: %v\n%s", ciReconcileRecordedHead, err, process.Output.String())
	}
	var state cistore.State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		t.Fatalf("decode head %d's record: %v", ciReconcileRecordedHead, err)
	}
	if run, ok := state.Checks["build"]; !ok || uint64(run.CheckRunID) != 11 || run.Status != "completed" || run.Conclusion != "success" {
		t.Fatalf("head %d's record holds the checks %+v, want the completed run 11 of build, a success:\n%s", ciReconcileRecordedHead, state.Checks, process.Output.String())
	}
}

// startCIReconcileCursorProcess starts the envoy-dispatch process the cursor tests read: one
// project whose default repository's GitHub is github (seedCIReconcileDefault), a NATS server
// holding the CI state bucket, and the reconcile loop on a one-second interval, which is each
// pass's bound as well. It answers the process and that NATS server's URL.
func startCIReconcileCursorProcess(t *testing.T, github *cursorGitHub) (*dispatchProcess, string) {
	t.Helper()
	database := storetest.Open(t)
	seedCIReconcileDefault(t, database)
	_, natsURL := testnats.Start(t)
	makeCIStateBucket(t, natsURL)
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
	return process, natsURL
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

// waitForCIReconcilePasses waits for the pass lines the process has logged to satisfy enough,
// and answers them. The passes run a second apart, so a cycle of them is several seconds.
func waitForCIReconcilePasses(t *testing.T, process *dispatchProcess, what string, enough func([]map[string]string) bool) []map[string]string {
	t.Helper()
	var passes []map[string]string
	testwait.Until(t, what, func() (fmt.Stringer, bool) {
		passes = ciReconcilePasses(process.Output.String())
		return process.Output, enough(passes)
	})
	return passes
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

// ciReconcileCursorQueryPath is where the stand-in answers the pass's GraphQL query.
const ciReconcileCursorQueryPath = "/graphql"

// cursorGitHub is the loopback GitHub the cursor test's process calls: the installation lookup
// and the token mint, the repository's identity, its ten open pull requests and each head's two
// listings, the one-shot rate limit a test arms on one of those reads, and the one-shot hold.
type cursorGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// limits is the reset of the rate limit the next request to each armed path is answered
	// with, and the path is disarmed once it has been answered so.
	limits map[string]time.Duration
	// holds is the paths whose next request is held until its caller gives it up (holdFirst),
	// and the path is disarmed once a request to it has been held so.
	holds map[string]bool
}

// holdFirst holds the next request to path until the process gives it up, which a pass does at
// its own bound, and answers it nothing: GitHub slower than the pass has time for. The holds of
// two paths are armed and spent apart.
func (g *cursorGitHub) holdFirst(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holds[path] = true
}

// held holds one request when a test armed its path (holdFirst), and reports whether it did. It
// waits on the request's own end -- its caller giving it up, or the stand-in closing -- and on
// no clock. It reads the request's body to its end first: net/http watches a connection for its
// client's close only once the body is read, so a held query, which has a body, would otherwise
// outlive the pass that sent it, and the stand-in's Close would wait for it for good.
func (g *cursorGitHub) held(r *http.Request) bool {
	g.mu.Lock()
	armed := g.holds[r.URL.Path]
	delete(g.holds, r.URL.Path)
	g.mu.Unlock()
	if !armed {
		return false
	}
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
	return true
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
	github := &cursorGitHub{t: t, limits: map[string]time.Duration{}, holds: map[string]bool{}}
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
	mux.HandleFunc("POST "+ciReconcileCursorQueryPath, func(w http.ResponseWriter, r *http.Request) {
		if github.held(r) {
			return
		}
		// This App may not read the repository's Checks through GraphQL, which GitHub answers
		// with a null repository and a FORBIDDEN entry rather than with an HTTP error. The
		// pass then reads that repository's open pull requests and every head of it over REST,
		// two requests a head, since the token above carries no Commit statuses and so no status
		// read follows, which is the read this test's cursor walks: a pass every query of which
		// answered would cover ten heads in one query and leave no cursor to prove.
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
		if github.held(r) || github.limited(w, r) {
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
