package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// requestAnswer is how a narrowBroker answers POST /v1/requests, which is what tells apart the
// three brokers a narrowed run can meet: one that takes the github_app field, one from before
// it, and one that read the narrowing and will not serve it.
type requestAnswer string

const (
	// requestTakesNarrowing grants every request, the github_app field among the ones it reads.
	requestTakesNarrowing requestAnswer = "takes"
	// requestRefusesUnknownField is a broker from before that field: its readJSON decodes with
	// DisallowUnknownFields, so a body carrying github_app is refused 400 INVALID_REQUEST before
	// the request machine runs, leaving the signed request object unread and its jti unspent.
	requestRefusesUnknownField requestAnswer = "unknown-field"
	// requestRefusesNarrowing is a broker that read the narrowing and will not serve it: 422
	// NARROWING_REFUSED, the answer its release route gives such a narrowing.
	requestRefusesNarrowing requestAnswer = "narrowing-refused"
)

// narrowBroker is a fake broker for the exec form's --github-* flags: every request is granted
// at once, under a grant that holds every name the request asked for, and each release records
// the body it was sent.
//
// values is what each name is released as. With none, each release answers the one name its
// grant holds: a grant of OLD_APP answers as a broker from before narrowing (no github_app in
// the answer, the value owner-wide), and any other grant echoes the github_app it was sent and
// answers "narrowed" for one, "owner-wide" for none. With values, each name is answered its own
// entry and the github_app the release was sent is answered back, as a broker that applied it
// does, so narrowedAsAsked passes and the flag under test is what decides the run.
type narrowBroker struct {
	srv    *httptest.Server
	values map[string]string

	mu       sync.Mutex
	answer   requestAnswer       // how POST /v1/requests answers
	requests []string            // each POST /v1/requests body, as it was sent
	bodies   []string            // each release's body, "" for none
	grants   map[string][]string // grant id -> the names that grant holds
}

// requestBody is POST /v1/requests's body as a broker that takes the narrowing reads it: the
// signed request object, the wake-only session id, and the narrowing the release will ask for.
type requestBody struct {
	Request   string          `json:"request"`
	SessionID *string         `json:"session_id"`
	GitHubApp json.RawMessage `json:"github_app"`
}

// readRequestBody decodes raw as the broker this answer stands for reads it. A broker from
// before the narrowing declares no github_app field, and the broker's readJSON
// (internal/broker/api) decodes with DisallowUnknownFields, so a body carrying one is an error
// here as it is there.
func readRequestBody(raw []byte, answer requestAnswer) (requestBody, error) {
	var body requestBody
	if answer != requestRefusesUnknownField {
		return body, json.Unmarshal(raw, &body)
	}
	var older struct {
		Request   string  `json:"request"`
		SessionID *string `json:"session_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&older); err != nil {
		return requestBody{}, err
	}
	return requestBody{Request: older.Request, SessionID: older.SessionID}, nil
}

// writeRefusal is the broker's error envelope under its status, {"code":...,"error":...}, which
// is where the client reads the code of a refusal (its apiError).
func writeRefusal(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": message})
}

func newNarrowBroker(t *testing.T, values map[string]string) *narrowBroker {
	t.Helper()
	b := &narrowBroker{values: values, answer: requestTakesNarrowing, grants: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.requests = append(b.requests, string(raw))
		answer := b.answer
		b.mu.Unlock()
		body, err := readRequestBody(raw, answer)
		if err != nil {
			writeRefusal(w, http.StatusBadRequest, "INVALID_REQUEST", "body must be one valid JSON object: "+err.Error())
			return
		}
		if answer == requestRefusesNarrowing && body.GitHubApp != nil {
			writeRefusal(w, http.StatusUnprocessableEntity, "NARROWING_REFUSED",
				"github app: the narrowing names a repository the installation does not reach")
			return
		}
		obj, err := record.VerifyRequestObject(body.Request, b.srv.URL, time.Minute, time.Now())
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(obj.Details))
		granted := make([]map[string]string, 0, len(obj.Details))
		for _, detail := range obj.Details {
			names = append(names, detail.Identifier)
			granted = append(granted, map[string]string{"name": detail.Identifier, "decision": "automatic"})
		}
		// One grant id for the whole request, so a release reads back every name it holds.
		held := strings.ToLower(strings.Join(names, "-"))
		b.mu.Lock()
		b.grants["grant-"+held] = names
		b.mu.Unlock()
		writeJSON(w, map[string]any{
			"request_id": "req-" + held, "state": "granted",
			"secrets":  granted,
			"grant_id": "grant-" + held, "record_id": nil, "approver": nil, "lifetime_seconds": nil,
		})
	})
	mux.HandleFunc("POST /v1/grants/{id}/values", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		held := b.grants[r.PathValue("id")]
		b.mu.Unlock()
		var sent struct {
			GitHubApp json.RawMessage `json:"github_app"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &sent); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		if b.values != nil {
			values := make(map[string]string, len(held))
			for _, name := range held {
				values[name] = b.values[name]
			}
			answer := map[string]any{"values": values, "expires_at": nil}
			if sent.GitHubApp != nil {
				answer["github_app"] = sent.GitHubApp
			}
			writeJSON(w, answer)
			return
		}
		// The one name this half answers for, as the grant holds it: a fake that read the name
		// out of its own grant id would make that id's shape a contract of the fake's.
		name := held[0]
		answer := map[string]any{"values": map[string]string{name: "owner-wide"}, "expires_at": nil}
		switch {
		case sent.GitHubApp == nil || name == "OLD_APP":
		case name == "OTHER_APP":
			// A broker that applied another narrowing than the one sent: the owner alone.
			var asked GitHubNarrowing
			_ = json.Unmarshal(sent.GitHubApp, &asked)
			answer["values"] = map[string]string{name: "narrowed-otherwise"}
			answer["github_app"] = GitHubNarrowing{Owner: asked.Owner}
		default:
			answer["values"] = map[string]string{name: "narrowed"}
			answer["github_app"] = sent.GitHubApp
		}
		writeJSON(w, answer)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// take returns the release bodies the broker recorded since the last take, and forgets them.
func (b *narrowBroker) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	bodies := b.bodies
	b.bodies = nil
	return bodies
}

// takeRequests returns each POST /v1/requests body the broker received since the last call, as
// the JSON object it was sent as, and forgets them.
func (b *narrowBroker) takeRequests(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	sent := b.requests
	b.requests = nil
	b.mu.Unlock()
	bodies := make([]map[string]any, 0, len(sent))
	for _, raw := range sent {
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("POST /v1/requests body %q: %v", raw, err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// answersRequests makes POST /v1/requests answer as answer says from here on.
func (b *narrowBroker) answersRequests(answer requestAnswer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer = answer
}

// TestTheGitHubFlagsNarrowTheRelease: the exec form's --github-owner, --github-repo and
// --github-read-only send the release a github_app narrowing, the owner lowercased and the
// repositories sorted, each once, and run the command once the broker answers that it applied that
// narrowing. Without them the release carries no body at all, as before.
func TestTheGitHubFlagsNarrowTheRelease(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, nil)
	keyDir := newKeyDir(t)
	for _, tc := range []struct {
		name string
		args []string
		body string
		ran  string
	}{
		{
			"every flag",
			[]string{"APP", "--github-owner", "Acme-Org", "--github-repo", "widgets", "--github-repo=proofs", "--github-repo", "widgets", "--github-read-only"},
			`{"github_app":{"owner":"acme-org","repositories":["proofs","widgets"],"read_only":true}}`,
			"narrowed",
		},
		{"the owner and read-only", []string{"APP", "--github-owner=ada", "--github-read-only"}, `{"github_app":{"owner":"ada","read_only":true}}`, "narrowed"},
		{"no flag", []string{"APP"}, "", "owner-wide"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.args, "--", "sh", "-c", `echo "ran $APP"`)
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
			bodies := b.take()
			if exit != 0 || stdout != "ran "+tc.ran+"\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 running with the %s value", exit, stdout, stderr, tc.ran)
			}
			if len(bodies) != 1 || strings.TrimSpace(bodies[0]) != tc.body {
				t.Fatalf("release bodies = %q, want one release carrying %q", bodies, tc.body)
			}
		})
	}
}

// TestANarrowingTheBrokerDidNotApplyRunsNothing: a broker from before narrowing ignores the
// release's body and answers the owner-wide token with no github_app, so the exec form exits 1
// saying so and runs nothing, rather than handing the command a token wider than it asked for.
func TestANarrowingTheBrokerDidNotApplyRunsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, nil)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"OLD_APP", "--github-owner", "ada", "--github-read-only", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "did not narrow") || !strings.Contains(stderr, "nothing was run") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 saying the broker did not narrow and nothing was run", exit, stdout, stderr)
	}
	if bodies := b.take(); len(bodies) != 1 || !strings.Contains(bodies[0], `"github_app"`) {
		t.Fatalf("release bodies = %q, want the one narrowed release", bodies)
	}
}

// TestANarrowingTheBrokerAppliedOtherwiseRunsNothing: an answer naming another narrowing than the
// one asked for (here the owner alone, without the read-only limit) is no narrowing the command
// asked for, so the exec form exits 1 and runs nothing.
func TestANarrowingTheBrokerAppliedOtherwiseRunsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, nil)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"OTHER_APP", "--github-owner", "ada", "--github-read-only", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "did not narrow") || !strings.Contains(stderr, "nothing was run") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 saying the broker did not narrow as asked and nothing was run", exit, stdout, stderr)
	}
}

// TestAGitHubNarrowingTheBrokerWouldRefuseExitsTwo: --github-repo, --github-read-only and
// --github-token each name one account's token, so without --github-owner each is a usage error,
// and the refusal names the flag the caller gave: --github-token beside --github-repo names
// --github-token, since a caller who asked for a token and named no account is told about the
// flag they asked with. So is a --github-owner that names no account (an unset variable a helper
// passed), which would otherwise send no narrowing at all, and any narrowing the broker's parser
// refuses (policy.ParseGitHubNarrowing): the CLI runs that parser itself, so nothing reaches the
// broker, not even the request a person would approve.
func TestAGitHubNarrowingTheBrokerWouldRefuseExitsTwo(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, nil)
	keyDir := newKeyDir(t)
	for _, tc := range []struct {
		flags []string
		names string
	}{
		{[]string{"--github-repo", "widgets"}, "--github-owner"},
		{[]string{"--github-read-only"}, "--github-owner"},
		{[]string{"--github-token"}, "--github-token"},
		{[]string{"--github-token", "--github-repo", "widgets"}, "--github-token"},
		{[]string{"--github-owner", ""}, "--github-owner"},
		{[]string{"--github-owner="}, "--github-owner"},
		{[]string{"--github-owner", "  ", "--github-read-only"}, "--github-owner"},
		{[]string{"--github-owner", "ada acme"}, "--github-owner"},
		{[]string{"--github-owner", "ada", "--github-repo", "acme/widgets"}, "--github-repo"},
	} {
		args := append(append([]string{"APP"}, tc.flags...), "--", "sh", "-c", "echo ran-the-child")
		stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
		if exit != 2 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, tc.names) {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want exit 2 naming %s", tc.flags, exit, stdout, stderr, tc.names)
		}
		if requests, bodies := b.takeRequests(t), b.take(); len(requests) != 0 || len(bodies) != 0 {
			t.Fatalf("%v: the broker was asked %d requests and %d releases, want none", tc.flags, len(requests), len(bodies))
		}
	}
}

// TestARequestCarriesTheNarrowingAndIsSentAgainForOneRefusalAlone: POST /v1/requests carries the
// narrowing the release will ask for as github_app, so a broker that cannot serve it refuses it
// before anybody is asked to approve the request, and a run that asks for no narrowing sends the
// body it always sent. A broker from before that field refuses the whole body 400
// INVALID_REQUEST without reading the request object, so the client sends that same signed
// object once more carrying no such field and the run gets its secret and runs its child. That
// second call is for that one refusal: a broker that answers 422 NARROWING_REFUSED is asked
// once, and its refusal reaches the caller, who runs nothing.
func TestARequestCarriesTheNarrowingAndIsSentAgainForOneRefusalAlone(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	narrowed := map[string]any{"owner": "ada", "read_only": true}
	for _, tc := range []struct {
		name   string
		answer requestAnswer
		flags  []string
		// sent is the github_app each POST /v1/requests carried, in order, nil for a body that
		// carried no such field at all.
		sent   []map[string]any
		stdout string
		exit   int
		names  string // what the refusal must name, "" for a run nothing refused
	}{
		{
			name:   "a broker that takes the field is asked once",
			answer: requestTakesNarrowing,
			flags:  []string{"--github-owner", "ada", "--github-read-only"},
			sent:   []map[string]any{narrowed},
			stdout: "ran narrowed\n",
		},
		{
			name:   "a run with no narrowing sends the body it always sent",
			answer: requestTakesNarrowing,
			sent:   []map[string]any{nil},
			stdout: "ran owner-wide\n",
		},
		{
			name:   "a broker from before the field is sent the same object again without it",
			answer: requestRefusesUnknownField,
			flags:  []string{"--github-owner", "ada", "--github-read-only"},
			sent:   []map[string]any{narrowed, nil},
			stdout: "ran narrowed\n",
		},
		{
			name:   "a narrowing the broker refuses reaches the caller and is not sent again",
			answer: requestRefusesNarrowing,
			flags:  []string{"--github-owner", "ada", "--github-read-only"},
			sent:   []map[string]any{narrowed},
			exit:   1,
			names:  "NARROWING_REFUSED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newNarrowBroker(t, nil)
			b.answersRequests(tc.answer)
			args := append(append([]string{"APP"}, tc.flags...), "--", "sh", "-c", `echo "ran $APP"`)
			// Every variable the client would forward as session_id is cleared, so each body
			// below is the one a session outside omp sends (TestRequestSignsARequestObject).
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir,
				[]string{"OMP_SESSION_ID=", "ENVOY_SESSION_ID=", "CLAUDE_CODE_SESSION_ID="}, args...)
			if exit != tc.exit || stdout != tc.stdout {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d with stdout %q", exit, stdout, stderr, tc.exit, tc.stdout)
			}
			if tc.names != "" && !strings.Contains(stderr, tc.names) {
				t.Fatalf("stderr = %q, want the refusal to name %s", stderr, tc.names)
			}
			sent := b.takeRequests(t)
			if len(sent) != len(tc.sent) {
				t.Fatalf("the broker was asked %d POST /v1/requests calls, want %d", len(sent), len(tc.sent))
			}
			for i, body := range sent {
				want := map[string]bool{"request": true, "session_id": true}
				if tc.sent[i] != nil {
					want["github_app"] = true
				}
				if got := keySet(body); !mapsEqual(got, want) {
					t.Fatalf("request %d carried the fields %v, want %v", i+1, got, want)
				}
				if body["session_id"] != nil {
					t.Fatalf("request %d session_id = %v, want a null", i+1, body["session_id"])
				}
				if tc.sent[i] != nil && !reflect.DeepEqual(body["github_app"], any(tc.sent[i])) {
					t.Fatalf("request %d github_app = %v, want %v", i+1, body["github_app"], tc.sent[i])
				}
				if body["request"] != sent[0]["request"] {
					t.Fatalf("request %d carried another signed object than the first, want the one object sent again", i+1)
				}
			}
		})
	}
}
