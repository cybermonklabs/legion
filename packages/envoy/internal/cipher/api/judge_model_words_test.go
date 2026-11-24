// packages/envoy/internal/cipher/api/judge_model_words_test.go
// The model judge's verdict as the broker serves it, over real HTTP against a real Postgres, when
// whatever answers in the judge's model's place echoes the bearer the judge presented to it: the
// request.judged row GET /v1/grant-log serves the access review, and the refusal the requesting
// agent reads in its request's status.
package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/judge"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// servedBearer is the bearer the judge presents to its model here: made up, holding two of the
// gate's markers (eyJhbGci and -RDX-). servedEcho is words that echo a URL holding the probe's user
// name and password, the bearer whole and a run of 12 of its characters, which the rule for remote
// text reads as clean, so what keeps the bearer out is the run's own record of what it sent.
const (
	servedBearer = "eyJhbGciOiJQUzI1NiJ9.served-RDX-bearer.c2lnbmF0dXJlLXNlcnZlZA"
	servedEcho   = "the issuer at https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/path, then echo " +
		servedBearer + ", a run " + "hbGciOiJQUzI1"
)

// echoedVerdict is a model endpoint speaking Chat Completions whose every reply is the object
// {"verdict": verdict, "reason": servedEcho}, each call failing the test unless it carries
// servedBearer, so the echo is of the bearer the judge sent.
func echoedVerdict(t *testing.T, verdict string) *httptest.Server {
	t.Helper()
	answer, err := json.Marshal(map[string]string{"verdict": verdict, "reason": servedEcho})
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(string(answer))
	if err != nil {
		t.Fatal(err)
	}
	var stream strings.Builder
	for _, chunk := range []string{
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"claude-standin","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"claude-standin","choices":[{"index":0,"delta":{"content":` + string(content) + `},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"claude-standin","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"claude-standin","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24}}`,
	} {
		stream.WriteString("data: " + chunk + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if got := r.Header.Get("Authorization"); got != "Bearer "+servedBearer {
			t.Errorf("the judge presented its model %q, want the bearer the reply echoes", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream.String())
	}))
	t.Cleanup(server.Close)
	return server
}

// judgedServer is the test server with the judge (withJudge's rules) running on its own machine
// against the model endpoint at model, which it presents servedBearer, and the access review
// listed for readsGrantLog alone; it answers the server and the review's token.
func judgedServer(t *testing.T, model string) (*testServer, string) {
	t.Helper()
	ts := newTestServerWith(t, func(d *api.Deps) {
		withJudge(d)
		d.Machine.Judge = judge.New(t.Context(), judge.Options{
			Rules: d.Machine.Airlock,
			Model: judge.Model{ID: "claude-standin", Endpoint: agentloop.Endpoint{
				Kind: agentloop.OpenAIChat, BaseURL: model,
				Token: func(context.Context) (string, error) { return servedBearer, nil },
			}},
			Requests: d.Machine,
		}).Consider
		d.APIAccounts[reviewAccount] = ciphertest.Caller(t, reviewCaller)
	})
	return ts, ts.accountToken(t, reviewAccount, "", time.Hour)
}

// awaitJudged waits until the judge has written its request.judged row for requestID, which it
// writes in the same transaction as any decision it makes.
func (ts *testServer) awaitJudged(t *testing.T, requestID string) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var judged int
		if err := ts.Store.Pool.QueryRow(context.Background(), `select count(*) from audit where kind = 'request.judged' and request_id = $1`, requestID).Scan(&judged); err != nil {
			t.Fatalf("read the audit table: %v", err)
		}
		if judged > 0 {
			return
		}
	}
	t.Fatalf("the judge wrote no request.judged row for %s within a minute", requestID)
}

// withoutRawAnswers is the JSON value v with every member named raw_answer taken out, at any depth:
// the judge keeps the model's unparsed answer as the answer's data, which keeps the URL the model
// wrote as it came, and holds no run of the bearer, which requireNoRunOfServedBearer reads.
func withoutRawAnswers(v any) any {
	switch v := v.(type) {
	case map[string]any:
		delete(v, "raw_answer")
		for key, member := range v {
			v[key] = withoutRawAnswers(member)
		}
	case []any:
		for i, member := range v {
			v[i] = withoutRawAnswers(member)
		}
	}
	return v
}

// requireNoRunOfServedBearer captures text under capture and fails t, naming capture, where it
// holds a run of servedBearer (redacttest.Piece).
func requireNoRunOfServedBearer(t *testing.T, capture, text string) {
	t.Helper()
	redacttest.Capture(t, capture, text)
	if piece := redacttest.Piece(text, servedBearer); piece != "" {
		t.Errorf("REDACT-ASSERT %s: the output prints %q, a run of the bearer the judge presented: %q", capture, piece, text)
	}
}

// TestTheGrantLogServesNoBearerTheJudgesModelEchoed: a request for a key the judge decides, every
// check passing, is judged by a model whose reason echoes the bearer the judge presented, whole, a
// run of it and a URL that holds a password. A deny verdict refuses the request: the status the
// requesting agent reads, whose detail is the judge's refusal, holds no secret (deny/status). A
// deny, a grant and a last reply that is no object of the judge's form, which hands the request to
// its person, each leave a request.judged row: the page GET /v1/grant-log serves the access review
// holds no run of the bearer anywhere (<case>/grant-log), and, but for the raw answer the judge
// keeps as the answer's data, no secret at all (<case>/grant-log-but-raw-answer). Captures
// api.judge-model-words/<case>/<place>.
func TestTheGrantLogServesNoBearerTheJudgesModelEchoed(t *testing.T) {
	for _, c := range []struct{ name, verdict, state string }{
		{"deny", judge.Deny, "denied"},
		{"grant", judge.Grant, "granted"},
		{"malformed", "maybe", "pending"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts, reviewToken := judgedServer(t, echoedVerdict(t, c.verdict).URL)
			enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
			status, held, body := ts.createFor(t, sessionKey, enrollmentID, "export the payroll", 600, "DEEL_API_KEY")
			if status != http.StatusOK || held.State != "pending" || held.JudgeDeciding == nil || !*held.JudgeDeciding {
				t.Fatalf("the create answer = %d %s; want pending, held for the judge", status, body)
			}
			ts.awaitJudged(t, held.RequestID)
			capture := "api.judge-model-words/" + c.name + "/"

			code, body := ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+held.RequestID, nil)
			if read := decode[wireRequestStatus](t, body); code != http.StatusOK || read.State != c.state {
				t.Fatalf("the request's status = %d %s; want it %s", code, body, c.state)
			}
			if c.name == "deny" {
				redacttest.RequireNoPiece(t, capture+"status", string(body), servedBearer)
				if !strings.Contains(string(body), "the model judge refused this request") {
					t.Errorf("the denied request's status %s carries no refusal of the judge's", body)
				}
			}

			status, page := ts.grantLog(t, reviewToken, closeGrantLogPeriod(t, ts).params())
			if status != http.StatusOK || !strings.Contains(string(page), `"request.judged"`) {
				t.Fatalf("GET /v1/grant-log as access-review = %d %s, want 200 and the judge's row", status, page)
			}
			requireNoRunOfServedBearer(t, capture+"grant-log", string(page))
			var served any
			if err := json.Unmarshal(page, &served); err != nil {
				t.Fatal(err)
			}
			rest, err := json.Marshal(withoutRawAnswers(served))
			if err != nil {
				t.Fatal(err)
			}
			redacttest.RequireNone(t, capture+"grant-log-but-raw-answer", string(rest))
		})
	}
}
