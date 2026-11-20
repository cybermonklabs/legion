// packages/envoy/cmd/agent-secrets/answer_decoders_test.go
//
// What this client's answer decoders print of an answer they cannot read. encoding/json's error
// can quote the answer it refused - a time field's value whole, through time.Parse's error - and
// whoever answers at an address that is not clean can repeat that address's credential in any
// text of its answer. So each decoder prints the decoder's words as the address lets the answer's
// own words print (brokeraddr.Address.Words): not at all beside a credentialed address, and on one
// line beside a clean one.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestTheAnswerDecodersPrintTheAnswerOnlyAsTheAddressLetsItPrint: every decoder of client.go,
// given an answer it cannot read, refuses it naming the call it decoded, and beside a credentialed
// address prints brokeraddr.WordsNotPrinted in place of the decoder's words. Where the answer's
// type has a time field, the answer puts the address's own password there (the echo a server that
// answers for a credentialed address can send), which encoding/json's error quotes whole; no part
// of it may print. Control: beside a clean address the same answer's decoder words still print,
// the echoed password among them where the type has a time field, so the check sees what a
// decoder prints.
func TestTheAnswerDecodersPrintTheAnswerOnlyAsTheAddressLetsItPrint(t *testing.T) {
	// Built from parts, so no line of this file reads as a credential.
	password := "hunter2pw-" + "decoders-5e1b"
	echo := `"` + password + `"`
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := &fileSigner{key: key, enrollmentID: "enr-test"}
	ctx := context.Background()
	decoders := []struct {
		call string
		// body is the answer: the password in a time field where the type has one, which
		// time.Parse's error quotes; else a JSON number, which the decoder refuses quoting
		// nothing of it.
		body string
		run  func(c *client) error
	}{
		{"decode create request response", `4815162342`, func(c *client) error {
			_, _, err := c.postRequest(ctx, signer, func(bool) ([]byte, error) { return []byte("{}"), nil })
			return err
		}},
		{"decode request status", `{"state":"granted","decided_at":` + echo + `}`, func(c *client) error {
			_, _, err := c.GetRequest(ctx, signer, "req-1", 0)
			return err
		}},
		{"decode grant values", `{"values":{},"expires_at":` + echo + `}`, func(c *client) error {
			_, err := c.GrantValues(ctx, signer, "grant-1", nil, releaseCall{})
			return err
		}},
		{"decode self", `{"enrollment_id":"enr-test","lease_expires_at":` + echo + `}`, func(c *client) error {
			_, _, err := c.Self(ctx, signer)
			return err
		}},
		{"decode renew response", `{"lease_expires_at":` + echo + `}`, func(c *client) error {
			_, err := c.RenewEnrollment(ctx, signer, "enr-test", "")
			return err
		}},
		{"decode settings", `4815162342`, func(c *client) error {
			_, err := c.Settings(ctx)
			return err
		}},
		{"decode reread", `4815162342`, func(c *client) error {
			_, err := c.RereadSecret(ctx, "SCRATCH_TEST")
			return err
		}},
		{"decode machine logins", `{"credentials":[{"credential_id":"lcred-1","issued_at":` + echo + `}]}`, func(c *client) error {
			_, _, err := c.OperatorMachines(ctx, signer)
			return err
		}},
		{"decode grants", `{"grants":[{"grant_id":"grant-1","created_at":` + echo + `}]}`, func(c *client) error {
			_, _, err := c.OperatorGrants(ctx, signer)
			return err
		}},
	}
	for _, decoder := range decoders {
		answer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(decoder.body))
		}))
		t.Cleanup(answer.Close)
		echoes := strings.Contains(decoder.body, password)
		t.Run(decoder.call+", beside a credentialed address", func(t *testing.T) {
			credentialed, err := url.Parse(answer.URL)
			if err != nil {
				t.Fatal(err)
			}
			credentialed.User = url.UserPassword("demouser7q", password)
			err = decoder.run(newClient(brokeraddr.New(credentialed.String())))
			if err == nil {
				t.Fatal("the answer was read")
			}
			wantNamedNotPrinted(t, "the error", err.Error(), []string{decoder.call + ": " + brokeraddr.WordsNotPrinted}, password, "demouser7q")
		})
		t.Run("control: "+decoder.call+", beside a clean address", func(t *testing.T) {
			err := decoder.run(newClient(brokeraddr.New(answer.URL)))
			if err == nil || !strings.Contains(err.Error(), decoder.call+": ") || strings.Contains(err.Error(), brokeraddr.WordsNotPrinted) {
				t.Fatalf("the error %v does not print the decoder's words beside a clean address", err)
			}
			if echoes && !strings.Contains(err.Error(), password) {
				t.Errorf("the error %v does not quote the echo the answer carries, so the check above could not see it", err)
			}
		})
	}
}
