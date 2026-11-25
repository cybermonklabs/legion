package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// capabilityBroker is a secrets broker answering GET /v1/settings and a machine login's approve the
// way one release or another does. An older broker (takesFields false) answers settings with no
// approver_fields, as the production broker from before the account binding does, and decodes the
// approve as that broker does (internal/cipher/api's readJSON into decideBody on main): exactly one
// JSON value holding approver and code alone, with json.Decoder.DisallowUnknownFields, refusing any
// other field 400 INVALID_DECISION. A broker that takes the fields answers approver_fields true and
// decodes the three approver fields beside them, refusing any other field the same way.
// settingsStatus and settingsBody, when set, are what its settings read answers instead.
type capabilityBroker struct {
	server         *httptest.Server
	takesFields    bool
	settingsStatus int
	settingsBody   string

	mu            sync.Mutex
	settingsReads int
	approves      []string
}

func newCapabilityBroker(t *testing.T, takesFields bool) *capabilityBroker {
	t.Helper()
	b := &capabilityBroker{takesFields: takesFields}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/settings" {
			b.mu.Lock()
			b.settingsReads++
			b.mu.Unlock()
			switch {
			case b.settingsStatus != 0:
				w.WriteHeader(b.settingsStatus)
				_, _ = w.Write([]byte(`{"code":"INTERNAL","error":"the test's broker cannot answer its settings"}`))
			case b.settingsBody != "":
				_, _ = w.Write([]byte(b.settingsBody))
			case b.takesFields:
				_, _ = w.Write([]byte(`{"store":"gcp-secret-manager","secrets_prefix":"projects/1/secrets/agent-secrets-","secret_type_tag":true,"approver_fields":true}`))
			default:
				_, _ = w.Write([]byte(`{"store":"gcp-secret-manager","secrets_prefix":"projects/1/secrets/agent-secrets-","secret_type_tag":true}`))
			}
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/credential-requests/") || !strings.HasSuffix(r.URL.Path, "/approve") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"not found"}`))
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b.mu.Lock()
		b.approves = append(b.approves, r.URL.Path+" "+string(raw))
		b.mu.Unlock()
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if b.takesFields {
			var body struct {
				Approver                  string  `json:"approver"`
				Code                      *string `json:"code"`
				ApproverIssuer            string  `json:"approver_issuer"`
				ApproverSubject           string  `json:"approver_subject"`
				ApproverSessionGeneration *int64  `json:"approver_session_generation"`
			}
			err = decoder.Decode(&body)
		} else {
			var body struct {
				Approver string  `json:"approver"`
				Code     *string `json:"code"`
			}
			err = decoder.Decode(&body)
		}
		if err == nil && decoder.More() {
			err = errors.New("body must contain one JSON value")
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_DECISION", "error": "body must be one valid JSON object: " + err.Error()})
			return
		}
		_, _ = w.Write([]byte(`{"state":"approved","credential_id":"cred-1"}`))
	}))
	t.Cleanup(b.server.Close)
	return b
}

// read is how many settings reads the broker answered and every approve it received, as
// "<path> <body>".
func (b *capabilityBroker) read() (int, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settingsReads, append([]string(nil), b.approves...)
}

const (
	capabilityIssuer  = "https://accounts.google.com"
	capabilityCarol   = "carol@example.com"
	capabilityAccount = "carol-account"
)

// carolsApprove is the server this file's rows drive, its broker relay at broker, with
// carol@example.com recorded under capabilityAccount and a personal token of hers bound to that
// account, so her approve has an account and a session generation to name. It answers the server's
// dependencies, carol's token and her address's session generation.
func carolsApprove(t *testing.T, broker *capabilityBroker) (http.Handler, Deps, string, int64) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{signInIssuer: capabilityIssuer, agentSecretsURL: broker.server.URL})
	ctx := context.Background()
	seedPeople(t, database, capabilityCarol)
	if _, err := database.Pool.Exec(ctx, `update people set sub = $2 where email = $1`, capabilityCarol, capabilityAccount); err != nil {
		t.Fatalf("record carol's account: %v", err)
	}
	plaintext, hash, err := newAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes, subject)
		values ($1, 'carol''s agent', $2, $3, $4, $5)`, capabilityCarol, hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength],
		[]string{model.ScopePerson, model.ScopeApprove}, capabilityAccount); err != nil {
		t.Fatalf("store carol's token, bound to her account: %v", err)
	}
	var generation int64
	if err := database.Pool.QueryRow(ctx, `select coalesce((select generation from user_sessions where login = $1), 0)`, capabilityCarol).Scan(&generation); err != nil {
		t.Fatalf("read carol's session generation: %v", err)
	}
	return handler, deps, plaintext, generation
}

// TestTheApproveNamesTheApproverFieldsOnlyToABrokerThatSaysItTakesThem
// (cipher.Client.TakesApproverFields): carol's personal token is bound to the account people records
// for her, so her approve of a machine login has an account and a session generation to name.
//
//   - An older broker, whose settings answer 200 with no approver_fields, is sent the approver and
//     the code alone, exactly the body it decodes, and approves it.
//   - A broker whose settings answer approver_fields true is sent approver_issuer, approver_subject
//     and approver_session_generation, and approves it.
//   - A broker whose settings read fails (500), or answers what is not settings (a 200 that is no
//     JSON object, or an approver_fields that is not the literal true or false, null included), is
//     sent nothing: whether it takes the fields is unknown, and the approve is 503
//     AGENT_SECRETS_SETTINGS_UNREAD.
//
// Each approve reads the settings once. Controls: approveCredentialRecord filling the three whatever
// the broker says fails the first row (the older broker refuses them 400 INVALID_DECISION), and
// TakesApproverFields answering a failed read as a broker that does not take the fields fails the
// four unread rows (each is sent the approver and the code, and approves); reading approver_fields
// with json.Unmarshal into a bool fails the null row alone, since that reads null as false.
func TestTheApproveNamesTheApproverFieldsOnlyToABrokerThatSaysItTakesThem(t *testing.T) {
	const todays = `/v1/credential-requests/rec1/approve {"approver":"carol@example.com","code":"ABCD-1234"}`
	for _, row := range []struct {
		name           string
		takesFields    bool
		settingsStatus int
		settingsBody   string
		// want is the one approve the broker receives, or "" for none.
		want func(generation int64) string
	}{
		{name: "an older broker", want: func(int64) string { return todays }},
		{name: "a broker that takes the fields", takesFields: true, want: func(generation int64) string {
			return fmt.Sprintf(`/v1/credential-requests/rec1/approve {"approver":"carol@example.com","code":"ABCD-1234","approver_issuer":%q,"approver_subject":%q,"approver_session_generation":%d}`,
				capabilityIssuer, capabilityAccount, generation)
		}},
		{name: "a broker whose settings read fails", takesFields: true, settingsStatus: http.StatusInternalServerError, want: func(int64) string { return "" }},
		{name: "a 200 that is no settings object", takesFields: true, settingsBody: `<html>default backend</html>`, want: func(int64) string { return "" }},
		{name: "an approver_fields that is no boolean", takesFields: true, settingsBody: `{"approver_fields":"yes"}`, want: func(int64) string { return "" }},
		{name: "an approver_fields that is null", takesFields: true, settingsBody: `{"approver_fields":null}`, want: func(int64) string { return "" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			broker := newCapabilityBroker(t, row.takesFields)
			broker.settingsStatus, broker.settingsBody = row.settingsStatus, row.settingsBody
			handler, _, token, generation := carolsApprove(t, broker)
			response := agentRequest(t, handler, http.MethodPost, "/api/v1/credential-requests/rec1/approve", map[string]any{"code": "ABCD-1234"}, token)
			reads, approves := broker.read()
			if reads != 1 {
				t.Errorf("the approve read the broker's settings %d times, want once", reads)
			}
			want := row.want(generation)
			if want == "" {
				if len(approves) != 0 {
					t.Fatalf("the broker received %q, want no approve: its settings could not be read", approves)
				}
				if response.Code != http.StatusServiceUnavailable || responseCode(t, response) != "AGENT_SECRETS_SETTINGS_UNREAD" {
					t.Fatalf("carol's approve answered %d %s, want 503 AGENT_SECRETS_SETTINGS_UNREAD", response.Code, response.Body.String())
				}
				return
			}
			if len(approves) != 1 || approves[0] != want {
				t.Fatalf("the broker received %q, want only %s", approves, want)
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"approved"`) {
				t.Fatalf("carol's approve answered %d %s, want the broker's 200 approved", response.Code, response.Body.String())
			}
		})
	}
}

// deadlineRecorder is a RoundTripper that notes, for each request Dispatch's broker client sends,
// its path, when it was handed over and the deadline its context carries, and then sends it.
type deadlineRecorder struct {
	mu   sync.Mutex
	sent []sentRequest
}

type sentRequest struct {
	path     string
	at       time.Time
	deadline time.Time
	bounded  bool
}

func (d *deadlineRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	deadline, bounded := request.Context().Deadline()
	d.mu.Lock()
	d.sent = append(d.sent, sentRequest{path: request.URL.Path, at: time.Now(), deadline: deadline, bounded: bounded})
	d.mu.Unlock()
	return http.DefaultTransport.RoundTrip(request)
}

// TestTheSettingsReadAndTheRelayShareOneDeadline (api.relayDeadline): the approve reaches the broker
// within the client's window of the approver's check, whatever the settings read before it costs, the
// bound machine.ApprovalInFlightWindow counts. So the approve's request carries a deadline no later
// than the client's window after the settings read was sent, which followed the check: one taken
// when the settings read began, not when the approve did. A relay given a window of its own (the
// request's context, with only the client's per-call timeout) carries a later one, or none.
//
// Control: client.Approve run on the request's context in place of relay fails here: its deadline
// is the client's window after the approve was sent, after the settings read.
func TestTheSettingsReadAndTheRelayShareOneDeadline(t *testing.T) {
	broker := newCapabilityBroker(t, true)
	handler, deps, token, _ := carolsApprove(t, broker)
	recorder := &deadlineRecorder{}
	deps.AgentSecrets.HTTP.Transport = recorder
	window := deps.AgentSecrets.HTTP.Timeout
	if window <= 0 {
		t.Fatalf("the broker client's window is %s, want the five seconds cipher.New gives it", window)
	}
	started := time.Now()
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/credential-requests/rec1/approve", map[string]any{"code": "ABCD-1234"}, token)
	if response.Code != http.StatusOK {
		t.Fatalf("carol's approve answered %d %s, want 200", response.Code, response.Body.String())
	}
	recorder.mu.Lock()
	sent := append([]sentRequest(nil), recorder.sent...)
	recorder.mu.Unlock()
	if len(sent) != 2 || sent[0].path != "/v1/settings" || sent[1].path != "/v1/credential-requests/rec1/approve" {
		t.Fatalf("Dispatch sent the broker %+v, want its settings read and then the approve", sent)
	}
	settings, approve := sent[0], sent[1]
	if !approve.bounded {
		t.Fatal("the approve's request carries no deadline, want the one its check began")
	}
	if latest := settings.at.Add(window); approve.deadline.After(latest) {
		t.Fatalf("the approve's deadline is %s after the settings read was sent, want no more than the client's window %s: the relay has a window of its own",
			approve.deadline.Sub(settings.at), window)
	}
	if earliest := started.Add(window); approve.deadline.Before(earliest) {
		t.Fatalf("the approve's deadline is %s before the request began plus the window, want it taken after the request began", earliest.Sub(approve.deadline))
	}
}
