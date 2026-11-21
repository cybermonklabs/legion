// packages/envoy/cmd/cipher/login_test.go
//
// The session name `agent-secrets login` trades under. The broker binds each name to one
// principal, so a person's trade must ask under a name of their own: unset, the exchange
// refuses a person's trade for want of one, and this machine's own name belongs to
// google:<account> the moment the helper has traded under it — after which the person's trade is
// refused, or, trading first, takes the name and the helper's next machine trade is refused
// invalid_grant, which is permanent and stops its renewal loop for good.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// loginBroker is the token, enrollment and session endpoints `agent-secrets login` and the
// commands after it call: it records the session name and the whole form of each trade, hands out
// an enrollment id per registration, answers a session proof naming any enrollment but the last
// one it made exactly as the broker answers one whose lease has lapsed, and answers GET
// /v1/settings with settings, or 404 while it holds none. Its POST /v1/secrets/{name}/reread,
// once rereadAnswer is set, answers a live session's proof with rereadAnswer, a lapsed one 401
// PROOF_INVALID "proof invalid: not live", and a call with no proof 202, and records each call's
// enrollment ("" for none).
type loginBroker struct {
	server *httptest.Server
	mu     sync.Mutex
	// sessions is the session form field of every trade, in order, and forms each trade's form.
	sessions []string
	forms    []url.Values
	// enrollments counts the registrations, and live is the id of the one whose lease the
	// broker still holds; lapse() ends it, as BROKER_LEASE_SECONDS does.
	enrollments int
	live        string
	settings    map[string]any
	// enrollmentID, when set, is the id every registration is answered with.
	enrollmentID string
	// echoEnrollment, when set, makes every registration's id of the Proof header it carried, as
	// whatever answers in the broker's place can echo the request it was sent.
	echoEnrollment func(proof string) string
	// refusal, once set (refuseTrades), answers every trade.
	refusal *loginRefusal
	// rereadAnswer answers a live session's reread; rereads is each reread's enrollment.
	rereadAnswer func(name string) rereadReply
	rereads      []string
}

// loginRefusal is the answer refuseTrades makes the token endpoint give.
type loginRefusal struct {
	status int
	body   map[string]any
}

func newLoginBroker(t *testing.T) *loginBroker {
	t.Helper()
	b := &loginBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("the trade's body did not parse: %v", err)
		}
		b.mu.Lock()
		b.sessions = append(b.sessions, r.PostForm.Get("session"))
		b.forms = append(b.forms, r.PostForm)
		refusal := b.refusal
		b.mu.Unlock()
		if refusal != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(refusal.status)
			_ = json.NewEncoder(w).Encode(refusal.body)
			return
		}
		writeLoginJSON(w, map[string]any{
			"access_token": "access" + "-token", "token_type": "DPoP", "expires_in": 3600,
			"credential_id": "credential-1", "credential_expires_at": "2099-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		settings := b.settings
		b.mu.Unlock()
		if settings == nil {
			http.NotFound(w, r)
			return
		}
		writeLoginJSON(w, settings)
	})
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.enrollments++
		b.live = fmt.Sprintf("enrollment-%d", b.enrollments)
		if b.enrollmentID != "" {
			b.live = b.enrollmentID
		}
		if b.echoEnrollment != nil {
			b.live = b.echoEnrollment(r.Header.Get("Proof"))
		}
		id := b.live
		b.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeLoginJSON(w, map[string]any{"enrollment_id": id})
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		live := b.live
		b.mu.Unlock()
		if proofEnrollment(t, r) != live {
			w.WriteHeader(http.StatusUnauthorized)
			writeLoginJSON(w, map[string]any{"code": "PROOF_INVALID", "error": "proof invalid: not live"})
			return
		}
		writeLoginJSON(w, map[string]any{"enrollment_id": live, "runtime_id": "this-box", "grants": []any{}})
	})
	mux.HandleFunc("POST /v1/secrets/{name}/reread", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		answer, live := b.rereadAnswer, b.live
		b.mu.Unlock()
		if answer == nil {
			http.NotFound(w, r)
			return
		}
		enrollment := ""
		if r.Header.Get("Proof") != "" {
			enrollment = proofEnrollment(t, r)
		}
		b.mu.Lock()
		b.rereads = append(b.rereads, enrollment)
		b.mu.Unlock()
		switch {
		case enrollment == "":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"accepted":true}` + "\n"))
		case enrollment != live:
			w.WriteHeader(http.StatusUnauthorized)
			writeLoginJSON(w, map[string]any{"code": "PROOF_INVALID", "error": "proof invalid: not live"})
		default:
			reread := answer(r.PathValue("name"))
			writeLoginJSON(w, map[string]any{"name": reread.Name, "served": reread.Served, "reason": reread.Reason, "type": reread.Type})
		}
	})
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

// lapse ends the lease of the enrollment the broker holds, as BROKER_LEASE_SECONDS does for a
// session nothing renews.
func (b *loginBroker) lapse() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.live = ""
}

// refuseTrades answers every later trade with status and body.
func (b *loginBroker) refuseTrades(status int, body map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refusal = &loginRefusal{status: status, body: body}
}

// liveEnrollment is the enrollment the broker holds a lease for.
func (b *loginBroker) liveEnrollment() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live
}

// rereadEnrollments is each reread's enrollment, in order, "" for a call with no proof.
func (b *loginBroker) rereadEnrollments() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.rereads...)
}

// proofEnrollment reads the enrollment a request's session proof names (its `eid` claim).
func proofEnrollment(t *testing.T, r *http.Request) string {
	t.Helper()
	parts := strings.Split(r.Header.Get("Proof"), ".")
	if len(parts) != 3 {
		t.Fatalf("the request carries no session proof: %q", r.Header.Get("Proof"))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode the proof's claims: %v", err)
	}
	var claims struct {
		EnrollmentID string `json:"eid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("read the proof's claims: %v", err)
	}
	return claims.EnrollmentID
}

func (b *loginBroker) tradedSessions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sessions...)
}

func writeLoginJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// userCodeLine is how login tells the person what to type, which the test reads to approve the
// code as that person would.
var userCodeLine = regexp.MustCompile(`type the code (\S+)`)

// approvingWriter approves the device code the moment login prints it, standing in for the
// person at the keyboard.
type approvingWriter struct {
	t      *testing.T
	issuer *oidctest.Issuer
	buf    bytes.Buffer
}

func (w *approvingWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if match := userCodeLine.FindStringSubmatch(w.buf.String()); match != nil {
		if approveErr := w.issuer.ApproveDeviceCode(match[1]); approveErr != nil {
			w.t.Errorf("the person types the code in: %v", approveErr)
		}
		w.buf.Reset()
	}
	return n, err
}

// forceMetadataMachine makes this machine's identity, for the rest of t, the Google identity
// token a metadata server of the test's own mints (AGENT_SECRETS_MACHINE_IDENTITY=metadata), the
// actor a person's sign-in presents.
func forceMetadataMachine(t *testing.T) {
	t.Helper()
	t.Setenv(cipherclient.IdentityVar, cipherclient.IdentityMetadata)
	t.Setenv(cipherclient.MachineAudienceVar, "agent-secrets-vm")
	startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
}

// TestLoginTradesUnderThePersonsOwnSessionName: login's first trade is the one the broker binds
// the name to, so it must ask under cipherclient.SessionFor and never under
// AGENT_SECRETS_SESSION — which is unset on most boxes, and on the rest is the name this machine
// trades its own identity under.
func TestLoginTradesUnderThePersonsOwnSessionName(t *testing.T) {
	const person = "ada@example.com"
	for _, machineSession := range []string{"", "machine-of-this-box"} {
		name := "with AGENT_SECRETS_SESSION unset"
		if machineSession != "" {
			name = "with AGENT_SECRETS_SESSION naming this machine's own session"
		}
		t.Run(name, func(t *testing.T) {
			broker := newLoginBroker(t)
			issuer := loginIssuer(t, person)
			keyDir := t.TempDir()
			t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
			t.Setenv(cipherclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
			t.Setenv(cipherclient.BrokerURLVar, broker.server.URL)
			t.Setenv(cipherclient.SessionVar, machineSession)
			forceMetadataMachine(t)

			stderr := &approvingWriter{t: t, issuer: issuer}
			if err := login(context.Background(), io.Discard, stderr); err != nil {
				t.Fatalf("login: %v", err)
			}

			records, err := cipherclient.SignIns(os.Getenv).List()
			if err != nil || len(records) != 1 {
				t.Fatalf("the sign-ins login kept = %+v, %v; want the one it just made", records, err)
			}
			want := cipherclient.SessionFor(records[0])
			traded := broker.tradedSessions()
			if len(traded) != 1 || traded[0] != want {
				t.Fatalf("login traded under %q; want %q, the name every later trade of this sign-in asks under", traded, want)
			}
			if machineSession != "" && strings.Contains(traded[0], machineSession) {
				t.Fatalf("login traded under %q, which carries this machine's own session name: that name belongs to google:<account>", traded[0])
			}
		})
	}
}

// TestASignInWhoseLeaseLapsedRegistersItselfAgain: nothing renews a sign-in's enrollment, so
// every one of them ends after BROKER_LEASE_SECONDS. Before this, the record kept the dead id
// and every `agent-secrets NAME -- cmd`, `request` and `status` on the box failed until
// somebody ran `agent-secrets login` again — which made a second box key and a second sign-in
// on that person's own list. The box registers the session again under the same box key
// instead, once, and writes the fresh id into the record it already has.
func TestASignInWhoseLeaseLapsedRegistersItselfAgain(t *testing.T) {
	const person = "ada@example.com"
	broker := newLoginBroker(t)
	issuer := loginIssuer(t, person)
	keyDir := t.TempDir()
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv(cipherclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
	t.Setenv(cipherclient.BrokerURLVar, broker.server.URL)
	t.Setenv(cipherclient.SessionVar, "")
	forceMetadataMachine(t)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "absent.sock"))

	if err := login(context.Background(), io.Discard, &approvingWriter{t: t, issuer: issuer}); err != nil {
		t.Fatalf("login: %v", err)
	}
	store := cipherclient.SignIns(os.Getenv)
	first, err := store.Choose("")
	if err != nil {
		t.Fatalf("the sign-in login kept: %v", err)
	}
	broker.lapse()

	_, signer, err := selectSigner(keyDir)
	if err != nil {
		t.Fatalf("selectSigner after the lease lapsed: %v", err)
	}
	if _, _, err := newClient(cipheraddr.New(broker.server.URL)).Self(context.Background(), signer); err != nil {
		t.Fatalf("a call on a lapsed sign-in = %v; want the session registered again and the call answered", err)
	}

	kept, err := store.Choose("")
	if err != nil {
		t.Fatalf("the sign-in after the re-registration: %v", err)
	}
	switch {
	case kept.EnrollmentID == first.EnrollmentID:
		t.Fatalf("the record still names %s, the enrollment the broker no longer holds", kept.EnrollmentID)
	case kept.EnrollmentID != broker.liveEnrollment():
		t.Fatalf("the record names %s; want %s, the enrollment the broker now holds", kept.EnrollmentID, broker.liveEnrollment())
	case kept.Thumbprint != first.Thumbprint:
		t.Fatalf("the box key changed from %s to %s; want the same key, so the person's list keeps one sign-in for this box", first.Thumbprint, kept.Thumbprint)
	}
	if records, err := store.List(); err != nil || len(records) != 1 {
		t.Fatalf("the box holds %+v, %v; want the one sign-in it started with", records, err)
	}
}

// loginIssuer is the device-flow issuer this machine is told to sign people in at, with the
// client's own secret in a file as the deployment gives it.
func loginIssuer(t *testing.T, person string) *oidctest.Issuer {
	t.Helper()
	const deviceStem = "example-device-"
	const clientID, clientSecret = deviceStem + "client", deviceStem + "secret"
	issuer := oidctest.New(t)
	issuer.EnableDeviceFlow(issuer.PublishKey(t, "google-key"), clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{"sub": person, "email": person, "email_verified": true})
	t.Setenv(cipherclient.SignInIssuerVar, issuer.URL())
	t.Setenv(cipherclient.SignInClientIDVar, clientID)
	t.Setenv(cipherclient.SignInClientSecretFileVar, writeLoginFile(t, "client-secret", clientSecret))
	return issuer
}

// writeLoginFile writes content into a file of the test's own directory and answers its path.
func writeLoginFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoginsSessionNameIsTheOneEveryLaterTradeAsks pins the name itself: the person's address
// and the box key's thumbprint, which is what tells two boxes of one person apart, and which
// registerSignInSession and SignInAuthority.Current ask under too.
func TestLoginsSessionNameIsTheOneEveryLaterTradeAsks(t *testing.T) {
	record := cipherclient.SignInRecord{Person: "ada@example.com", Thumbprint: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}
	if got, want := cipherclient.SessionFor(record), "signin-ada@example.com-abcdefghijkl"; got != want {
		t.Fatalf("SessionFor = %q, want %q", got, want)
	}
	if _, err := url.ParseQuery("session=" + url.QueryEscape(cipherclient.SessionFor(record))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(cipherclient.SessionFor(record)), " ") {
		t.Fatal("the session name carries a space, which no form field of a trade may")
	}
}

// TestASignInsFailedTradeNamesOnlyTheCureOfItsClass: a kept sign-in whose session lapsed trades
// again on the next call (registerSignInSession), and a trade the broker refuses tells the person
// only what cures that refusal, in the broker's own words and nothing added: a sign-in the broker
// revoked names `agent-secrets login` once, as the broker words it; a machine the deployment does
// not list names no login, which would not list it; a person Dispatch holds names no login, since
// a new sign-in on the box makes a second box key and changes nothing; and a broker that answers
// 503 names none either, since the next trade may be served.
func TestASignInsFailedTradeNamesOnlyTheCureOfItsClass(t *testing.T) {
	const held = "ada@example.com's credentials are held by Dispatch; they work again when ada@example.com signs in to Dispatch, or when Dispatch can confirm ada@example.com with Google again"
	for _, tc := range []struct {
		name   string
		status int
		body   map[string]any
		logins int // how often the error names agent-secrets login, in any form
	}{
		// The broker's words for a revoked sign-in (internal/cipher/oauth, Provider.personSignIn).
		{"a sign-in the broker revoked", http.StatusBadRequest, map[string]any{"error": "invalid_grant",
			"error_description": "ada@example.com revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with `agent-secrets login`."}, 1},
		// The broker's words for a machine no entry lists (internal/cipher/exchange, Set.Person),
		// as the token endpoint answers them.
		{"a machine the deployment does not list", http.StatusBadRequest, map[string]any{"error": "invalid_request",
			"error_description": "The subject token was refused: no entry lists the Google account box@fleet.iam.gserviceaccount.com, which the actor token names as the machine."}, 0},
		{"a person Dispatch holds", http.StatusBadRequest, map[string]any{"error": "membership_held", "error_description": held}, 0},
		{"a broker that answers 503", http.StatusServiceUnavailable, map[string]any{"error": "unavailable"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const person = "ada@example.com"
			broker := newLoginBroker(t)
			issuer := loginIssuer(t, person)
			keyDir := t.TempDir()
			t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
			t.Setenv(cipherclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
			t.Setenv(cipherclient.BrokerURLVar, broker.server.URL)
			t.Setenv(cipherclient.SessionVar, "")
			forceMetadataMachine(t)
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "absent.sock"))
			if err := login(context.Background(), io.Discard, &approvingWriter{t: t, issuer: issuer}); err != nil {
				t.Fatalf("login: %v", err)
			}
			broker.lapse()
			broker.refuseTrades(tc.status, tc.body)

			_, signer, err := selectSigner(keyDir)
			if err == nil {
				_, _, err = newClient(cipheraddr.New(broker.server.URL)).Self(context.Background(), signer)
			}
			if err == nil {
				t.Fatal("a call whose trade the broker refused succeeded")
			}
			got := err.Error()
			if told := strings.Count(got, cipherclient.SignInCommand); told != tc.logins {
				t.Fatalf("the error reads %q; it names %s %d times, want %d", got, cipherclient.SignInCommand, told, tc.logins)
			}
			if strings.Contains(got, "run: ") {
				t.Fatalf("the error reads %q; want the broker's words and no hint added to them", got)
			}
			if tc.body["error_description"] != nil && !strings.Contains(got, tc.body["error_description"].(string)) {
				t.Errorf("the error reads %q; want the broker's own words %q", got, tc.body["error_description"])
			}
		})
	}
}

// TestLoginFindsTheMachineWithNoSetting: a person signs in on a machine whose identity nothing
// configures, and `agent-secrets login` finds it from the broker's exchange audience: the trade
// carries the metadata server's identity token as its actor. A broker that names no exchange
// audience is refused naming that, since a sign-in has no machine to act on.
func TestLoginFindsTheMachineWithNoSetting(t *testing.T) {
	for _, row := range []struct {
		name     string
		audience string
		exit     int
	}{
		{"the broker names its audience", "agent-secrets-vm", 0},
		{"the broker names none", "", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			broker := newLoginBroker(t)
			broker.settings = storeSettings("gcp", row.audience)
			issuer := loginIssuer(t, "ada@example.com")
			metadata := startMetadataServer(t, "machine@example.iam.gserviceaccount.com")

			t.Setenv("HOME", t.TempDir())
			t.Setenv(cipherclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
			t.Setenv(cipherclient.BrokerURLVar, broker.server.URL)
			t.Setenv(cipherclient.SessionVar, "")
			t.Setenv(cipherclient.IdentityVar, "")
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
			t.Cleanup(func() { clientConfig = nil })

			var stdout bytes.Buffer
			stderr := &approvingWriter{t: t, issuer: issuer}
			code := run([]string{"login"}, &stdout, stderr)
			if code != row.exit {
				t.Fatalf("login: exit %d, want %d (stderr %q)", code, row.exit, stderr.buf.String())
			}
			broker.mu.Lock()
			forms := slices.Clone(broker.forms)
			broker.mu.Unlock()
			if row.exit == 1 {
				if !strings.Contains(stderr.buf.String(), "names no exchange audience") || len(forms) != 0 {
					t.Fatalf("stderr %q, %d trades; want the refusal naming the broker's missing exchange audience, and no trade", stderr.buf.String(), len(forms))
				}
				if n := metadata.totalRequests(); n != 0 {
					t.Fatalf("the metadata server saw %d requests; a broker naming no audience asks it nothing", n)
				}
				return
			}
			if len(forms) != 1 || forms[0].Get("actor_token") != "metadata-identity-token" || forms[0].Get("actor_token_type") != oauthwire.ActorTokenTypeMachine {
				t.Fatalf("the trades = %v; want one whose actor is the metadata server's identity token", forms)
			}
		})
	}
}
