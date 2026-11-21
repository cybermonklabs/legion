package approval

import (
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/oauth2"
)

// TestSignInFailureCodeReadsATokenEndpointCodeOnlyFromARefusal pins signInFailureCode on the
// token endpoint's raw answer, as an exchange that is no outage returns it: the code is written
// only from a refusal, a 4xx that is no 408 or 429, and only when it is one of the four the status
// names. A 5xx, a 408 or a 429 naming one of the four, a 2xx naming one, a refusal naming another
// code and one naming none write nothing.
func TestSignInFailureCodeReadsATokenEndpointCodeOnlyFromARefusal(t *testing.T) {
	answered := func(status int, code string) error {
		return &oauth2.RetrieveError{Response: &http.Response{StatusCode: status}, ErrorCode: code}
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a 503 naming invalid_grant", answered(http.StatusServiceUnavailable, codeInvalidGrant), ""},
		{"a 500 naming invalid_client", answered(http.StatusInternalServerError, codeInvalidClient), ""},
		{"a 429 naming redirect_uri_mismatch", answered(http.StatusTooManyRequests, codeRedirectURIMismatch), ""},
		{"a 408 naming unauthorized_client", answered(http.StatusRequestTimeout, codeUnauthorizedClient), ""},
		{"a 200 naming invalid_grant", answered(http.StatusOK, codeInvalidGrant), ""},
		{"a 400 naming access_denied", answered(http.StatusBadRequest, "access_denied"), ""},
		{"a 400 naming no code", answered(http.StatusBadRequest, ""), ""},
		{"a 400 naming invalid_grant", answered(http.StatusBadRequest, codeInvalidGrant), codeInvalidGrant},
		{"a 403 naming unauthorized_client", answered(http.StatusForbidden, codeUnauthorizedClient), codeUnauthorizedClient},
		{"a wrapped 401 naming invalid_client", fmt.Errorf("oidc: exchange code: %w", answered(http.StatusUnauthorized, codeInvalidClient)), codeInvalidClient},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := signInFailureCode(c.err); got != c.want {
				t.Fatalf("signInFailureCode(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}
