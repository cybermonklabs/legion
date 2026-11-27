package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnAnswerThatDoesNotVerifyNamesItsWordsAsTheRuleReadsThem: an answer the client does not
// verify is another party's words, and an error names them only as the rule reads them, with the
// credentials the request carried redacted (Answer.Said). Whatever answers in the broker's place
// echoes the operator's token as a record's key (record-key), as the key id of a row's signature
// that no listed key has (key-id: signedrecord reads an id of eleven characters, which can be eleven
// of the token's), and as a cutover row's family (cutover-family). Each read answers ErrUnknown
// naming no run of the token. The twins show each answer, or Verify's own error of it, holds the
// token's run. Captures holdrecord.answer-words/<case>.
func TestAnAnswerThatDoesNotVerifyNamesItsWordsAsTheRuleReadsThem(t *testing.T) {
	ctx := context.Background()
	signedAt := time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	// keyIDSig is a signature in Sign's form whose key id is operatorKeyID, cut from the bearer
	// the request carried, over 64 bytes no key signed.
	keyIDSig := func(r *http.Request) string {
		bearer := bearerOf(r)
		at := strings.Index(bearer, operatorKeyID)
		if at < 0 {
			return ""
		}
		return bearer[at:at+len(operatorKeyID)] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	}
	answers := map[string]func(r *http.Request) string{
		"record-key": func(r *http.Request) string {
			return `{"records":[{"key":"` + bearerOf(r) + `","value":{},"gen":1,"sig":""}]}`
		},
		"key-id": func(r *http.Request) string {
			return fmt.Sprintf(`{"gen":1,"phase":"released","hold_id":"","signed_at":%q,"sig":%q}`, signedAt, keyIDSig(r))
		},
		"cutover-family": func(r *http.Request) string {
			return `{"families":[{"family":"` + bearerOf(r) + `","state":"done","hold_id":"h","hold_gen":1,"sig":""}]}`
		},
	}
	reads := map[string]func(c *holdrecord.Client) error{
		"record-key": func(c *holdrecord.Client) error { _, err := c.Records(ctx, wordsHold, "done"); return err },
		"key-id":     func(c *holdrecord.Client) error { _, err := c.Row(ctx); return err },
		"cutover-family": func(c *holdrecord.Client) error {
			_, err := c.Cutovers(ctx)
			return err
		},
	}
	for _, name := range []string{"record-key", "key-id", "cutover-family"} {
		base := standIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, answers[name](r))
		})
		t.Run("twin-"+name, func(t *testing.T) {
			response, err := http.DefaultClient.Do(operatorRequest(t, http.MethodGet, base+"/v1/nats-hold"))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || redacttest.Piece(string(body), operatorToken) == "" {
				t.Fatalf("the stand-in's answer %q, %v holds no run of the token, so the row cannot see one", body, err)
			}
			if name != "key-id" {
				return
			}
			var row struct{ Sig string }
			if err := json.Unmarshal(body, &row); err != nil {
				t.Fatal(err)
			}
			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			if err := signedrecord.Verify([]ed25519.PublicKey{pub}, row.Sig, []string{"line"}); err == nil || redacttest.Piece(err.Error(), operatorToken) == "" {
				t.Fatalf("signedrecord.Verify's own refusal %v holds no run of the token, so the row cannot see one", err)
			}
		})
		t.Run(name, func(t *testing.T) {
			c, _ := wordsClient(t, base)
			err := reads[name](c)
			redacttest.RequireNoPiece(t, "holdrecord.answer-words/"+name, errText(err), operatorToken)
			if !errors.Is(err, holdrecord.ErrUnknown) {
				t.Errorf("the read answered %v, want ErrUnknown", err)
			}
		})
	}
}
