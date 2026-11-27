package oauthrefusal_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
)

// answered is the error x/oauth2 returns for a token endpoint's answer of status naming code.
func answered(status int, code string) error {
	return &oauth2.RetrieveError{Response: &http.Response{StatusCode: status}, ErrorCode: code}
}

// TestRefusalReadsTheCodeOfARefusalAlone pins which failures name an OAuth error code: a token
// endpoint's answer whose status is a refusal (Refuses), wrapped or not. A status that is no
// refusal, an answer with no response, and an error that is no answer at all name none, whatever
// the body said.
func TestRefusalReadsTheCodeOfARefusalAlone(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"400 invalid_grant", answered(http.StatusBadRequest, "invalid_grant"), "invalid_grant"},
		{"401 invalid_client", answered(http.StatusUnauthorized, "invalid_client"), "invalid_client"},
		{"400 naming no code", answered(http.StatusBadRequest, ""), ""},
		{"a wrapped 400 invalid_grant", fmt.Errorf("refresh: %w", answered(http.StatusBadRequest, "invalid_grant")), "invalid_grant"},
		{"403 access_denied, as the broker denies a device code", answered(http.StatusForbidden, "access_denied"), "access_denied"},
		{"404 invalid_grant", answered(http.StatusNotFound, "invalid_grant"), "invalid_grant"},
		{"408 invalid_grant", answered(http.StatusRequestTimeout, "invalid_grant"), ""},
		{"429 invalid_grant", answered(http.StatusTooManyRequests, "invalid_grant"), ""},
		{"500 invalid_client", answered(http.StatusInternalServerError, "invalid_client"), ""},
		{"503 invalid_grant", answered(http.StatusServiceUnavailable, "invalid_grant"), ""},
		{"an answer with no response", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, ""},
		{"no answer", errors.New("dial tcp: connection refused"), ""},
		{"nil", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := oauthrefusal.Refusal(c.err); got != c.want {
				t.Fatalf("Refusal(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestRefusesNamesTheStatusesARefusalComesWith pins the statuses a token endpoint refuses with:
// every 4xx but 408 and 429.
func TestRefusesNamesTheStatusesARefusalComesWith(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusOK:                  false,
		http.StatusFound:               false,
		http.StatusBadRequest:          true,
		http.StatusUnauthorized:        true,
		http.StatusForbidden:           true,
		http.StatusNotFound:            true,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
		http.StatusServiceUnavailable:  false,
	} {
		if got := oauthrefusal.Refuses(status); got != want {
			t.Errorf("Refuses(%d) = %t, want %t", status, got, want)
		}
	}
}

// TestEndsGrantIsA400Or401InvalidGrantAlone pins the one answer that ends a grant: a 400 or 401
// naming invalid_grant, wrapped or not. Any other refusal, invalid_grant at another status, and
// every failure that is no refusal end nothing.
func TestEndsGrantIsA400Or401InvalidGrantAlone(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"400 invalid_grant", answered(http.StatusBadRequest, "invalid_grant"), true},
		{"401 invalid_grant", answered(http.StatusUnauthorized, "invalid_grant"), true},
		{"a wrapped 400 invalid_grant", fmt.Errorf("refresh: %w", answered(http.StatusBadRequest, "invalid_grant")), true},
		{"400 invalid_client", answered(http.StatusBadRequest, "invalid_client"), false},
		{"401 invalid_request", answered(http.StatusUnauthorized, "invalid_request"), false},
		{"403 invalid_grant", answered(http.StatusForbidden, "invalid_grant"), false},
		{"404 invalid_grant", answered(http.StatusNotFound, "invalid_grant"), false},
		{"408 invalid_grant", answered(http.StatusRequestTimeout, "invalid_grant"), false},
		{"429 invalid_grant", answered(http.StatusTooManyRequests, "invalid_grant"), false},
		{"500 invalid_grant", answered(http.StatusInternalServerError, "invalid_grant"), false},
		{"503 invalid_grant", answered(http.StatusServiceUnavailable, "invalid_grant"), false},
		{"an answer with no response", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, false},
		{"no answer", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := oauthrefusal.EndsGrant(c.err); got != c.want {
				t.Fatalf("EndsGrant(%v) = %t, want %t", c.err, got, c.want)
			}
		})
	}
}
