// packages/envoy/cmd/cipher/urlcredential_test.go

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestSelfPrintsNoPartOfTheBrokerURLCredential: `agent-secrets self` on a machine that proves an
// identity of its own (here the metadata server's, a stand-in) trades it at the broker before
// anything else, and with the broker unreachable prints that trade's failure on stderr. The line
// names the failure, and no part of a user name, password, query or fragment AGENT_SECRETS_URL
// carries. The values are made up.
func TestSelfPrintsNoPartOfTheBrokerURLCredential(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at urlcredential_test.go:36); it signals the test process or changes its signal handling (main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.run, main.loadClientConfig, clientConfig set at config.go:98).
	for form, address := range map[string]string{
		"a user name and password": "http://demouser7q:demopass9z-not-a-secret@127.0.0.1:1",
		"a query":                  "http://127.0.0.1:1/?token=demoquery4k",
		"a fragment":               "http://127.0.0.1:1#demofrag8w",
	} {
		t.Run(form, func(t *testing.T) {
			tmp := t.TempDir()
			startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			t.Setenv("HOME", tmp)
			t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
			t.Setenv("AGENT_SECRETS_SIGNIN_DIR", filepath.Join(tmp, "signins"))
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
			t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(tmp, "keys"))
			t.Setenv("AGENT_SECRETS_ENROLL_WAIT", "0s")
			t.Setenv("AGENT_SECRETS_URL", address)
			t.Setenv("AGENT_SECRETS_MACHINE_IDENTITY", "metadata")
			t.Setenv("AGENT_SECRETS_MACHINE_AUDIENCE", "demo-audience")
			var stdout, stderr bytes.Buffer
			if code := run([]string{"self"}, &stdout, &stderr); code == 0 {
				t.Fatalf("self against a broker that never answers exited 0\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "connection refused") {
				t.Errorf("stderr does not name the failure:\n%s", stderr.String())
			}
			for _, marker := range []string{"demouser7q", "demopass9z-not-a-secret", "demoquery4k", "demofrag8w"} {
				if strings.Contains(stdout.String()+stderr.String(), marker) {
					t.Errorf("the output holds %q\nstdout: %s\nstderr: %s", marker, stdout.String(), stderr.String())
				}
			}
		})
	}
}

// TestTheCLIsBrokerClientFollowsNoRedirect: every call this command makes to the broker carries a
// Proof header of this session's own, so a 3xx answered for the broker - by the broker, or by
// anyone who can answer for it - must reach no other host. The client answers the 3xx to its
// caller instead, and the target receives nothing.
func TestTheCLIsBrokerClientFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var targetCalls atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer target.Close()
			var brokerCalls atomic.Int64
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				brokerCalls.Add(1)
				http.Redirect(w, r, target.URL+"/v1/enrollments/self", status)
			}))
			defer broker.Close()

			c := newClient(cipheraddr.New(broker.URL))
			_, _, _ = c.doStatus(context.Background(), http.MethodGet, "/v1/enrollments/self", nil, map[string]string{"Proof": "the-session-proof"})
			if got := targetCalls.Load(); got != 0 {
				t.Errorf("the redirect's target received %d requests, want none", got)
			}
			if got := brokerCalls.Load(); got != 1 {
				t.Errorf("the broker received %d requests, want 1", got)
			}
		})
	}
}

// TestABrokerRefusalPrintsNoCredentialItsWordsRepeat: the broker, or whoever answers at its
// address, refuses a call in its error envelope with words and an answer id that repeat the
// credential the configured address carries. Beside that address the refusal prints its status
// and none of its words, its answer id or its code: no line on stderr and no text of the error
// holds any of them. Beside a clean address the broker's words, code and answer id are what a
// person acts on, and print. Whether a refusal's text is withheld is decided from the address
// where the refusal is built, and never read back from what the refusal says: a clean broker
// whose words are cipheraddr.WordsNotPrinted itself is a clean broker still, so its code and its
// answer id print on `agent-secrets status`'s stderr and in the client's error, and its code in a
// pod login's refusal line.
func TestABrokerRefusalPrintsNoCredentialItsWordsRepeat(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at urlcredential_test.go:127); it signals the test process or changes its signal handling (main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.run, main.loadClientConfig, clientConfig set at config.go:98).
	const answerID = "demopass9z-not-a-secret"
	var asked atomic.Int64
	var words string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/requests/req-1" {
			asked.Add(1)
		}
		body, _ := json.Marshal(map[string]string{"code": "REQUEST_INVALID", "error": words, "answer_id": answerID})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(answerIDHeader, answerID)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	credentialed := strings.Replace(srv.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1)
	words = "the request object names " + credentialed + ", which is not this broker"
	// status is `agent-secrets status req-1` against address: its stderr, and the refusal its
	// client reads.
	status := func(t *testing.T, address string) (string, error) {
		t.Helper()
		t.Setenv("AGENT_SECRETS_URL", address)
		t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
		before := asked.Load()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"status", "req-1"}, &stdout, &stderr); code != 1 {
			t.Fatalf("status against a broker that refuses it exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
		if asked.Load() == before {
			t.Fatalf("status never reached the broker\nstderr: %s", stderr.String())
		}
		_, err := newClient(cipheraddr.New(address)).do(context.Background(), http.MethodGet, "/v1/requests/req-1", nil, nil)
		if err == nil {
			t.Fatal("the client took the broker's refusal as an answer")
		}
		return stderr.String(), err
	}

	t.Run("an address carrying a credential", func(t *testing.T) {
		stderr, err := status(t, credentialed)
		printed := stderr + "\n" + err.Error() + "\n" + fmt.Sprintf("%+v %#v", err, err)
		for _, marker := range []string{"demouser7q", "demopass9z-not-a-secret"} {
			if strings.Contains(printed, marker) {
				t.Errorf("the refusal holds %q:\n%s", marker, printed)
			}
		}
		for what, text := range map[string]string{"stderr": stderr, "the error": err.Error()} {
			if strings.Contains(text, "REQUEST_INVALID") {
				t.Errorf("%s names the code the answerer wrote: %s", what, text)
			}
			if !strings.Contains(text, "HTTP 403") {
				t.Errorf("%s does not name the status: %s", what, text)
			}
		}
	})

	t.Run("a clean address", func(t *testing.T) {
		stderr, err := status(t, srv.URL)
		shown := "the request object names " + srv.URL + ", which is not this broker"
		for what, text := range map[string]string{"stderr": stderr, "the error": err.Error()} {
			for _, want := range []string{shown, "REQUEST_INVALID", answerID} {
				if !strings.Contains(text, want) {
					t.Errorf("%s does not carry the broker's %q: %s", what, want, text)
				}
			}
			if strings.Contains(text, "demouser7q") {
				t.Errorf("%s holds the user information the words' URL carries: %s", what, text)
			}
		}
	})

	t.Run("a clean address whose words are the marker", func(t *testing.T) {
		words = cipheraddr.WordsNotPrinted
		stderr, err := status(t, srv.URL)
		for what, text := range map[string]string{"stderr": stderr, "the error": err.Error()} {
			for _, want := range []string{"REQUEST_INVALID", answerID} {
				if !strings.Contains(text, want) {
					t.Errorf("%s does not carry the clean broker's %q: %s", what, want, text)
				}
			}
		}
		// status left AGENT_SECRETS_URL naming srv.URL, which answers the pod login's call too.
		tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
		var stdout, podStderr bytes.Buffer
		if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &podStderr); code != 1 {
			t.Fatalf("the pod login exited %d, want 1 (stderr %q)", code, podStderr.String())
		}
		if !strings.Contains(podStderr.String(), "HTTP 403 REQUEST_INVALID") {
			t.Errorf("the pod login's stderr %q does not name the clean broker's status and code", podStderr.String())
		}
	})
}

// TestTheRefusalsToldApartByTheirWordsHoldBesideACredentialedAddress: two refusals are told apart
// by the broker's words as well as their code - an older broker's 400 INVALID_NARROWING naming
// the release's command as an unknown field (callRefused), and its PROOF_INVALID saying an
// enrollment is not live (enrollmentNotLive). Beside an address that carries a credential the
// client prints none of those words, and each still reads as it does beside a clean one; the
// same codes with other words, another refusal of the release and a skewed clock's proof failure,
// read as neither, beside either address.
func TestTheRefusalsToldApartByTheirWordsHoldBesideACredentialedAddress(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answers := map[string]struct {
			status      int
			code, words string
		}{
			"/older-release": {http.StatusBadRequest, "INVALID_NARROWING", `body must be one valid JSON object: json: unknown field "command"`},
			"/other-refusal": {http.StatusBadRequest, "INVALID_NARROWING", "the release cannot be read"},
			"/ended":         {http.StatusUnauthorized, "PROOF_INVALID", "proof invalid: not live"},
			"/skewed":        {http.StatusUnauthorized, "PROOF_INVALID", "iat outside the proof skew"},
		}
		answer := answers[r.URL.Path]
		body, _ := json.Marshal(map[string]string{"code": answer.code, "error": answer.words})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	for name, address := range map[string]string{
		"a clean address":                  srv.URL,
		"an address carrying a credential": strings.Replace(srv.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1),
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(cipheraddr.New(address))
			refusal := func(path string) error {
				t.Helper()
				_, err := c.do(context.Background(), http.MethodPost, path, nil, nil)
				if err == nil {
					t.Fatalf("%s: the client took the broker's refusal as an answer", path)
				}
				return err
			}
			for path, want := range map[string]bool{"/older-release": true, "/other-refusal": false} {
				if err := refusal(path); callRefused(err) != want {
					t.Errorf("%s answered %v, read as an older broker's refusal of the command %v; want %v", path, err, !want, want)
				}
			}
			for path, want := range map[string]bool{"/ended": true, "/skewed": false} {
				if err := refusal(path); enrollmentNotLive(err) != want {
					t.Errorf("%s answered %v, read as an ended enrollment %v; want %v", path, err, !want, want)
				}
			}
		})
	}
}

// TestAnAudienceRefusalPrintsNoCredentialItsWordsRepeat: the machine source refuses a Google
// exchange audience other than the one this machine kept, and both audiences are text a broker's
// settings answer wrote. Beside an address that carries a credential the refusal prints neither
// of them, so an answer that repeats the credential prints none of it; beside a clean address it
// quotes both, as it did before the address was held as a cipheraddr.Address.
func TestAnAudienceRefusalPrintsNoCredentialItsWordsRepeat(t *testing.T) {
	t.Parallel()
	const audience = "//iam.googleapis.com/projects/1/demopass9z-not-a-secret"
	const kept = "//iam.googleapis.com/projects/1/the-kept-one"
	t.Run("an address carrying a credential", func(t *testing.T) {
		err := sameGoogleAudience(cipheraddr.New("https://demouser7q:demopass9z-not-a-secret@secrets.example"), audience, kept)
		if err == nil {
			t.Fatal("a changed audience was taken")
		}
		printed := err.Error() + "\n" + fmt.Sprintf("%+v %#v", err, err)
		for _, marker := range []string{"demouser7q", "demopass9z-not-a-secret", "the-kept-one"} {
			if strings.Contains(printed, marker) {
				t.Errorf("the refusal printed %q: %s", marker, printed)
			}
		}
		if !strings.Contains(err.Error(), cipheraddr.WordsNotPrinted) {
			t.Errorf("the refusal = %q; want %q in place of each audience", err, cipheraddr.WordsNotPrinted)
		}
	})
	t.Run("a clean address", func(t *testing.T) {
		err := sameGoogleAudience(cipheraddr.New("https://secrets.example"), audience, kept)
		if err == nil {
			t.Fatal("a changed audience was taken")
		}
		for _, want := range []string{strconv.Quote(audience), strconv.Quote(kept), "https://secrets.example"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal = %q; want it to name %s", err, want)
			}
		}
	})
}
