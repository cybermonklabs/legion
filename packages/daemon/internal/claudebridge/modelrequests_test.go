package claudebridge

//ci:reads packages/contracts/fixtures/refusal-classes.json TestBridgeReportsModelRequests reads it

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// windowClock is a Config.Clock on which only the model requests' batch window passes, and only
// when the row fires it: fire fires every window asked for so far. Every other timer of the
// bridge's never fires on it, so a row reads what the bridge sent before the window, at it, and at
// the turn's end apart.
type windowClock struct {
	mu      sync.Mutex
	waiting []chan time.Time
	at      time.Time
}

func (c *windowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *windowClock) After(d time.Duration) <-chan time.Time {
	fires := make(chan time.Time, 1)
	if d == requestWindow {
		c.mu.Lock()
		c.waiting = append(c.waiting, fires)
		c.mu.Unlock()
	}
	return fires
}

// fire fires each window the bridge has asked for, and answers how many.
func (c *windowClock) fire() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(requestWindow)
	for _, fires := range c.waiting {
		fires <- c.at
	}
	fired := len(c.waiting)
	c.waiting = nil
	return fired
}

// requestsRig is a rig whose pane names a daemon that takes both of the bridge's reports: each
// report of model requests on requests, held whole, and each turn outcome on outcomes.
type requestsRig struct {
	*rig
	clock    *windowClock
	requests chan claim.ModelRequestsRequest
	outcomes chan claim.TurnOutcomeRequest
	// started is when the rig started: a request's end at or after it is the bridge's clock.
	started time.Time
}

func newRequestsRig(t *testing.T, env []string) *requestsRig {
	t.Helper()
	r := &requestsRig{
		// Place the reporting clock at the earliest recorded stream so replays retain their dates.
		clock:    &windowClock{at: time.Date(2026, time.October, 30, 9, 1, 26, 0, time.UTC)},
		requests: make(chan claim.ModelRequestsRequest, 16),
		outcomes: make(chan claim.TurnOutcomeRequest, 16),
		started:  time.Now(),
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read the bridge's report: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/legion/v1/claims/model-requests":
			decoder := json.NewDecoder(strings.NewReader(string(body)))
			decoder.DisallowUnknownFields()
			var report claim.ModelRequestsRequest
			if err := decoder.Decode(&report); err != nil {
				t.Errorf("decode the bridge's report of model requests %s: %v", body, err)
				return
			}
			r.requests <- report
			kept := make([]string, 0, len(report.Requests))
			for _, request := range report.Requests {
				kept = append(kept, request.ID)
			}
			_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: kept})
		case "/legion/v1/claims/turn-outcome":
			var report claim.TurnOutcomeRequest
			if err := json.Unmarshal(body, &report); err != nil {
				t.Errorf("decode the bridge's turn outcome %s: %v", body, err)
				return
			}
			r.outcomes <- report
			_ = json.NewEncoder(w).Encode(claim.TurnOutcomeResponse{})
		default:
			t.Errorf("the bridge posted to %s", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(daemon.Close)
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.rig = startRig(t, append([]string{
		"LEGION_DAEMON_URL=" + daemon.URL,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		"LEGION_GENERATION=4",
	}, env...), r.clock, "-p", "--resume", sessionID)
	return r
}

// reported is the next report of model requests the bridge sent, waited for until deadlineMargin
// before the test binary's deadline.
func (r *requestsRig) reported() claim.ModelRequestsRequest {
	r.t.Helper()
	select {
	case report := <-r.requests:
		return report
	case <-time.After(deadlineWait(r.t)):
		r.t.Fatalf("the bridge reported no model requests, %s before the test binary's deadline; the bridge logged:\n%s", deadlineMargin, r.log)
		return claim.ModelRequestsRequest{}
	}
}

// rows is each request of reports as rowsOf reads it, against the rig's start.
func (r *requestsRig) rows(reports ...claim.ModelRequestsRequest) []requestRow {
	r.t.Helper()
	return rowsOf(r.t, r.started, reports...)
}

// noReport holds that the bridge sends no report of model requests within a short while.
func (r *requestsRig) noReport() {
	r.t.Helper()
	select {
	case report := <-r.requests:
		r.t.Fatalf("the bridge reported %+v, want no report", report)
	case <-time.After(300 * time.Millisecond):
	}
}

// untilAnswer reads the bridge's frames up to the message_end that names the model of the
// assistant line just read: every line before it has reached the bridge.
func (r *requestsRig) untilAnswer() {
	r.t.Helper()
	for !answerFrame(r.nextRaw()) {
	}
}

// refusalClasses is packages/contracts/fixtures/refusal-classes.json, the table of refusal texts
// and their classes that the Oh My Pi plugin's test (packages/pi-legion/src/model-requests.test.ts)
// reads too, so the two hosts class one text alike.
type refusalClasses struct {
	Patterns struct {
		Own      string `json:"own"`
		Provider string `json:"provider"`
	} `json:"patterns"`
	Cases []struct {
		Name    string             `json:"name"`
		Text    string             `json:"text"`
		Refusal claim.RefusalClass `json:"refusal"`
	} `json:"cases"`
}

func readRefusalClasses(t *testing.T) refusalClasses {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "refusal-classes.json"))
	if err != nil {
		t.Fatalf("read the fixture refusal-classes.json: %v", err)
	}
	var classes refusalClasses
	if err := json.Unmarshal(raw, &classes); err != nil {
		t.Fatalf("decode the fixture refusal-classes.json: %v", err)
	}
	return classes
}

// requestRow is what a row asserts of one reported request: everything but its id, and its
// end where the stream named one.
type requestRow struct {
	session  string
	model    string
	endedAt  string
	uncached string
	refusal  claim.RefusalClass
}

// rowsOf is each request of reports as a requestRow, and fails on a request that names no id, no
// end, or an id another request of reports has: each request counts once. It fails too on a
// report that does not say its host reports each 429 its transport retries, which the bridge does
// by each `api_retry` line, so a Claude Code session never leaves the provider's refusals
// unrecorded. An end at or after since is the bridge's own clock, for a line that names none (a
// retry line), and reads "now"; uncached is "" where the request names none.
func rowsOf(t *testing.T, since time.Time, reports ...claim.ModelRequestsRequest) []requestRow {
	t.Helper()
	var rows []requestRow
	seen := map[string]bool{}
	for _, report := range reports {
		if report.BootToken != testBootToken || report.Generation != 4 || report.ClaimToken != "" || report.Secret != "" {
			t.Fatalf("the report named %+v, want the launch's boot token and generation 4 and no other credential", report)
		}
		if !report.RetriedRefusalsReported {
			t.Fatalf("the report %+v does not say that its host reports each 429 its transport retries, which the bridge does by each api_retry line", report)
		}
		for _, request := range report.Requests {
			if request.ID == "" || seen[request.ID] || request.EndedAt.IsZero() {
				t.Fatalf("the request %+v has no id, an id reported already, or no end", request)
			}
			seen[request.ID] = true
			row := requestRow{session: request.Session, model: request.Model, refusal: request.Refusal,
				endedAt: request.EndedAt.UTC().Format(time.RFC3339Nano)}
			if !request.EndedAt.Before(since) {
				row.endedAt = "now"
			}
			if request.UncachedInputTokens != nil {
				row.uncached = strconv.FormatInt(*request.UncachedInputTokens, 10)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func compareRows(t *testing.T, what string, got, want []requestRow) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s: the bridge reported\n%+v\nwant\n%+v", what, got, want)
	}
}

// The recorded subagent turn (testdata/subagent-turn.jsonl, Claude Code 2.1.311 in the bridge's
// print mode against an Anthropic-shaped stand-in that answered a Task call, the subagent's request
// and the main conversation's answer): the main conversation's two requests, each with
// parent_tool_use_id null and the session's id, and the subagent's, with parent_tool_use_id the
// Task call and the subagent's agent_id.
const (
	capturedSession  = "9af5775d-fd05-42bb-9c9b-8244a87ff114"
	capturedSubagent = "a9d39748b3c5ba11e"
	capturedModel    = "claude-sonnet-4-5"
)

// The bridge reports each model request of the stream within its window of the request's end
// while the turn goes on, and what is left at the turn's end at once; each refused attempt with
// who refused it; the role's own session and each subagent's once each, under the launch's boot
// token alone, each batch saying that its host reports each 429 its transport retries; and
// classes a refusal's text by the project's two patterns as the Oh My Pi plugin does.
func TestBridgeReportsModelRequests(t *testing.T) {
	classes := readRefusalClasses(t)
	patterns := []string{
		"LEGION_SIGNAL_OWN_REFUSAL=" + classes.Patterns.Own,
		"LEGION_SIGNAL_PROVIDER_REFUSAL=" + classes.Patterns.Provider,
	}

	// The window sends every answer seen so far, including the last one whose result has not
	// arrived. The result must not send any of those IDs twice.
	t.Run("partial", func(t *testing.T) {
		r := newRequestsRig(t, patterns)
		r.prompt("p1", "replay subagent-turn.jsonl")
		r.expect("agent_start", nil)
		r.untilAnswer()
		r.untilAnswer()
		r.noReport()
		if fired := r.clock.fire(); fired != 1 {
			t.Fatalf("the bridge asked for %d windows, want the one its first request arms", fired)
		}
		compareRows(t, "the window's report", r.rows(r.reported()), []requestRow{
			{session: capturedSession, model: capturedModel, endedAt: "2026-11-26T05:13:16.266Z", uncached: "49"},
			{session: capturedSubagent, model: capturedModel, endedAt: "2026-11-26T05:13:16.34Z", uncached: "49"},
			{session: capturedSession, model: capturedModel, endedAt: "2026-11-26T05:13:16.441Z", uncached: "49"},
		})
		r.open()
		r.untilTurnEnds()
		r.noReport()
	})

	// The turn ends before the window: every request goes at once, the window the first armed
	// sends nothing more, and the turn's own outcome is reported exactly as before.
	t.Run("end", func(t *testing.T) {
		r := newRequestsRig(t, patterns)
		r.prompt("p1", "replay subagent-turn.jsonl")
		r.expect("agent_start", nil)
		r.open()
		r.untilTurnEnds()
		compareRows(t, "the turn end's report", r.rows(r.reported()), []requestRow{
			{session: capturedSession, model: capturedModel, endedAt: "2026-11-26T05:13:16.266Z", uncached: "49"},
			{session: capturedSubagent, model: capturedModel, endedAt: "2026-11-26T05:13:16.34Z", uncached: "49"},
			{session: capturedSession, model: capturedModel, endedAt: "2026-11-26T05:13:16.441Z", uncached: "49"},
		})
		// The turn outcome's own bound is as long as the window (reportTimeout), so it is read
		// before the window is fired: the fire would end that report's call.
		select {
		case outcome := <-r.outcomes:
			want := claim.TurnOutcomeRequest{BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean}
			if outcome != want {
				t.Fatalf("the turn was reported %+v, want %+v", outcome, want)
			}
		case <-time.After(deadlineWait(t)):
			t.Fatalf("the bridge reported no turn outcome; it logged:\n%s", r.log)
		}
		r.clock.fire()
		r.noReport()
	})

	// Each attempt the provider refused with 429 is a request of its own: Claude Code's retry
	// lines say no text of the refusal (testdata/api-error-429.jsonl:2-3), so each is unknown, and
	// the final attempt's text, which matches neither pattern here, is unknown too; a 529 is no
	// limit's refusal and is not reported; a retry the next attempt answered is the refusal and the
	// answer; and the three lines of one request (testdata/background-turn.jsonl:4-6) are one
	// request, ended at its last line.
	t.Run("refusals", func(t *testing.T) {
		r := newRequestsRig(t, patterns)
		for _, file := range []string{"api-error-429.jsonl", "api-error-529.jsonl", "api-retry-answered.jsonl"} {
			r.prompt("p-"+file, "replay "+file)
			r.expect("agent_start", nil)
			r.open()
			r.untilTurnEnds()
			if err := os.Remove(r.gate); err != nil {
				t.Fatal(err)
			}
		}
		refused := r.rows(r.reported())
		compareRows(t, "the 429's attempts", refused, []requestRow{
			{session: "7e61602e-c2c7-4381-ac7e-04fa5b74b101", model: capturedModel, endedAt: "now", refusal: claim.RefusalUnknown},
			{session: "7e61602e-c2c7-4381-ac7e-04fa5b74b101", model: capturedModel, endedAt: "now", refusal: claim.RefusalUnknown},
			{session: "7e61602e-c2c7-4381-ac7e-04fa5b74b101", model: capturedModel, endedAt: "2026-11-04T17:06:20.465Z", refusal: claim.RefusalUnknown},
		})
		compareRows(t, "the retry an attempt answered", r.rows(r.reported()), []requestRow{
			{session: "d60bd78b-9d12-46e5-86d9-9d02d2edd19e", model: capturedModel, endedAt: "now", refusal: claim.RefusalUnknown},
			{session: "d60bd78b-9d12-46e5-86d9-9d02d2edd19e", model: capturedModel, endedAt: "2026-11-26T05:13:34.279Z", uncached: "49"},
		})
		r.prompt("p-background", "replay background-turn.jsonl")
		r.expect("agent_start", nil)
		r.open()
		r.untilTurnEnds()
		background := r.rows(r.reported())
		if len(background) == 0 || background[0].endedAt != "2026-10-30T09:01:26.35Z" || background[0].uncached != "21305" {
			t.Fatalf("the background turn's requests were reported as %+v, want the first ended at its third line with 10 + 21295 uncached tokens", background)
		}
	})

	// The sessions a Claude Code role's requests run in: its own and each subagent's it starts.
	// Each is reported once, under its own session, and every report names the launch's boot token
	// alone, never a credential of the subagent's (rowsOf).
	t.Run("session-inventory", func(t *testing.T) {
		r := newRequestsRig(t, patterns)
		r.prompt("p1", "replay subagent-turn.jsonl")
		r.expect("agent_start", nil)
		r.open()
		r.untilTurnEnds()
		counted := map[string]int{}
		for _, row := range r.rows(r.reported()) {
			counted[row.session]++
		}
		inventory := map[string]int{
			capturedSession:  2, // the role's own session: the Task call and the answer
			capturedSubagent: 1, // the subagent the Task call started
		}
		for session, want := range inventory {
			if counted[session] != want {
				t.Errorf("session %s was reported %d times, want %d", session, counted[session], want)
			}
		}
		if len(counted) != len(inventory) {
			t.Errorf("the bridge reported the sessions %v, want exactly %v", counted, inventory)
		}
	})

	// A refusal's text is classed by the launch's two patterns, the own one first, as the table
	// both hosts read says; a launch that sets neither classes every refusal unknown.
	t.Run("configured-patterns", func(t *testing.T) {
		for _, configuration := range []struct {
			name string
			env  []string
			want func(claim.RefusalClass) claim.RefusalClass
		}{
			{"both set", patterns, func(c claim.RefusalClass) claim.RefusalClass { return c }},
			{"unset", nil, func(claim.RefusalClass) claim.RefusalClass { return claim.RefusalUnknown }},
		} {
			r := newRequestsRig(t, configuration.env)
			for i, row := range classes.Cases {
				r.prompt("p"+strconv.Itoa(i), "refused429 "+row.Text)
				r.expect("agent_start", nil)
				r.untilTurnEnds()
				reported := r.rows(r.reported())
				if len(reported) != 1 || reported[0].refusal != configuration.want(row.Refusal) {
					t.Errorf("%s, %s: the bridge reported %+v, want one request refused %s", configuration.name, row.Name, reported, configuration.want(row.Refusal))
				}
			}
		}
	})
}
