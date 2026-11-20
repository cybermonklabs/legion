package brokerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// tradeAt is one trade at a broker that answers status and body, beside a clean address.
func tradeAt(t *testing.T, status int, body string) (*Token, error) {
	t.Helper()
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	client, err := New(Config{BrokerURL: brokeraddr.New(base), Key: testKey(t), Subject: projectedStub(), HTTP: HTTPClient()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client.Token(context.Background())
}

// answerWith is a token answer with field set to value, as JSON.
func answerWith(t *testing.T, field string, value any) string {
	t.Helper()
	answer := map[string]any{"access_token": "demo-access-not-a-secret", "token_type": "DPoP", "expires_in": 3600}
	answer[field] = value
	data, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestATokenAnswersFieldsAreReadOnlyThroughTheBox: the token type is compared as it came, so a
// lowercase dpop is the closed TokenTypeDPoP; the expiry and the credential's expiry are parsed, a
// JSON string holding a number among them; and each that is not this grant's is refused in words
// that quote it only as the rule reads it, so a credential's URL in it prints none of its secrets.
func TestATokenAnswersFieldsAreReadOnlyThroughTheBox(t *testing.T) {
	token, err := tradeAt(t, http.StatusOK, answerWith(t, "token_type", "dpop"))
	if err != nil || token.TokenType != TokenTypeDPoP || fmt.Sprint(token.TokenType) != "DPoP" {
		t.Fatalf("a lowercase dpop: %+v, %v; want TokenTypeDPoP", token, err)
	}
	token, err = tradeAt(t, http.StatusOK, answerWith(t, "expires_in", "3600"))
	if err != nil || token.Expiry.IsZero() {
		t.Fatalf("an expiry held in a JSON string: %+v, %v; want it read as json.Number reads it", token, err)
	}
	for field, value := range map[string]any{
		"token_type":            redacttest.Words,
		"expires_in":            redacttest.Words,
		"credential_expires_at": redacttest.Words,
	} {
		t.Run(field, func(t *testing.T) {
			token, err := tradeAt(t, http.StatusOK, answerWith(t, field, value))
			if err == nil {
				t.Fatalf("Token took %+v", token)
			}
			redacttest.RequireNone(t, "the refusal of "+field, err.Error())
			if field != "expires_in" && !strings.Contains(err.Error(), `"`+redacttest.Shown+`"`) {
				t.Errorf("the refusal of %s is %q, want it to quote the field as the rule reads it, %q", field, err, redacttest.Shown)
			}
		})
	}
}

// TestARefusalUnwrapsToTheRulesReadingAndItsCode: a refusal whose description quotes a credential's
// URL unwraps to an *oauth2.RetrieveError that holds the rule's reading of the code and the
// description, so printing that answer whole prints none of the secrets, while redact.RefusalCode
// still reads the refusal's code as it came and the refusal stays one no retry cures.
func TestARefusalUnwrapsToTheRulesReadingAndItsCode(t *testing.T) {
	body, err := json.Marshal(map[string]string{"error": "invalid_grant", "error_description": redacttest.Words})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tradeAt(t, http.StatusBadRequest, string(body))
	var refused *RefusedError
	var answer *oauth2.RetrieveError
	if !errors.As(err, &refused) || !errors.As(err, &answer) {
		t.Fatalf("Token: %v, want the refusal and its OAuth answer", err)
	}
	if answer.ErrorCode != "invalid_grant" || answer.ErrorDescription != redacttest.Shown || answer.ErrorURI != "" || answer.Body != nil {
		t.Errorf("the answer holds %q %q %q %q, want the rule's reading", answer.ErrorCode, answer.ErrorDescription, answer.ErrorURI, answer.Body)
	}
	if redact.RefusalCode(err) != redact.InvalidGrant || refused.RefusalCode() != redact.InvalidGrant || !refused.Permanent() {
		t.Errorf("RefusalCode = %v, the refusal's own %v, permanent %t; want invalid_grant, permanent", redact.RefusalCode(err), refused.RefusalCode(), refused.Permanent())
	}
	redacttest.RequireNoneFormatted(t, "brokerclient.refusal-answer", answer)
	if data, err := json.Marshal(answer); err != nil || len(redacttest.Held(string(data))) != 0 {
		t.Errorf("json.Marshal of the answer: %s, %v", data, err)
	}
}
