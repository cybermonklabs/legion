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
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnUnknownMemberKindNamesDispatchsWordsAsTheRuleReadsThem: Dispatch, or whatever answers in
// its place, answers a person's member kind with words that echo the broker's account token the
// question carried and quote a credential's URL (echo). MemberKind's ErrUnknownMemberKind names the
// kind as the rule reads it, the token redacted (the kind is held as redact.RemoteText, decoded with
// the answer's record), and none of what the stand-in wrote; a kind that is plain words still prints
// as it came, quoted (plain). Captures projectaccess.answer-words/<case>.
func TestAnUnknownMemberKindNamesDispatchsWordsAsTheRuleReadsThem(t *testing.T) {
	// The account token is GitHub-shaped, ghs_ and 36 characters.
	account := githubtest.InstallationToken(t.Name())
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
			redacttest.RequireNoPiece(t, "projectaccess.answer-words/"+c.name, err.Error(), account)
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("MemberKind's error is %q, want %q in it", err, want)
				}
			}
		})
	}
}

// TestARefusalCodeNamesDispatchsWordsAsTheRuleReadsThem: Dispatch, or whatever answers in its
// place, refuses a question 503 with a code that echoes the broker's account token the question
// carried and quotes a credential's URL. The error names the code as the rule reads it, the token
// redacted, since the code is read from the answer with the record of the question; a plain code
// still prints as it came, quoted. Captures projectaccess.answer-words/code-<case>.
func TestARefusalCodeNamesDispatchsWordsAsTheRuleReadsThem(t *testing.T) {
	account := githubtest.InstallationToken(t.Name())
	for _, c := range []struct {
		name string
		code func(echoed string) string
		want string
	}{
		{"echo", func(echoed string) string { return "token " + echoed + " refused" }, `"token [redacted] refused"`},
		{"plain", func(string) string { return "INDEX_REBUILDING" }, `"INDEX_REBUILDING"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": c.code(echoed), "error": "unavailable"})
			}))
			t.Cleanup(server.Close)
			client := projectaccess.New(server.URL, projectaccesstest.StaticAccount(account), nil)
			_, err := client.Access(t.Context(), "PRIV", "carol@example.com")
			// Control: the refusal is the one that names Dispatch's code.
			if err == nil || !strings.Contains(err.Error(), "with the code "+c.want) {
				t.Fatalf("Access = %v, want an error naming the code %s", err, c.want)
			}
			redacttest.RequireNoPiece(t, "projectaccess.answer-words/code-"+c.name, err.Error(), account)
		})
	}
}

// TestAnUnknownVisibilityPrintsNoneOfDispatchsWords: Dispatch, or whatever answers in its place,
// answers a project's visibility with words that echo the broker's account token the question
// carried and quote a credential's URL. Access refuses the answer as ErrUnknownVisibility naming
// the field and the project, and none of the words. Captures projectaccess.answer-words/visibility.
func TestAnUnknownVisibilityPrintsNoneOfDispatchsWords(t *testing.T) {
	account := githubtest.InstallationToken(t.Name())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"project": "PRIV", "visibility": "token " + echoed + " " + redacttest.Words, "owner": "alice@example.com"})
	}))
	t.Cleanup(server.Close)
	_, err := projectaccess.New(server.URL, projectaccesstest.StaticAccount(account), nil).Access(t.Context(), "PRIV", "")
	// Control: the refusal is the unknown visibility's, naming the field and the project.
	if !errors.Is(err, projectaccess.ErrUnknownVisibility) || !strings.Contains(err.Error(), "(visibility) for PRIV") {
		t.Fatalf("Access = %v, want ErrUnknownVisibility naming the visibility field and PRIV", err)
	}
	redacttest.RequireNone(t, "projectaccess.answer-words/visibility", err.Error())
	redacttest.RequireNoPiece(t, "projectaccess.answer-words/visibility", err.Error(), account)
}
