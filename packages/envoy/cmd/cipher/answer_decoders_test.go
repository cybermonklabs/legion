// packages/envoy/cmd/cipher/answer_decoders_test.go
//
// What this client's answer decoders print of an answer they cannot read. encoding/json's error
// can quote the answer it refused - a time field's value whole, through time.Parse's error - and
// whoever answers in the broker's place can repeat a credential, the address's or the request's,
// in any value of its answer. So each decoder reads the answer through redact.Answer.Decode, whose
// error names the call and the member that did not decode and quotes no value, beside a
// credentialed address and a clean one alike.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
)

// TestTheAnswerDecodersPrintTheAnswerOnlyAsTheAddressLetsItPrint: every decoder of client.go,
// given an answer it cannot read, refuses it naming the call it decoded and the member that did
// not decode (redact.Answer.Decode), beside a credentialed address and a clean one alike. Where
// the answer's type has a time field, the answer puts the address's own password there (the echo
// a server that answers for a credentialed address can send), which encoding/json's error quotes
// whole; no part of it may print beside either address. Control: beside a clean address the
// error names the member the echo sat in, so the check reads the decoder's error for the value
// that held the echo, and still prints none of it.
func TestTheAnswerDecodersPrintTheAnswerOnlyAsTheAddressLetsItPrint(t *testing.T) {
	t.Parallel()
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
		// member is the member the decoder's error names: the one that did not decode, as the
		// answer's type names it at its depth, or "value" for an answer that is no object.
		member string
		run    func(c *client) error
	}{
		{"decode create request response", `4815162342`, "value", func(c *client) error {
			_, _, err := c.postRequest(ctx, signer, func(bool) ([]byte, error) { return []byte("{}"), nil })
			return err
		}},
		{"decode request status", `{"state":"granted","decided_at":` + echo + `}`, `"decided_at"`, func(c *client) error {
			_, _, err := c.GetRequest(ctx, signer, "req-1", 0)
			return err
		}},
		{"decode grant values", `{"values":{},"expires_at":` + echo + `}`, `"expires_at"`, func(c *client) error {
			_, err := c.GrantValues(ctx, signer, "grant-1", releaseCall{})
			return err
		}},
		{"decode self", `{"enrollment_id":"enr-test","lease_expires_at":` + echo + `}`, `"lease_expires_at"`, func(c *client) error {
			_, _, err := c.Self(ctx, signer)
			return err
		}},
		{"decode renew response", `{"lease_expires_at":` + echo + `}`, `"lease_expires_at"`, func(c *client) error {
			_, err := c.RenewEnrollment(ctx, signer, "enr-test", "")
			return err
		}},
		{"decode settings", `4815162342`, "value", func(c *client) error {
			_, err := c.Settings(ctx)
			return err
		}},
		{"decode reread", `4815162342`, "value", func(c *client) error {
			_, err := c.RereadSecret(ctx, signer, "SCRATCH_TEST")
			return err
		}},
		{"decode machine logins", `{"credentials":[{"credential_id":"lcred-1","issued_at":` + echo + `}]}`, `"credentials.(item).issued_at"`, func(c *client) error {
			_, _, err := c.OperatorMachines(ctx, signer)
			return err
		}},
		{"decode grants", `{"grants":[{"grant_id":"grant-1","created_at":` + echo + `}]}`, `"grants.(item).created_at"`, func(c *client) error {
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
		named := decoder.call + ": the answer's " + decoder.member
		t.Run(decoder.call+", beside a credentialed address", func(t *testing.T) {
			credentialed, err := url.Parse(answer.URL)
			if err != nil {
				t.Fatal(err)
			}
			credentialed.User = url.UserPassword("demouser7q", password)
			err = decoder.run(newClient(cipheraddr.New(credentialed.String())))
			if err == nil {
				t.Fatal("the answer was read")
			}
			wantNamedNotPrinted(t, "the error", err.Error(), []string{named}, password, "demouser7q")
		})
		t.Run("control: "+decoder.call+", beside a clean address", func(t *testing.T) {
			err := decoder.run(newClient(cipheraddr.New(answer.URL)))
			if err == nil || !strings.Contains(err.Error(), decoder.call+": ") || strings.Contains(err.Error(), cipheraddr.WordsNotPrinted) {
				t.Fatalf("the error %v does not name the call beside a clean address", err)
			}
			wantNamedNotPrinted(t, "the error", err.Error(), []string{named}, password)
		})
	}
}
