package redact_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// probeAnswer is an OAuth error answer x/oauth2 returned whose every field quotes the probe: its
// code, description and error_uri, and its body. It travels through a RefusalClient so the
// boundary reads it on the recorded path. With code false it names no code, which is the form
// whose Error prints the body.
func probeAnswer(t *testing.T, code bool) *oauth2.RetrieveError {
	t.Helper()
	client := redact.RefusalClient(&http.Client{Transport: scopeRoundTripper{
		status: http.StatusBadRequest,
		reason: func(*http.Request) string { return http.StatusText(http.StatusBadRequest) },
		body:   func(*http.Request) string { return "" },
	}})
	response, err := client.Get("https://idp.example/token")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	re := &oauth2.RetrieveError{
		Response: response,
		Body:     []byte(`{"detail":"` + redacttest.Words + `"}`),
	}
	if code {
		re.ErrorCode, re.ErrorDescription, re.ErrorURI = redacttest.Code, redacttest.Words, redacttest.Code
	}
	return re
}

// TestTheBoundarysAnswerPrintsNoSecretItsWordsQuote: an answer that crossed the boundary
// (redact.OAuth2) prints none of the probe's secrets from its code, description, error_uri or
// body, in its Error and as the RetrieveError it unwraps to; and NewRetrieveError's answer prints
// none from the code and description it is given. Each is a designated assertion of the check's
// mutation gate: redact.oauth2-answer (an answer with a code), redact.oauth2-body (one without,
// which prints the body) and redact.new-retrieve-error.
func TestTheBoundarysAnswerPrintsNoSecretItsWordsQuote(t *testing.T) {
	for capture, code := range map[string]bool{"redact.oauth2-answer": true, "redact.oauth2-body": false} {
		_, err := redact.OAuth2(0, probeAnswer(t, code))
		var held *redact.OAuthAnswer
		var re *oauth2.RetrieveError
		if !errors.As(err, &held) || !errors.As(err, &re) {
			t.Fatalf("%s: OAuth2 answered %T, want an *OAuthAnswer over a RetrieveError", capture, err)
		}
		redacttest.RequireNone(t, capture, err.Error()+"\n"+re.Error()+"\n"+string(re.Body))
	}
	made := redact.NewRetrieveError(http.StatusBadRequest, redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words))
	redacttest.RequireNone(t, "redact.new-retrieve-error", made.Error())
}

// oauthAnswerTwin is an OAuthAnswer's twin: its code and refusal parts as strings, and an Error
// that prints them as they are.
type oauthAnswerTwin struct {
	code           string
	refusalCode    string
	refusalMessage string
	answer         *oauth2.RetrieveError
}

func (a *oauthAnswerTwin) Error() string {
	return fmt.Sprintf("oauth2: %q %q %q", a.code, a.refusalCode, a.refusalMessage)
}

type oauthAnswerExported struct {
	Code           string
	RefusalCode    string
	RefusalMessage string
}

// TestTheBoundarysHolderPrintsNoSecretByAnyPrinterAtAnyPlacement is the box print test of the
// boundary's holder, an *OAuthAnswer holding the probe in its code and description.
func TestTheBoundarysHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	_, err := redact.OAuth2(0, probeAnswer(t, true))
	var held *redact.OAuthAnswer
	if !errors.As(err, &held) {
		t.Fatalf("OAuth2 answered %T, want an *OAuthAnswer", err)
	}
	var re *oauth2.RetrieveError
	errors.As(err, &re)
	redacttest.PrintTest(t, redacttest.Holder{
		Name:     "redact.OAuthAnswer",
		Kind:     "error",
		Boxed:    held,
		Same:     &oauthAnswerTwin{redacttest.Code, redacttest.Code, redacttest.Words, re},
		Exported: &oauthAnswerExported{redacttest.Code, redacttest.Code, redacttest.Words},
	})
}

// valueError is a library error of a value type, whose text is its own value: a printer that reads
// a holder's fields by reflection reaches it whole, as it would a string.
type valueError string

func (e valueError) Error() string { return string(e) }

// libraryErrorTwin is a LibraryError's twin: the text as a string, and the library's error.
type libraryErrorTwin struct {
	text string
	err  error
}

func (e *libraryErrorTwin) Error() string { return e.text }

type libraryErrorExported struct{ Text string }

// TestTheBoundarysLibraryErrorPrintsNoSecretByAnyPrinterAtAnyPlacement is the box print test of
// the boundary's other holder: a LibraryError over a library error of a value type whose text
// quotes the probe.
func TestTheBoundarysLibraryErrorPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	library := valueError("oauth2/google: status code 400: " + redacttest.Words)
	_, err := redact.OAuth2(0, error(library))
	var held *redact.LibraryError
	if !errors.As(err, &held) {
		t.Fatalf("OAuth2 answered %T, want a *LibraryError", err)
	}
	redacttest.PrintTest(t, redacttest.Holder{
		Name:     "redact.LibraryError",
		Kind:     "error",
		Boxed:    held,
		Same:     &libraryErrorTwin{string(library), library},
		Exported: &libraryErrorExported{string(library)},
	})
}
