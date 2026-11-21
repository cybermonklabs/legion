package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// brokerStandIn stands in for the two routes of the secrets broker a switch to private calls, to
// Dispatch's own account token alone: the count of a project's live developer-box tokens (GET
// /v1/admin/machine-tokens), which records the project in the broker's store of private projects
// first when slack_channels is present and answers the record's number from one sequence, and the
// forget of such a record (POST /v1/admin/private-projects/forget). It counts live[project]
// tokens, fails every call while down, and answers each forget the next status of forgetSequence,
// then forgetStatus once the sequence is spent: 0 is 200 forgotten, http.StatusConflict is
// RECORD_IN_FLIGHT with retryAfter seconds, any other its own status. It keeps every call it
// answered.
type brokerStandIn struct {
	mu             sync.Mutex
	live           map[string]int
	down           bool
	record         int64
	forgetStatus   int
	forgetSequence []int
	retryAfter     int
	counts         []standInCount
	forgets        []standInForget
}

// standInCount is one count the stand-in answered: its project, the channels it recorded (nil when
// the call sent no slack_channels, which records nothing), deadline_seconds as sent, and the record
// number it answered, 0 for none.
type standInCount struct {
	Project  string
	Channels []string
	Deadline string
	Record   int64
}

// standInForget is one forget the stand-in answered: its project and record, nil for none.
type standInForget struct {
	Project string
	Record  *int64
}

const brokerStandInAccount = "dispatch-account-token"

func newBrokerStandIn() *brokerStandIn { return &brokerStandIn{live: map[string]int{}} }

// serve starts the stand-in for t and answers the Dispatch broker client that calls it.
func (b *brokerStandIn) serve(t testing.TB) *agentsecrets.Client {
	t.Helper()
	stand := httptest.NewServer(b)
	t.Cleanup(stand.Close)
	return agentsecrets.New(stand.URL, accountTokenFile(t, brokerStandInAccount))
}

func (b *brokerStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.down:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"DATABASE_UNAVAILABLE","error":"down"}`))
	case r.Header.Get("Authorization") != "Bearer "+brokerStandInAccount:
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"ACCOUNT_LACKS_POWER","error":"not dispatch"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/machine-tokens":
		query := r.URL.Query()
		project := query.Get("project")
		count := standInCount{Project: project, Deadline: query.Get("deadline_seconds")}
		answer := map[string]any{"project": project, "live_tokens": b.live[project], "last_expires_at": nil}
		if listed, present := query["slack_channels"]; present {
			count.Channels = []string{}
			if listed[0] != "" {
				count.Channels = strings.Split(listed[0], ",")
			}
			b.record++
			count.Record = b.record
			answer["record"] = b.record
		}
		if b.live[project] > 0 {
			answer["last_expires_at"] = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
		}
		b.counts = append(b.counts, count)
		_ = json.NewEncoder(w).Encode(answer)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/private-projects/forget":
		var forget standInForget
		if err := json.NewDecoder(r.Body).Decode(&struct {
			Project *string `json:"project"`
			Record  **int64 `json:"record"`
		}{&forget.Project, &forget.Record}); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b.forgets = append(b.forgets, forget)
		status := b.forgetStatus
		if len(b.forgetSequence) > 0 {
			status, b.forgetSequence = b.forgetSequence[0], b.forgetSequence[1:]
		}
		switch status {
		case 0:
			_, _ = w.Write([]byte(`{"outcome":"forgotten"}`))
		case http.StatusConflict:
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "RECORD_IN_FLIGHT", "error": "in flight", "retry_after_seconds": b.retryAfter})
		default:
			w.WriteHeader(status)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *brokerStandIn) set(project string, live int, down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.live[project], b.down = live, down
}

// answered is a copy of the counts and forgets the stand-in has answered so far.
func (b *brokerStandIn) answered() ([]standInCount, []standInForget) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]standInCount(nil), b.counts...), append([]standInForget(nil), b.forgets...)
}

// startClosing is the owner's first switch of key to private through handler, which starts its
// closing state and answers 409 DEVELOPER_BOX_TOKENS_LIVE (closeForDeveloperBoxes), and then moves
// the state's start an hour back, past developerBoxSettle, so the owner's next switch completes
// while the broker counts none live. A switch to private through the route takes two requests, as
// the broker reads only a committed closing state; a test about what the second does starts here.
func startClosing(t *testing.T, handler http.Handler, database *store.Store, key, owner string) {
	t.Helper()
	first := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/"+key+"/access", map[string]string{"visibility": "private"}, owner)
	if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), `"code":"DEVELOPER_BOX_TOKENS_LIVE"`) {
		t.Fatalf("the first switch of %s to private = %d %s, want 409 DEVELOPER_BOX_TOKENS_LIVE, the closing state started", key, first.Code, first.Body.String())
	}
	if _, err := database.Pool.Exec(context.Background(), `update projects set closing_since = closing_since - interval '1 hour' where key = $1`, key); err != nil {
		t.Fatalf("settle %s's closing state: %v", key, err)
	}
}

// fullSwitch is the owner's whole switch of key to private through handler: startClosing, then
// the switch body answers, the response of that second request.
func fullSwitch(t *testing.T, handler http.Handler, database *store.Store, key, owner string, body any) *httptest.ResponseRecorder {
	t.Helper()
	startClosing(t, handler, database, key, owner)
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/"+key+"/access", body, owner)
}
