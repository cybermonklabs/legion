package cipher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARelayedAnswerCarriesNoTokenItsAnswerEchoes: an answer in the broker's place answers
// Dispatch's call 200, as data the page shows, with a body that echoes the account token the call
// carried. The body Dispatch relays to the page (do's answer, which the api's relayBrokerResponse
// writes as it is) holds no run of the token, for a read (pending) and a decision (approve), and
// the rest of the answer is relayed as it came. The twin shows the stand-in's 200 holds the token
// as it wrote it, so the rows can see one relayed. Captures dispatch-agentsecrets.answer-echo-ok/<case>.
func TestARelayedAnswerCarriesNoTokenItsAnswerEchoes(t *testing.T) {
	const token = "eyJhbGciOiJSUzI1NiJ9.not-a-secret-token.c2lnbmVk"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"state":"approved","note":"seen %s"}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(server.Close)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/pending", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		said, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(said), token) {
			t.Fatalf("the stand-in answered %s, which holds no token, so the rows could not see one relayed", said)
		}
	})
	for _, c := range []struct {
		name string
		call func(*Client) (json.RawMessage, error)
	}{
		{"pending", func(c *Client) (json.RawMessage, error) { return c.Pending(context.Background(), "sjawhar") }},
		{"approve", func(c *Client) (json.RawMessage, error) {
			return c.Approve(context.Background(), "req-1", Decision{Approver: "sjawhar"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, err := c.call(New(server.URL, tokenFile(t, token)))
			if err != nil {
				t.Fatalf("the call = %v, want the broker's 200 relayed", err)
			}
			redacttest.RequireNoPiece(t, "dispatch-agentsecrets.answer-echo-ok/"+c.name, string(body), token)
			if want := `{"state":"approved","note":"seen Bearer ` + redact.Redacted + `"}`; string(body) != want {
				t.Errorf("the relayed body is %s, want %s", body, want)
			}
		})
	}
}

// TestARelayedListKeepsTheWordsItsCallSent: the broker's pending list names back the words
// Dispatch's call sent it - the approver its session resolved and the states the list asks for -
// as the data of each row. The body Dispatch relays to the page is the broker's answer byte for
// byte: only Dispatch's account token is redacted from a 2xx, never the rest of the call's record,
// which would turn each row's "state":"pending" into "[redacted]".
func TestARelayedListKeepsTheWordsItsCallSent(t *testing.T) {
	const list = `{"requests":[{"id":"rec-1","state":"pending","approver":"sjawhar"},{"id":"rec-2","state":"expired","approver":"sjawhar"}]}`
	server, captured := serveCapturing(t, http.StatusOK, list)
	defer server.Close()
	body, err := New(server.URL, tokenFile(t, "account-token-0123456789")).Pending(context.Background(), "sjawhar")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if want := "/v1/pending?approver=sjawhar&state=pending&state=expired"; captured.Path != want {
		t.Fatalf("request = %s, want %s: the row needs the call to carry the words the list names back", captured.Path, want)
	}
	if string(body) != list {
		t.Errorf("the relayed body is %s, want the broker's answer as it came, %s", body, list)
	}
}
