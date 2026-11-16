// packages/envoy/internal/broker/helper/urlcredential_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// TestTheHelpersBrokerCallsFollowNoRedirect: every call the helper makes to the broker carries
// something a host the deployment never named must not receive - the refresh a refresh token, the
// device authorization a signed request object, a renew the session's proof - so a broker address
// answering 3xx gets that answer back, and the host it names receives no request at all: not the
// call again (307, 308), and not the bodiless GET net/http turns a POST into on a 301, 302 or 303
// either. The broker receives each call itself, once, with the client the helper is given and
// with net/http's default alike.
func TestTheHelpersBrokerCallsFollowNoRedirect(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		for name, httpClient := range map[string]*http.Client{"a supplied client": {Timeout: 5 * time.Second}, "the default client": nil} {
			t.Run(http.StatusText(status)+" with "+name, func(t *testing.T) {
				var elsewhere atomic.Int64
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					elsewhere.Add(1)
					w.WriteHeader(http.StatusBadRequest)
				}))
				t.Cleanup(target.Close)
				var mu sync.Mutex
				asked := map[string]int{}
				broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					asked[r.URL.Path]++
					mu.Unlock()
					http.Redirect(w, r, target.URL+r.URL.Path, status)
				}))
				t.Cleanup(broker.Close)
				key, err := proof.NewKey()
				if err != nil {
					t.Fatal(err)
				}
				b := &Broker{URL: brokeraddr.New(broker.URL), OperatorFile: operatorFile(t, "ada@example.com"), HTTP: httpClient}
				t.Cleanup(b.Close)
				ctx := context.Background()

				_, _, refreshErr := b.redeem(ctx, &machineCredential{key: key, id: "credential-1", refreshToken: "demo-refresh-not-a-secret", gone: make(chan struct{})})
				_, loginErr := b.Login(ctx, "helper-host")
				_, renewErr := b.renewIdentity(ctx, identity{EnrollmentID: "enrollment-1", Key: key})
				for call, err := range map[string]error{"the refresh": refreshErr, "the device authorization": loginErr, "the renew": renewErr} {
					if err == nil {
						t.Errorf("%s took the redirect as the broker's answer", call)
					}
				}
				if got := elsewhere.Load(); got != 0 {
					t.Errorf("the redirect's target received %d requests, want none (refresh: %v, login: %v, renew: %v)", got, refreshErr, loginErr, renewErr)
				}
				mu.Lock()
				defer mu.Unlock()
				for _, path := range []string{"/v1/oauth/token", "/v1/oauth/device_authorization", "/v1/enrollments/enrollment-1/renew"} {
					if asked[path] != 1 {
						t.Errorf("the broker received %d requests at %s, want the call once (all: %v)", asked[path], path, asked)
					}
				}
			})
		}
	}
}

// TestALoginRefusalPrintsNoCredentialItsDescriptionRepeats: the broker, or whoever answers at its
// address, refuses a machine login with a description that repeats the credential the configured
// address carries. Beside that address the redactor prints the refusal as its status alone, and
// the helper must not take the description back out from under it: no BrokerError, and no text
// of the error, holds any part of the credential. Beside a clean address the broker's own words
// are what a person acts on, and the refusal still carries its code and its description whole.
func TestALoginRefusalPrintsNoCredentialItsDescriptionRepeats(t *testing.T) {
	var asked atomic.Int64
	var description string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"` + description + `"}`))
	}))
	t.Cleanup(srv.Close)
	credentialed := strings.Replace(srv.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1)
	description = "REQUEST_INVALID: the request object names " + credentialed + ", which is not this broker"
	login := func(t *testing.T, address string) error {
		t.Helper()
		before := asked.Load()
		b := &Broker{URL: brokeraddr.New(address), OperatorFile: operatorFile(t, "ada@example.com"), HTTP: srv.Client()}
		t.Cleanup(b.Close)
		_, err := b.Login(context.Background(), "helper-host")
		if err == nil {
			t.Fatal("Login took the broker's refusal as a login")
		}
		if asked.Load() == before {
			t.Fatalf("the login never reached the broker (Login: %v)", err)
		}
		return err
	}

	t.Run("an address carrying a credential", func(t *testing.T) {
		err := login(t, credentialed)
		for _, marker := range []string{"demouser7q", "demopass9z-not-a-secret"} {
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the refusal holds %q: %s", marker, err)
			}
			var be *BrokerError
			if errors.As(err, &be) && (strings.Contains(be.Code, marker) || strings.Contains(be.Message, marker)) {
				t.Errorf("the refusal's BrokerError holds %q: %+v", marker, be)
			}
		}
		if err.Error() != "the broker answered HTTP 400" {
			t.Errorf("the refusal is %q, want the status the broker answered", err)
		}
	})

	t.Run("a clean address", func(t *testing.T) {
		err := login(t, srv.URL)
		var be *BrokerError
		if !errors.As(err, &be) {
			t.Fatalf("Login: %v, want the broker's refusal as a BrokerError", err)
		}
		_, words, _ := strings.Cut(description, ": ")
		if be.Status != http.StatusBadRequest || be.Code != "REQUEST_INVALID" || be.Message != words {
			t.Errorf("the refusal is %+v, want 400 REQUEST_INVALID and the broker's description whole, %q", be, words)
		}
	})
}

// TestAnEnrollRefusalPrintsNoCredentialItsWordsRepeat is the enrollment route's
// TestALoginRefusalPrintsNoCredentialItsDescriptionRepeats: the broker, or whoever answers at its
// address, refuses a session's enrolment with words (the answer's error) that repeat the
// credential the configured address carries. The helper writes that refusal to its log and keeps
// it as the session's last error, which `sessions`, a register reply and a sign answer report.
// Beside that address none of them, and no BrokerError of the enrolment, the renew or the revoke,
// holds any part of the credential, and each refusal keeps the status and the code the helper
// acts on. Beside a clean address the broker's words are what a person acts on, and the refusal
// carries them whole.
func TestAnEnrollRefusalPrintsNoCredentialItsWordsRepeat(t *testing.T) {
	var asked atomic.Int64
	var words string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_SLOT", "error": words})
	}))
	t.Cleanup(srv.Close)
	credentialed := strings.Replace(srv.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1)
	words = "the slot names " + credentialed + ", which is not this broker"
	markers := []string{"demouser7q", "demopass9z-not-a-secret"}
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signIns := &stubSignIns{authority: &brokerclient.Authority{Key: key, CredentialID: "credential-1", Operator: "ada@example.com"}}

	// refusal is what the helper makes of an enrolment the broker at address refuses: the
	// enrolment's error, and the log and the session's last error enrollLoop writes from it.
	type refusal struct {
		err       error
		log       string
		lastError string
	}
	enroll := func(t *testing.T, address string) refusal {
		t.Helper()
		var out syncBuffer
		log := slog.New(slog.NewJSONHandler(&out, nil))
		s := &Server{
			Registry: NewRegistry(filepath.Join(t.TempDir(), "sessions.json")),
			Broker:   &Broker{URL: brokeraddr.New(address), HTTP: srv.Client(), Log: log, SignIns: signIns},
			Hostname: "testhost", Log: log, MinRenew: time.Second,
		}
		t.Cleanup(s.Broker.Close)
		sess, err := newSession(os.Getpid(), 1, "example-host:1234:77", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := asked.Load()
		_, _, enrollErr := s.Broker.EnrollIdentity(context.Background(), sess.identity())
		if enrollErr == nil {
			t.Fatal("the enrolment took the broker's refusal as an enrollment")
		}
		if asked.Load() == before {
			t.Fatalf("the enrolment never reached the broker (EnrollIdentity: %v)", enrollErr)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.enrollLoop(ctx, sess)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
		waitFor(t, func() bool { return strings.Contains(out.String(), "enroll failed; retrying") })
		return refusal{err: enrollErr, log: out.String(), lastError: sess.snapshot().LastError}
	}

	t.Run("an address carrying a credential", func(t *testing.T) {
		got := enroll(t, credentialed)
		const want = "broker 400 INVALID_SLOT"
		for _, marker := range markers {
			for what, text := range map[string]string{"the enrolment's error": got.err.Error(), "the helper's log": got.log, "the session's last error": got.lastError} {
				if strings.Contains(text, marker) {
					t.Errorf("%s holds %q: %s", what, marker, text)
				}
			}
		}
		var be *BrokerError
		if !errors.As(got.err, &be) || be.Status != http.StatusBadRequest || be.Code != "INVALID_SLOT" {
			t.Errorf("the refusal is %v, want the broker's 400 INVALID_SLOT as a BrokerError", got.err)
		}
		if got.err.Error() != want || got.lastError != want || !strings.Contains(got.log, `"error":"`+want+`"`) {
			t.Errorf("the refusal reads %q, the session's last error %q and the log %s; want each to be %q", got.err, got.lastError, got.log, want)
		}
	})

	t.Run("the renew and the revoke beside that address", func(t *testing.T) {
		b := &Broker{URL: brokeraddr.New(credentialed), HTTP: srv.Client(), SignIns: signIns}
		t.Cleanup(b.Close)
		_, renewErr := b.renewIdentity(context.Background(), identity{EnrollmentID: "enrollment-1", Key: key})
		revokeErr := b.Revoke(context.Background(), "enrollment-1")
		for call, err := range map[string]error{"the renew": renewErr, "the revoke": revokeErr} {
			var be *BrokerError
			if !errors.As(err, &be) || be.Status != http.StatusBadRequest || be.Code != "INVALID_SLOT" || be.Message != "" {
				t.Errorf("%s answered %v, want the broker's 400 INVALID_SLOT with no words of the broker's", call, err)
			}
			for _, marker := range markers {
				if err != nil && strings.Contains(err.Error(), marker) {
					t.Errorf("%s's error holds %q: %s", call, marker, err)
				}
			}
		}
	})

	t.Run("a clean address", func(t *testing.T) {
		got := enroll(t, srv.URL)
		var be *BrokerError
		if !errors.As(got.err, &be) {
			t.Fatalf("EnrollIdentity: %v, want the broker's refusal as a BrokerError", got.err)
		}
		if be.Status != http.StatusBadRequest || be.Code != "INVALID_SLOT" || be.Message != words {
			t.Errorf("the refusal is %+v, want 400 INVALID_SLOT and the broker's words whole, %q", be, words)
		}
		if want := "broker 400 INVALID_SLOT: " + words; got.lastError != want || !strings.Contains(got.log, want) {
			t.Errorf("the session's last error is %q and the log %s; want both to carry %q", got.lastError, got.log, want)
		}
	})
}

// TestAnOlderBrokersNotLiveVerdictHoldsBesideACredentialedAddress: an older broker says that an
// enrollment ended as PROOF_INVALID with "not live" in its words (enrollmentNotLive). Beside an
// address that carries a credential the helper withholds those words, and the verdict holds all
// the same, so such a session enrols again rather than renewing an ended enrollment for ever. A
// PROOF_INVALID that does not say it, a skewed clock's, stays a proof failure beside either.
func TestAnOlderBrokersNotLiveVerdictHoldsBesideACredentialedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		words := "iat outside the proof skew"
		if strings.HasPrefix(r.URL.Path, "/v1/enrollments/ended/") {
			words = "enrollment is not live"
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "PROOF_INVALID", "error": words})
	}))
	t.Cleanup(srv.Close)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	for name, address := range map[string]string{
		"a clean address":                  srv.URL,
		"an address carrying a credential": strings.Replace(srv.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1),
	} {
		t.Run(name, func(t *testing.T) {
			b := &Broker{URL: brokeraddr.New(address), HTTP: srv.Client()}
			t.Cleanup(b.Close)
			for id, wantEnded := range map[string]bool{"ended": true, "skewed": false} {
				_, err := b.renewIdentity(context.Background(), identity{EnrollmentID: id, Key: key})
				if code, ended := enrollmentNotLive(err); code != "PROOF_INVALID" || ended != wantEnded {
					t.Errorf("the renew of %s answered %v, read as %s ended %v; want PROOF_INVALID ended %v", id, err, code, ended, wantEnded)
				}
			}
		})
	}
}
