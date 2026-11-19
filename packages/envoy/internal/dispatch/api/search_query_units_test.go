package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestSearchQueryBoundsCountUTF16CodeUnits holds q to both of its bounds in the unit
// `dispatch search`'s description names and every client counts, UTF-16 code units: one emoji is
// one code point and two units, so it reaches the minimum of 2, and the same holds at the
// maximum. Each row that is refused for its length names the length it read.
func TestSearchQueryBoundsCountUTF16CodeUnits(t *testing.T) {
	handler := newTestHandler(t)
	emoji := "\U0001F642"
	rows := []struct {
		name    string
		q       string
		status  int
		code    string
		message string
	}{
		{name: "one emoji, one code point and 2 units, reaches the minimum", q: emoji, status: http.StatusOK},
		{name: "two code points of 1 unit each reach the minimum", q: "ab", status: http.StatusOK},
		{name: "one code point of 1 unit is under the minimum", q: "a", status: http.StatusBadRequest,
			code: "INVALID_QUERY", message: "q: 1 of at least 2"},
		{name: "one accented letter, 1 unit, is under the minimum", q: "é", status: http.StatusBadRequest,
			code: "INVALID_QUERY", message: "q: 1 of at least 2"},
		{name: "half the maximum in emoji is the maximum in units", q: strings.Repeat(emoji, contracts.SearchQueryMax/2),
			status: http.StatusOK},
		{name: "one emoji more is over it", q: strings.Repeat(emoji, contracts.SearchQueryMax/2+1),
			status: http.StatusBadRequest, code: "CAP_EXCEEDED", message: "q: 1002 of at most 1000"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			response := searchRequest(t, handler, url.Values{"q": {row.q}}.Encode())
			if response.Code != row.status {
				t.Fatalf("q of %d code points: status=%d body=%.300s, want %d", len([]rune(row.q)), response.Code, response.Body.String(), row.status)
			}
			if row.code == "" {
				return
			}
			var refusal struct {
				Code    string `json:"code"`
				Message string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("refusal body %q: %v", response.Body.String(), err)
			}
			if refusal.Code != row.code || !strings.HasPrefix(refusal.Message, row.message) {
				t.Fatalf("refusal = %s %q, want %s starting %q", refusal.Code, refusal.Message, row.code, row.message)
			}
		})
	}
}
