//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerErrorPrintsNoProofItsAnswerEchoes: an answer in the broker's place refuses the
// identity's renewal with words that echo the signed Proof header the request carried. The
// BrokerError's text holds none of it: Broker.do holds the answer's words with every credential
// its request carried redacted (redact.Carried, RemoteText.WithoutTokens), while its code, which
// the helper acts on, is the one the answer gave (the red team's probe of #4368 at db7e4a0226).
// Capture helper.proof-echo.
func TestABrokerErrorPrintsNoProofItsAnswerEchoes(t *testing.T) {
	var mu sync.Mutex
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = r.Header.Get("Proof")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_SLOT", "error": "the request carried Proof: " + r.Header.Get("Proof")})
	}))
	t.Cleanup(srv.Close)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signIns := &stubSignIns{authority: &brokerclient.Authority{Key: key, CredentialID: "credential-1", Operator: "ada@example.com"}}
	b := &Broker{URL: brokeraddr.New(srv.URL), HTTP: srv.Client(), SignIns: signIns}
	t.Cleanup(b.Close)
	_, err = b.renewIdentity(context.Background(), identity{EnrollmentID: "enrollment-1", Key: key})
	if err == nil {
		t.Fatal("the renewal answered no error")
	}
	mu.Lock()
	proofSent := sent
	mu.Unlock()
	if len(proofSent) < 16 {
		t.Fatalf("the control: the stand-in saw no Proof header (%q)", proofSent)
	}
	var refused *BrokerError
	if !errors.As(err, &refused) || !refused.HasCode("INVALID_SLOT") {
		t.Errorf("the renewal's error is %v, want a BrokerError with the code the answer gave", err)
	}
	redacttest.Capture(t, "helper.proof-echo", err.Error())
	if strings.Contains(err.Error(), proofSent) {
		t.Errorf("REDACT-ASSERT helper.proof-echo: the output prints the proof its request carried: %q", err.Error())
	}
}
