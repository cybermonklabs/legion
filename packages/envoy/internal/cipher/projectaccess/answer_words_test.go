package projectaccess_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnUnknownMemberKindNamesDispatchsWordsAsTheRuleReadsThem: Dispatch, or whatever answers in
// its place, answers a person's member kind with words that echo the broker's account token the
// question carried and quote a credential's URL (echo). MemberKind's ErrUnknownMemberKind names the
// kind as the rule reads it, the token redacted (the kind is held as redact.RemoteText, decoded with
// the answer's record), and none of what the stand-in wrote; a kind that is plain words still prints
// as it came, quoted (plain). Captures projectaccess.answer-words/<case>.
func TestAnUnknownMemberKindNamesDispatchsWordsAsTheRuleReadsThem(t *testing.T) {
	// The account token is GitHub-shaped, ghs_ and 36 characters, written in two parts so no line
	// of this file reads as a credential to a scanner.
	const account = "ghs_" + "0123456789abcdefghijklmnopqrstuvwxyz"
	for _, c := range []struct {
		name string
		kind func(echoed string) string
		want []string
	}{
		{"echo", func(echoed string) string { return "token " + echoed + " refused; " + redacttest.Words }, []string{`"token [redacted] refused; ` + redacttest.Shown + `"`}},
		{"plain", func(string) string { return "guest" }, []string{`"guest"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"person": "carol@example.com", "member_kind": c.kind(echoed)})
			}))
			t.Cleanup(server.Close)
			client := projectaccess.New(server.URL, projectaccesstest.StaticAccount(account), nil)
			_, err := client.MemberKind(t.Context(), "carol@example.com")
			if !errors.Is(err, projectaccess.ErrUnknownMemberKind) {
				t.Fatalf("MemberKind = %v, want ErrUnknownMemberKind", err)
			}
			redacttest.RequireNone(t, "projectaccess.answer-words/"+c.name, err.Error())
			for at := 0; at+8 <= len(account); at++ {
				if strings.Contains(err.Error(), account[at:at+8]) {
					t.Fatalf("MemberKind's error %q prints the account token the question carried", err)
				}
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("MemberKind's error is %q, want %q in it", err, want)
				}
			}
		})
	}
}
