//go:build linux

package helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnAcceptedAnswerThatDoesNotDecodePrintsNoProofItEchoes: an answer in the broker's place takes
// the identity's enrolment and its renewal, 200, and echoes the request's Proof header into
// lease_expires_at, which the helper decodes as a time. time.Time's error, and encoding/json's,
// quote the value; the helper's error names the member that did not decode and holds no part of
// the proof: Broker.do reads the answer through redact.ReadAnswer, whose Decode quotes no value.
// Captures helper.decode-echo/<call>.
func TestAnAcceptedAnswerThatDoesNotDecodePrintsNoProofItEchoes(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proofSent := r.Header.Get("Proof")
		mu.Lock()
		sent = append(sent, proofSent)
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"enrollment_id": "enr-1", "lease_expires_at": "Proof " + proofSent})
	}))
	t.Cleanup(srv.Close)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signIns := &stubSignIns{authority: &cipherclient.Authority{Key: key, CredentialID: "credential-1", Operator: "ada@example.com"}}
	b := &Broker{URL: cipheraddr.New(srv.URL), HTTP: srv.Client(), SignIns: signIns}
	t.Cleanup(b.Close)
	sess, err := newSession(os.Getpid(), 1, "example-host:1234:77", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, enrollErr := b.EnrollIdentity(context.Background(), sess.identity())
	_, renewErr := b.renewIdentity(context.Background(), identity{EnrollmentID: "enr-1", Key: key})
	mu.Lock()
	proofs := append([]string(nil), sent...)
	mu.Unlock()
	if len(proofs) == 0 || len(proofs[0]) < 16 {
		t.Fatalf("the control: the stand-in saw no Proof header (%q)", proofs)
	}
	for call, err := range map[string]error{"enrol": enrollErr, "renew": renewErr} {
		if err == nil {
			t.Fatalf("%s took an answer whose lease_expires_at is no time", call)
		}
		capture := "helper.decode-echo/" + call
		redacttest.Capture(t, capture, err.Error())
		for _, p := range proofs {
			if p != "" && strings.Contains(err.Error(), p[:16]) {
				t.Errorf("REDACT-ASSERT %s: the output prints the proof its request carried: %q", capture, err.Error())
				break
			}
		}
		if !strings.Contains(err.Error(), "lease_expires_at") {
			t.Errorf("%s's error is %q, want it to name the member that did not decode", call, err)
		}
	}
}
