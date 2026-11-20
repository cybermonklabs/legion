package brokerclient

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// TestARefusalWhoseWordsTheRuleQuotesStillPrintsThemQuoted: a description the rule prints quoted,
// whose quoting puts a backslash after a slash, prints quoted through the redactor's reading of
// the refusal, as it did before NewRetrieveError held the rule's reading: that reading is the
// unquoted one, which the redactor reads again unchanged. It reads tradeAt (residual_box_test.go),
// so it is a file apart from boundary_rows_test.go, which the leak control applies alone to a base
// that has no tradeAt.
func TestARefusalWhoseWordsTheRuleQuotesStillPrintsThemQuoted(t *testing.T) {
	words := "see /\npath"
	body, err := json.Marshal(map[string]string{"error": "invalid_grant", "error_description": words})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tradeAt(t, http.StatusBadRequest, string(body))
	want := "the secrets broker refused this machine's identity: HTTP 400 invalid_grant: " + strconv.Quote(words)
	if err == nil || err.Error() != want {
		t.Fatalf("Token = %v, want %q", err, want)
	}
}
