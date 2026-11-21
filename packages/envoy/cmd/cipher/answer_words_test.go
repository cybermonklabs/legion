// packages/envoy/cmd/cipher/answer_words_test.go
//
// What the forms print of a broker's 2xx answer, and what they send back of it, when whatever
// answers in the broker's place echoes the request it was sent: the Proof header, a pod's token,
// the bearer Google's userinfo read carries. Every member a line prints is held as
// redact.RemoteText, decoded with the record of the request it answers (redact.Answer.Decode), so
// it prints as the rule reads it with that request's credentials redacted; an id the form sends
// back goes back only where the rule prints it as it came (RemoteText.SendBack); and a key of the
// answer, which no record holds, prints only where it is a name this run asked for.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoProof is a member that echoes the Proof header its request carried beside the probe's
// credential URL, and shownEcho what the rule prints of it: the proof redacted, and the URL as the
// rule prints it.
func echoProof(proof string) string { return "proof " + proof + " refused; " + redacttest.Words }

const shownEcho = "proof " + redact.Redacted + " refused; " + redacttest.Shown

// launcherProof is the proof the stand-in helper signs the operator's routes with, GitHub-shaped,
// written in two parts so no line of this file reads as a credential to a scanner.
const launcherProof = "ghs_" + "0123456789abcdefghijklmnopqrstuvwxyz"

// echoBroker stands in for the broker: each route answers 200 with what the test makes of the
// Proof header the request carried, and any other route 404 (the settings call among them, so a
// source names the broker alone). It records each request's method and path, and each proof.
type echoBroker struct {
	mu     sync.Mutex
	asked  []string
	proofs []string
}

func newEchoBroker(t *testing.T, routes map[string]func(proof string) any) *echoBroker {
	t.Helper()
	b := &echoBroker{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proof := r.Header.Get("Proof")
		b.mu.Lock()
		b.asked = append(b.asked, r.Method+" "+r.URL.Path)
		if proof != "" {
			b.proofs = append(b.proofs, proof)
		}
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		for route, answer := range routes {
			method, path, _ := strings.Cut(route, " ")
			if r.Method == method && (r.URL.Path == path || strings.HasSuffix(path, "/") && strings.HasPrefix(r.URL.Path, path)) {
				_ = json.NewEncoder(w).Encode(answer(proof))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "error": "not found"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AGENT_SECRETS_URL", srv.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	t.Setenv("AGENT_SECRETS_WAIT", "")
	return b
}

// seen is every request the stand-in was sent, as "METHOD PATH", and every proof they carried.
func (b *echoBroker) seen() (asked, proofs []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.asked...), append([]string(nil), b.proofs...)
}

// requireNoneEchoed is a row's assertion on out, what the form printed: none of the probe's
// credentials (redacttest.RequireNone, captured as agent-secrets.answer-words/<capture>) and no run
// of 8 characters of any of carried, the credentials the form's requests carried, which the rule
// redacts; and each of shown, the member's place on its line, so the line still prints.
func requireNoneEchoed(t *testing.T, capture, out string, carried []string, shown ...string) {
	t.Helper()
	capture = "agent-secrets.answer-words/" + capture
	redacttest.RequireNone(t, capture, out)
	if len(carried) == 0 {
		t.Fatal("control: the stand-in saw no credential, so the row is blind")
	}
	for _, credential := range carried {
		if run := runOf(out, credential); run != "" {
			t.Errorf("REDACT-ASSERT %s: the output prints %q, a run of a credential its request carried: %q", capture, run, out)
		}
	}
	for _, place := range shown {
		if !strings.Contains(out, place) {
			t.Errorf("the output does not print %q, so the line no longer names the member's place: %q", place, out)
		}
	}
}

// runOf is the first run of 8 characters of credential that text holds, and "" for none.
func runOf(text, credential string) string {
	for at := 0; at+8 <= len(credential); at++ {
		if strings.Contains(text, credential[at:at+8]) {
			return credential[at : at+8]
		}
	}
	return ""
}

// TestABrokerAnswerPrintsNoCredentialItsRequestCarried: the request, status and self forms, the
// exec form, a --ttl the broker did not give, and the grant and machine tables, each run against a
// stand-in that answers 2xx with members that echo the Proof header the request carried. Each line
// prints each member as the rule reads it, the proof redacted (the member held as
// redact.RemoteText, decoded with the answer's record), and still names the member's place; no
// request the form makes afterwards carries an id that echoes a proof back (RemoteText.SendBack);
// and a key of the release, which no record holds, prints only where this run asked for it.
// Captures agent-secrets.answer-words/<case>.
func TestABrokerAnswerPrintsNoCredentialItsRequestCarried(t *testing.T) {
	now := time.Now().UTC()
	granted := func(string) any {
		return map[string]any{"request_id": "req-1", "state": "granted", "grant_id": "grant-1"}
	}
	// The airlock's values are placeholders (testAirlockPlaceholder), as the broker's are.
	awsName := map[string]any{
		"value": testAirlockPlaceholder, "hosts": []string{"*.amazonaws.com"},
		"slots": map[string]string{slotAWSAccessKeyID: testAirlockPlaceholder, slotAWSSecretAccessKey: testAirlockPlaceholder},
	}
	exec := func(args ...string) []string {
		return append(append([]string{"--reason", "need it"}, args...), "--", "true")
	}
	for _, row := range []struct {
		name     string
		routes   map[string]func(proof string) any
		args     []string
		code     int
		launcher bool
		shown    []string
	}{
		{"request", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": echoProof(p), "state": echoProof(p), "detail": echoProof(p)}
		}}, []string{"request", "--reason", "need it", "SOME_SECRET"}, 0, false,
			[]string{"request_id: " + shownEcho, "state: " + shownEcho, "detail: " + shownEcho}},
		{"request-pending", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": "req-1", "state": "pending", "approver": echoProof(p), "approve_url": echoProof(p)}
		}}, []string{"request", "--reason", "need it", "SOME_SECRET"}, exitPending, false,
			[]string{"waiting for " + shownEcho + " to approve it on the secrets broker: " + shownEcho}},
		{"status", map[string]func(string) any{"GET /v1/requests/": func(p string) any {
			return map[string]any{"state": echoProof(p), "grant_id": echoProof(p), "decision": map[string]any{"by": echoProof(p), "at": now}, "detail": echoProof(p)}
		}}, []string{"status", "req-1"}, 0, false,
			[]string{"state: " + shownEcho, "grant_id: " + shownEcho, "decided_by: " + shownEcho, "detail: " + shownEcho}},
		{"status-pending", map[string]func(string) any{"GET /v1/requests/": func(p string) any {
			return map[string]any{"state": "pending", "approver": echoProof(p), "approve_url": echoProof(p)}
		}}, []string{"status", "req-1"}, 0, false,
			[]string{"waiting for " + shownEcho + " to approve it on the secrets broker: " + shownEcho}},
		{"self", map[string]func(string) any{"GET /v1/enrollments/self": func(p string) any {
			return map[string]any{"enrollment_id": echoProof(p), "kind": echoProof(p), "operator": echoProof(p), "lease_expires_at": now,
				"grants": []any{map[string]any{"grant_id": echoProof(p), "request_id": echoProof(p)}}}
		}}, []string{"self"}, 0, false,
			[]string{"enrollment_id: " + shownEcho, "kind: " + shownEcho, "operator: " + shownEcho, "grant: " + shownEcho + " (request " + shownEcho + ", until this session ends)"}},
		{"exec-denied", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": echoProof(p), "state": "denied", "detail": echoProof(p)}
		}}, exec("SOME_SECRET"), exitDenied, false,
			[]string{"request " + shownEcho + " to ", "was denied: " + shownEcho}},
		{"exec-pending", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": echoProof(p), "state": "pending", "approver": echoProof(p), "approve_url": echoProof(p)}
		}}, exec("--wait", "0s", "SOME_SECRET"), exitPending, false,
			[]string{"request " + shownEcho + " to ", "waiting for " + shownEcho + " to approve it on the secrets broker: " + shownEcho, "Check it with: agent-secrets status " + shownEcho}},
		{"exec-state", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": "req-1", "state": "state " + p}
		}}, exec("SOME_SECRET"), 1, false,
			[]string{`is in unexpected state "state ` + redact.Redacted + `"`}},
		{"exec-handover", map[string]func(string) any{
			"POST /v1/requests": func(string) any {
				return map[string]any{"request_id": "req-1", "state": "pending", "judge_deciding": true}
			},
			"GET /v1/requests/": func(p string) any {
				return map[string]any{"state": "pending", "judge_deciding": false, "approver": echoProof(p), "approve_url": echoProof(p)}
			},
		}, exec("--wait", "1s", "SOME_SECRET"), exitPending, false,
			[]string{"the model judge could not decide it; waiting for " + shownEcho + " to approve it on the secrets broker: " + shownEcho}},
		{"exec-poll", map[string]func(string) any{
			"POST /v1/requests": func(p string) any { return map[string]any{"request_id": p, "state": "pending"} },
			"GET /v1/requests/": func(string) any { return map[string]any{"state": "pending"} },
		}, exec("--wait", "2s", "SOME_SECRET"), exitPending, false,
			[]string{"request " + redact.Redacted + " to ", "is not read again"}},
		{"exec-grant", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": "req-1", "state": "granted", "grant_id": p}
		}}, exec("SOME_SECRET"), 1, false,
			[]string{"grant " + redact.Redacted + " from ", "is not released"}},
		{"exec-narrowing", map[string]func(string) any{
			"POST /v1/requests": granted,
			"POST /v1/grants/": func(p string) any {
				return map[string]any{"values": map[string]string{"APP": "released-value"}, "github_app": map[string]any{"owner": echoProof(p)}}
			},
		}, exec("--github-owner", "ada", "APP"), 1, false,
			[]string{`it answered {"owner":"` + shownEcho + `"}`}},
		{"exec-airlock-names", map[string]func(string) any{
			"POST /v1/requests": granted,
			"POST /v1/grants/": func(p string) any {
				return map[string]any{"values": map[string]string{}, "airlock": map[string]any{
					"proxy_url": "http://airlock.broker.example:3128", "proxy_password": testAirlockPlaceholder,
					"ca_certificate": "-----BEGIN CERTIFICATE-----\nproxy\n-----END CERTIFICATE-----\n",
					"names":          map[string]any{"AWS_ONE": awsName, p: awsName},
				}}
			},
		}, exec("AWS_ONE"), 1, false,
			[]string{"for both AWS_ONE and a name this run did not ask for"}},
		{"exec-github-token", map[string]func(string) any{
			"POST /v1/requests": granted,
			"POST /v1/grants/": func(p string) any {
				return map[string]any{"values": map[string]string{"APP": "released-value", p: "another-value"}, "github_app": map[string]any{"owner": "ada"}}
			},
		}, exec("--github-owner", "ada", "--github-token", "APP"), 1, false,
			[]string{"it read APP, 1 name this run did not ask for"}},
		{"ttl-revoke", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": "req-1", "state": "granted", "grant_id": p}
		}}, []string{"request", "--reason", "need it", "--ttl", "60s", "SOME_SECRET"}, 1, false,
			[]string{"its grant " + redact.Redacted + " was not ended"}},
		{"ttl-cancel", map[string]func(string) any{"POST /v1/requests": func(p string) any {
			return map[string]any{"request_id": p, "state": "pending"}
		}}, []string{"request", "--reason", "need it", "--ttl", "60s", "SOME_SECRET"}, 1, false,
			[]string{"the broker gave request " + redact.Redacted + " no lifetime", "it was not cancelled"}},
		{"grant-list", map[string]func(string) any{"GET /v1/operator/grants": func(p string) any {
			return map[string]any{"grants": []any{map[string]any{
				"grant_id": echoProof(p), "granted": echoProof(p), "names": []string{echoProof(p)}, "approver": echoProof(p), "decided_by": echoProof(p),
				"enrollment": map[string]any{"kind": echoProof(p), "runtime_id": echoProof(p), "operator": echoProof(p), "slot": echoProof(p)},
				"created_at": now,
			}}}
		}}, []string{"grant", "list"}, 0, true,
			[]string{shownEcho}},
		{"machine-list", map[string]func(string) any{"GET /v1/operator/machines": func(p string) any {
			return map[string]any{"credentials": []any{map[string]any{
				"credential_id": echoProof(p), "host": echoProof(p), "approved_by": echoProof(p), "issued_at": now, "expires_at": now,
			}}}
		}}, []string{"machine", "list"}, 0, true,
			[]string{shownEcho}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			b := newEchoBroker(t, row.routes)
			if row.launcher {
				sock, _ := fakeHelperByOp(t, map[string]helper.Response{
					"sign-launcher": {OK: true, Proof: launcherProof, CredentialID: "operator-credential"},
				})
				t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
			}
			var stdout, stderr bytes.Buffer
			code := run(row.args, &stdout, &stderr)
			asked, proofs := b.seen()
			out := stdout.String() + stderr.String()
			requireNoneEchoed(t, row.name, out, proofs, row.shown...)
			if code != row.code {
				t.Errorf("exit %d, want %d: %q", code, row.code, out)
			}
			// An id that echoes a proof is sent back in no request's path.
			for _, request := range asked {
				for _, proof := range proofs {
					if run := runOf(request, proof); run != "" {
						t.Errorf("the form sent back an id that echoes a proof its request carried: %s", request)
					}
				}
			}
		})
	}
}

// TestAnEnrollmentIDTheBrokerEchoesIsNotKept: a sign-in's registration and a pod's enrolment take
// the ids the broker answers as the ids every later call of theirs signs with, and the pod's enroll
// prints its enrollment id on stdout for a script. Whatever answers in the broker's place echoes the
// request: the enrollment id echoes the launcher proof it was registered under, or the login's
// credential id echoes the pod's token. Neither id is kept, written or signed with: each prints
// nowhere, the form refuses saying the id is not printed, and a pod whose login credential is
// refused makes no enrolment under it (RemoteText.SendBack). Captures
// agent-secrets.answer-words/login, /pod-enrollment and /pod-credential.
func TestAnEnrollmentIDTheBrokerEchoesIsNotKept(t *testing.T) {
	t.Run("login", func(t *testing.T) {
		broker := newLoginBroker(t)
		var mu sync.Mutex
		var proofs []string
		broker.echoEnrollment = func(proof string) string {
			mu.Lock()
			defer mu.Unlock()
			proofs = append(proofs, proof)
			return proof
		}
		issuer := loginIssuer(t, "ada@example.com")
		t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
		signIns := filepath.Join(t.TempDir(), "signins")
		t.Setenv(cipherclient.SignInDirVar, signIns)
		t.Setenv(cipherclient.BrokerURLVar, broker.server.URL)
		t.Setenv(cipherclient.SessionVar, "")
		forceMetadataMachine(t)
		var stdout bytes.Buffer
		err := login(context.Background(), &stdout, &approvingWriter{t: t, issuer: issuer})
		out := stdout.String()
		if err != nil {
			out += err.Error()
		}
		mu.Lock()
		carried := append([]string(nil), proofs...)
		mu.Unlock()
		requireNoneEchoed(t, "login", out+keptText(t, signIns), carried, "enrollment id", "not printed")
		if err == nil {
			t.Error("login kept an enrollment id that echoes the proof it was registered under")
		}
	})
	podBroker := func(t *testing.T, credential func(token string) string, enrollment func(proof string) string) (*[]string, *[]string) {
		t.Helper()
		var mu sync.Mutex
		var carried, enrolments []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path == "/v1/launcher-credentials/token" {
				var login struct{ Token, Request string }
				_ = json.Unmarshal(body, &login)
				mu.Lock()
				carried = append(carried, login.Token, login.Request)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"credential_id": credential(login.Token), "expires_at": time.Now().Add(time.Hour)})
				return
			}
			proof := r.Header.Get("Proof")
			mu.Lock()
			carried = append(carried, proof)
			enrolments = append(enrolments, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"enrollment_id": enrollment(proof), "lease_expires_at": time.Now().Add(time.Hour)})
		}))
		t.Cleanup(srv.Close)
		t.Setenv("AGENT_SECRETS_URL", srv.URL)
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
		return &carried, &enrolments
	}
	for _, row := range []struct {
		name       string
		credential func(token string) string
		enrollment func(proof string) string
		enrols     bool
		shown      []string
	}{
		{"pod-enrollment", func(string) string { return "credential-for-the-pod" }, func(proof string) string { return "enrolled " + proof }, true,
			[]string{"enrollment_id", "not printed"}},
		{"pod-credential", func(token string) string { return "credential " + token }, func(string) string { return "enr-pod-1" }, false,
			[]string{"credential_id", "not printed"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
			out := t.TempDir()
			t.Setenv("AGENT_SECRETS_KEY_DIR", out)
			carried, enrolments := podBroker(t, row.credential, row.enrollment)
			var stdout, stderr bytes.Buffer
			code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr)
			requireNoneEchoed(t, row.name, stdout.String()+stderr.String()+keptText(t, out), *carried, row.shown...)
			if code == 0 {
				t.Errorf("enroll --pod took an id that echoes its request: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			if !row.enrols && len(*enrolments) > 0 {
				t.Errorf("enroll --pod enrolled under a credential id that echoes its token: %v", *enrolments)
			}
		})
	}
}

// keptText is every file under dir, read whole and joined: what a form kept of an answer.
func keptText(t *testing.T, dir string) string {
	t.Helper()
	var kept strings.Builder
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if data, readErr := os.ReadFile(path); readErr == nil {
				kept.Write(data)
			}
		}
		return nil
	})
	return kept.String()
}

// TestARereadAnswerPrintsNoProofItEchoes: a write's reread is a session's signed call, and whatever
// answers in the broker's place echoes its Proof header in the reason it refuses the secret for or
// the type it says it serves it as. The write's lines - the broker's line, the refusal after the
// write, a value's type that is not the one written, a retag's type the broker does not serve yet
// - print each as the rule reads it, the proof redacted (Reread holds them as redact.RemoteText,
// decoded with the answer's record). Captures agent-secrets.answer-words/<case>.
func TestARereadAnswerPrintsNoProofItEchoes(t *testing.T) {
	held := func() *secrets.Local {
		return secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	}
	login := func() *secrets.Local {
		return secrets.NewLocal(policytest.Secret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policytest.TypeValue(policy.TypeGoogleLogin)))
	}
	for _, row := range []struct {
		name   string
		local  func() *secrets.Local
		answer func(proof string) map[string]any
		args   []string
		shown  []string
	}{
		{"reread-reason", held, func(p string) map[string]any {
			return map[string]any{"name": "HELD_KEY", "served": false, "reason": echoProof(p)}
		},
			[]string{"set", "HELD_KEY"}, []string{"(reason=" + shownEcho + ")", "refuses HELD_KEY after the write"}},
		{"reread-type", held, func(p string) map[string]any {
			return map[string]any{"name": "HELD_KEY", "served": true, "type": echoProof(p)}
		},
			[]string{"set", "HELD_KEY"}, []string{"(type " + shownEcho + ")", "serves HELD_KEY as " + shownEcho + ", not a stored secret"}},
		{"retag-type", login, func(p string) map[string]any {
			return map[string]any{"name": "LOGIN_KEY", "served": true, "type": echoProof(p)}
		},
			[]string{"retag", "LOGIN_KEY", "--type", policy.TypeGoogleLogin}, []string{"serves LOGIN_KEY as " + shownEcho + ", not " + policy.TypeGoogleLogin}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var mu sync.Mutex
			var proofs []string
			settings := Settings{SecretsPrefix: policytest.Prefix, KMSKeyARN: policytest.KeyARN, AWSAccountID: testAccount, AWSRegion: "us-east-1", SecretTypeTag: true}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch proof := r.Header.Get("Proof"); {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/settings":
					writeJSON(w, settings)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reread") && proof != "":
					mu.Lock()
					proofs = append(proofs, proof)
					mu.Unlock()
					writeJSON(w, row.answer(proof))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reread"):
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"accepted":true}` + "\n"))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			t.Setenv("AGENT_SECRETS_URL", srv.URL)
			t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
			useAWS(t, row.local(), testAccount, adaSignIn)
			useStdin(t, "v2")
			stdout, stderr, code := runSecret(row.args...)
			mu.Lock()
			carried := append([]string(nil), proofs...)
			mu.Unlock()
			requireNoneEchoed(t, row.name, stdout+stderr, carried, row.shown...)
			if code != 1 {
				t.Errorf("exit %d, want 1: stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}

// TestTheUserinfoEmailPrintsNoTokenItsReadCarried: --owner me reads the person's email from Google's
// userinfo endpoint with the sign-in's bearer (gcpUserinfo), and whatever answers in Google's place
// echoes that bearer as the email, shaped as a service account's. personEmail's refusal prints
// none of it: the email is held as redact.RemoteText, decoded with the read's record, and taken as
// an email only where the rule prints it as it came (RemoteText.SendBack). Control: a person's
// email is still answered, lowercased. Captures agent-secrets.answer-words/userinfo-email.
func TestTheUserinfoEmailPrintsNoTokenItsReadCarried(t *testing.T) {
	for _, row := range []struct {
		name  string
		email func(bearer string) string
		want  string
	}{
		{"an email that echoes the bearer", func(bearer string) string { return bearer + "@demo.iam.gserviceaccount.com" }, ""},
		{"control: a person's email", func(string) string { return "Ada@Example.com" }, "ada@example.com"},
	} {
		t.Run(row.name, func(t *testing.T) {
			answer := answering(func(r *http.Request) *http.Response {
				bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				body, _ := json.Marshal(map[string]string{"email": row.email(bearer)})
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(bytes.NewReader(body)), Request: r}
			})
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: answer})
			b := &gcpBackend{
				creds:        &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: redacttest.Token, TokenType: "Bearer"})},
				sourcePhrase: "the sign-in under test",
			}
			email, err := b.personEmail(ctx)
			if row.want != "" {
				if err != nil || email != row.want {
					t.Fatalf("personEmail = %q, %v; want %q", email, err, row.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("personEmail took %q, an email that echoes the bearer its read carried", email)
			}
			requireNoneEchoed(t, "userinfo-email", err.Error(), []string{redacttest.Token}, "not printed")
		})
	}
}
