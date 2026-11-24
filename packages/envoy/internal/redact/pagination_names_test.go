package redact_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// TestAPageNumberPrintsAndAnUndeclaredQueryValueDoesNot: a request to GitHub carries its page and
// its page size, which GitHub's pagination contract declares public, and a value under a query
// name no contract declares. The answer's words quote all three: the page numbers print as they
// came, so a message that names a number GitHub wrote ("installation ID 1") still reads, and the
// undeclared value is redacted as every value under an unlisted name is.
func TestAPageNumberPrintsAndAnUndeclaredQueryValueDoesNot(t *testing.T) {
	const undeclared = "v1_cursor_7d3f2a9c"
	request, err := http.NewRequest(http.MethodGet, "https://api.github.com/installation/repositories?per_page=100&page=1&cursor="+undeclared, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := "API rate limit exceeded for installation ID 1 on page 1 of 100 at cursor " + undeclared + "."
	answer, err := redact.ReadAnswer(&http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(body)), Request: request}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	words := answer.Words().String()
	if want := "API rate limit exceeded for installation ID 1 on page 1 of 100 at cursor "; !strings.HasPrefix(words, want) {
		t.Errorf("the words are %q; want the page numbers as GitHub wrote them, %q…", words, want)
	}
	// Control: a value under a query name no contract declares is still a credential.
	if strings.Contains(words, undeclared) || !strings.Contains(words, "[redacted]") {
		t.Errorf("the words are %q; want the undeclared cursor value redacted", words)
	}
}
