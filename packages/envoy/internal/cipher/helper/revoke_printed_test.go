// packages/envoy/internal/cipher/helper/revoke_printed_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// TestARevokeThatCannotReachTheBrokerPrintsNoID: the id of a revoke ends its request URL, and for
// `agent-secrets unenroll --enrollment` it is what the caller typed, which the broker never
// accepted. A clean broker address that does not answer makes net/http's error quote that URL
// whole, so the failure names the route with the id left out. It still unwraps to the refused
// connection. Control: the address itself and the route still print, and so does the system's
// reason.
func TestARevokeThatCannotReachTheBrokerPrintsNoID(t *testing.T) {
	t.Parallel()
	const typed = "hunter2pw-enrollment-id-7b31"
	closed := closedBroker(t)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{URL: cipheraddr.New(closed)}
	t.Cleanup(b.Close)
	err = b.revokeUnder(context.Background(), &cipherclient.Authority{Key: key, CredentialID: "cred-1"}, nil, typed)
	if err == nil {
		t.Fatal("the revoke reached a broker that is not there")
	}
	wantNamedNotPrinted(t, "the revoke's error", err.Error(), []string{closed, "/v1/enrollments/", "connection refused"}, typed)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("the revoke's error = %v, want it to unwrap to the refused connection", err)
	}
}

// TestARevokeAnsweredWithBrokenHTTPPrintsNoID: a broker, or something between it and the helper,
// that answers a revoke with a status line HTTP cannot read makes net/http's error quote the
// request URL, the typed id in it, and the broker client's own error names that URL as Call
// prints it. The revoke's route, with the id left out, is all that prints of it: the class of
// the failure under it must not carry the URL back. Control: the route and the class print.
func TestARevokeAnsweredWithBrokenHTTPPrintsNoID(t *testing.T) {
	t.Parallel()
	const typed = "hunter2pw-typed-enrollment-tn7"
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	address := brokenHTTPBroker(t)
	b := &Broker{URL: cipheraddr.New(address)}
	t.Cleanup(b.Close)
	err = b.revokeUnder(context.Background(), &cipherclient.Authority{Key: key, CredentialID: "cred-1"}, nil, typed)
	if err == nil {
		t.Fatal("the revoke took a status line HTTP cannot read")
	}
	wantNamedNotPrinted(t, "the revoke's error", err.Error(), []string{address, "/v1/enrollments/", "the request failed"}, typed)
}

// TestNoLineMadeFromARevokesFailurePrintsItsID: a revoke that failed names its route with the id
// left out (cipheraddr.Address.Named), and every line made from that failure keeps the id out:
// Error and Cause again beside the helper's own address, another clean one and one that carries
// a credential, Class, BrokerAnswer, Words, a wrap with %w, and the helper's relays of it, the
// unenroll-session-key answer (UNENROLL_FAILED, or UNENROLL_OUTCOME_UNKNOWN through
// mayHaveReachedCause). Error over the failure prints the failure as it is or its route line,
// never the URL beneath; Class prints the failure's class alone. It holds for a broker that
// refuses the connection and for one that answers a status line HTTP cannot read. Control: every
// line names the route and the class.
func TestNoLineMadeFromARevokesFailurePrintsItsID(t *testing.T) {
	t.Parallel()
	const typed = "hunter2pw-typed-walk-3c9"
	credentialed := cipheraddr.New("http://" + "walkuser" + ":" + "hunter2pwADDR" + "@127.0.0.1:1")
	for _, row := range []struct {
		name, address, class, code string
	}{
		{"a refused connection", closedBroker(t), "connection refused", CodeUnenrollFailed},
		{"a status line HTTP cannot read", brokenHTTPBroker(t), "the request failed", CodeUnenrollUnknown},
	} {
		t.Run(row.name, func(t *testing.T) {
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			authority := &cipherclient.Authority{Key: key, CredentialID: "cred-1"}
			b := &Broker{URL: cipheraddr.New(row.address), SignIns: &stubSignIns{authority: authority}}
			t.Cleanup(b.Close)
			failed := b.revokeUnder(context.Background(), authority, nil, typed)
			if failed == nil {
				t.Fatal("the revoke did not fail")
			}
			route := http.MethodDelete + " " + row.address + "/v1/enrollments/(the enrollment id, not printed): "
			wantNamedNotPrinted(t, "the revoke's error", failed.Error(), []string{route, row.class}, typed)
			addresses := map[string]cipheraddr.Address{
				"the helper's address":                 b.URL,
				"another clean address":                cipheraddr.New("https://other-broker.example"),
				"an address that carries a credential": credentialed,
			}
			for _, err := range []error{failed, fmt.Errorf("unenroll: %w", failed)} {
				printed := map[string]string{}
				for name, a := range addresses {
					line := a.Error(err).Error()
					if line != err.Error() && line != failed.Error() {
						t.Errorf("Error beside %s of %q = %q, want it as it is or its route line", name, err, line)
					}
					printed["Error beside "+name] = line
					printed["Cause beside "+name] = a.Cause(err).Error()
				}
				if class := redact.Class(err).Error(); class != row.class {
					t.Errorf("Class of %q = %q, want %q", err, class, row.class)
				}
				printed["Class"] = redact.Class(err).Error()
				if re, ok := redact.BrokerAnswer(err); ok {
					t.Errorf("BrokerAnswer of %q found %v", err, re)
				}
				for what, text := range printed {
					wantNamedNotPrinted(t, what, text, []string{row.class}, typed, "hunter2pwADDR")
					for name, a := range addresses {
						wantNamedNotPrinted(t, "Words beside "+name+" of "+what, a.Words(text), nil, typed, "hunter2pwADDR")
					}
				}
			}
			resp := (&Server{Broker: b}).unenrollSessionKey(context.Background(), typed)
			if resp.OK || resp.Code != row.code {
				t.Errorf("unenroll-session-key answered OK %t, code %q; want code %q", resp.OK, resp.Code, row.code)
			}
			wantNamedNotPrinted(t, "the unenroll-session-key answer", resp.Error, []string{route, row.class}, typed)
			wantNamedNotPrinted(t, "mayHaveReachedCause", mayHaveReachedCause(failed), []string{route, row.class}, typed)
		})
	}
}

// closedBroker is the address of a port nothing listens on: a connection to it is refused.
func closedBroker(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	listener.Close()
	return address
}

// brokenHTTPBroker is the address of a server that answers every request with a status line HTTP
// cannot read, so net/http's error quotes the request URL.
func brokenHTTPBroker(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf)
				_, _ = conn.Write([]byte("HTTP/1.1 not-a-status\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// TestAnAnswerUnderClassIsNotTheBrokersRefusal: oauthBrokerError makes a BrokerError, which
// prints an OAuth answer's code and description, only from the broker's own answer. An answer
// that redact.Class holds came from another server, and Class printed it as its class alone, so
// its description, which that server can fill with an echo of a credential, does not print here
// either. The broker's own refusal beside a clean address still prints its code and its words:
// the control.
func TestAnAnswerUnderClassIsNotTheBrokersRefusal(t *testing.T) {
	t.Parallel()
	const secret = "hunter2pw-answer-4d81"
	addr := cipheraddr.New("https://broker.example")
	foreign := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_client", ErrorDescription: "echo of " + secret,
	}
	err := oauthBrokerError(fmt.Errorf("renew the sign-in at X: %w", redact.Class(foreign)), addr)
	wantNamedNotPrinted(t, "the foreign answer", err.Error(), []string{"400"}, secret)
	var be *BrokerError
	if errors.As(err, &be) {
		t.Errorf("the foreign answer made the BrokerError %q, which is the broker's own refusal", be)
	}

	own := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_request", ErrorDescription: "LOGIN_PENDING: a login is already pending",
	}
	err = oauthBrokerError(own, addr)
	if !errors.As(err, &be) || err.Error() != "broker 400 LOGIN_PENDING: a login is already pending" {
		t.Errorf("the broker's own refusal = %q, want the BrokerError %q", err, "broker 400 LOGIN_PENDING: a login is already pending")
	}
}

// TestARefusalBesideAnAddressWithACredentialIsStillARefusal: beside an address that carries a
// credential, the broker's own OAuth refusal prints none of its words - whoever answers can repeat
// the credential there - yet it is still the broker's 4xx refusal, so the helper reports it as
// one (a BrokerError whose text is withheld) and not as a request whose outcome it does not know.
// Control: beside a clean address the same refusal prints its code and words.
func TestARefusalBesideAnAddressWithACredentialIsStillARefusal(t *testing.T) {
	t.Parallel()
	const secret = "hunter2pw-refusal-5e21"
	refusal := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_request", ErrorDescription: "LOGIN_PENDING: a login is already pending (echo " + secret + ")",
	}
	credentialed := cipheraddr.New("http://" + "tokuser9f3" + ":" + secret + "@broker.invalid")
	// settled is settle of err for a request that was sent whole, where only a refusal the helper
	// can read as the broker's 4xx is a known outcome.
	settled := func(err error) error {
		reach := &requestReach{}
		reach.sent.Store(true)
		return reach.settle(err)
	}
	err := settled(oauthBrokerError(refusal, credentialed))
	var be *BrokerError
	if !errors.As(err, &be) || be.Status != http.StatusBadRequest {
		t.Fatalf("the refusal beside an address with a credential = %v, want a BrokerError of status 400", err)
	}
	if outcomeUnknown(err) {
		t.Errorf("the refusal beside an address with a credential = %q, which reads as an unknown outcome", err)
	}
	wantNamedNotPrinted(t, "the refusal beside an address with a credential", err.Error(), []string{"broker 400: " + cipheraddr.WordsNotPrinted}, secret)
	if strings.Contains(err.Error(), "tokuser9f3") || strings.Contains(err.Error(), "LOGIN_PENDING") {
		t.Errorf("the refusal beside an address with a credential = %q, which prints the broker's words", err)
	}
	clean := cipheraddr.New("https://broker.example")
	if got := settled(oauthBrokerError(refusal, clean)); !strings.HasPrefix(got.Error(), "broker 400 LOGIN_PENDING: a login is already pending") {
		t.Errorf("the refusal beside a clean address = %q, want its code and words", got)
	}
	// An answer with an error_uri does not print whole even beside a clean address. It is still
	// the broker's refusal, of its status alone, and it names no unclean address it does not have.
	pointing := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_request", ErrorDescription: "see " + secret, ErrorURI: "https://broker.example/why?k=" + secret,
	}
	got := settled(oauthBrokerError(pointing, clean))
	if !errors.As(got, &be) || outcomeUnknown(got) || got.Error() != "broker 400" {
		t.Errorf("a refusal with an error_uri beside a clean address = %q (unknown outcome: %t), want \"broker 400\"", got, outcomeUnknown(got))
	}
}
