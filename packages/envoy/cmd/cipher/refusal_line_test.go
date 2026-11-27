// refusal_line_test.go drives a broker refusal whose code or words hold a control character to
// this client's error, which must read as one line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestABrokerRefusalReadsAsOneLine: a refusal from a broker at a clean address reads as one line
// however its code and words are written. Words and a code with no control character read as they
// came, and ones with a line feed, a carriage return or an escape sequence read Go-quoted. A body
// that is not the envelope still loses its own trailing line ending, as the mux's "404 page not
// found\n" does, rather than reading quoted for it. The code and words this client tells
// refusals apart by are still the ones that came.
func TestABrokerRefusalReadsAsOneLine(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		code, words string
		raw         string // a body that is not the envelope, written as it is
		want        string
	}{
		{name: "plain words", code: "NOT_YOURS", words: "this request belongs to another session",
			want: "this request belongs to another session (NOT_YOURS)"},
		{name: "words with a line feed", code: "NOT_YOURS", words: "refused\nagent-secrets: FORGED from the broker",
			want: strconv.Quote("refused\nagent-secrets: FORGED from the broker") + " (NOT_YOURS)"},
		{name: "words with a carriage return and a line feed", code: "NOT_YOURS", words: "refused\r\nagent-secrets: FORGED",
			want: strconv.Quote("refused\r\nagent-secrets: FORGED") + " (NOT_YOURS)"},
		{name: "words with an escape sequence", code: "NOT_YOURS", words: "refused\x1b[2Kagent-secrets: FORGED",
			want: strconv.Quote("refused\x1b[2Kagent-secrets: FORGED") + " (NOT_YOURS)"},
		{name: "a code with a line feed", code: "NOT_YOURS\nagent-secrets: FORGED", words: "refused",
			want: "refused (" + strconv.Quote("NOT_YOURS\nagent-secrets: FORGED") + ")"},
		{name: "a body that is not the envelope, its line ending cut", raw: "404 page not found\n",
			want: "HTTP 418: 404 page not found"},
		{name: "a body that is not the envelope, with a line inside", raw: "upstream refused\nagent-secrets: FORGED\n",
			want: "HTTP 418: " + strconv.Quote("upstream refused\nagent-secrets: FORGED")},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if row.raw != "" {
					w.WriteHeader(http.StatusTeapot)
					_, _ = w.Write([]byte(row.raw))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": row.code, "error": row.words})
			}))
			defer srv.Close()
			_, err := newClient(cipheraddr.New(srv.URL)).do(context.Background(), http.MethodGet, "/v1/requests/req-1", nil, nil)
			var refusal *apiError
			if !errors.As(err, &refusal) {
				t.Fatalf("the refusal read as %v, want an *apiError", err)
			}
			if got := refusal.Error(); got != row.want {
				t.Errorf("the refusal reads %q, want %q", got, row.want)
			}
			if strings.ContainsAny(refusal.Error(), "\r\n\x1b") {
				t.Errorf("the refusal reads %q, which holds a control character", refusal.Error())
			}
			if row.raw == "" && (!refusal.hasCode(row.code) || !refusal.says(row.words)) {
				t.Errorf("the refusal does not hold the code %q and the words %q as they came", row.code, row.words)
			}
		})
	}
}
