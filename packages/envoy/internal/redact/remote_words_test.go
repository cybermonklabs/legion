package redact_test

import (
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

const cleanBase = "http://127.0.0.1:1"

// answer is the broker's OAuth refusal with code and description, as x/oauth2 reports one.
func answer(code, description string) *oauth2.RetrieveError {
	return &oauth2.RetrieveError{
		Response:         &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"},
		ErrorCode:        code,
		ErrorDescription: description,
	}
}

// TestAnOAuthAnswerPrintsItsWordsAsTheRuleReadsThem: beside a clean base, redact.Error and
// redact.Cause print an OAuth answer as x/oauth2's text of it with its code and its description
// each read by the rule (redact.AnswerText), so a URL in either prints with no user information
// and no query or fragment value, and none of the credentials.
func TestAnOAuthAnswerPrintsItsWordsAsTheRuleReadsThem(t *testing.T) {
	rows := map[string]struct {
		code, description, want string
	}{
		"the code": {redacttest.Code, "revoked", fmt.Sprintf("oauth2: %q %q", redacttest.CodeShown, "revoked")},
	}
	for name, words := range map[string][2]string{
		"the description: user information and a query value": {redacttest.Words, redacttest.Shown},
		"the description: a fragment's value":                 {redacttest.FragmentWords, redacttest.FragmentShown},
		"the description: words the rule cannot read":         {redacttest.Unreadable, redact.WordsNotReadable},
	} {
		rows[name] = struct{ code, description, want string }{"invalid_grant", words[0], fmt.Sprintf("oauth2: %q %q", "invalid_grant", words[1])}
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			err := answer(row.code, row.description)
			for capture, printed := range map[string]string{
				"redact.answer-Error": redact.Error(err, cleanBase).Error(),
				"redact.answer-Cause": redact.Cause(err, cleanBase).Error(),
			} {
				redacttest.RequireNone(t, capture, printed)
				if printed != row.want {
					t.Errorf("%s = %q, want %q", capture, printed, row.want)
				}
			}
		})
	}
}

// rawPrintable is a redact.Printable whose sentence quotes its answer's description as it came:
// what an implementation that forgot the rule would hand redact.
type rawPrintable struct{ answer *oauth2.RetrieveError }

func (r *rawPrintable) Error() string         { return "the broker refused: " + r.answer.ErrorDescription }
func (r *rawPrintable) PrintableText() string { return r.Error() }
func (r *rawPrintable) Unwrap() error         { return r.answer }

// TestAPrintablesTextPrintsTheAnswersWordsAsTheRuleReadsThem: a Printable's text is candidate
// text. Where it quotes its answer's description as it came, redact.Error and redact.Cause print
// that description as the rule reads it (redact.sanitized), and none of the credentials.
func TestAPrintablesTextPrintsTheAnswersWordsAsTheRuleReadsThem(t *testing.T) {
	for name, words := range map[string][2]string{
		"user information and a query value": {redacttest.Words, redacttest.Shown},
		"a fragment's value":                 {redacttest.FragmentWords, redacttest.FragmentShown},
		"words the rule cannot read":         {redacttest.Unreadable, redact.WordsNotReadable},
	} {
		t.Run(name, func(t *testing.T) {
			err := &rawPrintable{answer: answer("invalid_grant", words[0])}
			want := "the broker refused: " + words[1]
			for capture, printed := range map[string]string{
				"redact.printable-Error": redact.Error(err, cleanBase).Error(),
				"redact.printable-Cause": redact.Cause(err, cleanBase).Error(),
			} {
				redacttest.RequireNone(t, capture, printed)
				if printed != want {
					t.Errorf("%s = %q, want %q", capture, printed, want)
				}
			}
		})
	}
}
