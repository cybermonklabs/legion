package auth

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestACallbackErrorPrintsTheIssuersWordsAsTheRuleReadsThem: the issuer's `?error=` or its
// `?error_description=`, as the callback carries them, quotes a URL carrying a credential. The
// CallbackError ParseCallback answers prints both as the rule for remote text reads them, under
// every verb and in a value that holds it, and none of the credentials.
func TestACallbackErrorPrintsTheIssuersWordsAsTheRuleReadsThem(t *testing.T) {
	type callback struct{ code, description, want string }
	rows := map[string]callback{
		"the code, with a description":                        {redacttest.Code, "cancelled", "sign-in callback " + redacttest.CodeShown + ": cancelled"},
		"the code, with no description":                       {redacttest.Code, "", "sign-in callback: " + redacttest.CodeShown},
		"the description: user information and a query value": {"access_denied", redacttest.Words, "sign-in callback access_denied: " + redacttest.Shown},
		"the description: a fragment's value":                 {"access_denied", redacttest.FragmentWords, "sign-in callback access_denied: " + redacttest.FragmentShown},
		"the description: words the rule cannot read":         {"access_denied", redacttest.Unreadable, "sign-in callback access_denied: " + redact.WordsNotReadable},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			query := url.Values{"error": {row.code}}
			if row.description != "" {
				query.Set("error_description", row.description)
			}
			_, _, err := ParseCallback(httptest.NewRequest("GET", "/auth/callback?"+query.Encode(), nil))
			var refused *CallbackError
			if !errors.As(err, &refused) || !refused.HasCode(row.code) {
				t.Fatalf("ParseCallback = %v, want the issuer's refusal", err)
			}
			redacttest.RequireNoneFormatted(t, "auth.CallbackError", refused)
			if err.Error() != row.want {
				t.Errorf("CallbackError = %q, want %q", err, row.want)
			}
		})
	}
}
