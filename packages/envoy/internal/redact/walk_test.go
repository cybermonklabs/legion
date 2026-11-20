// packages/envoy/internal/redact/walk_test.go
//
// Every error this package makes, walked through every accessor that can copy its text into
// another line: Error and Cause again (beside the same address, another clean one, and one that
// carries a credential), Class, BrokerAnswer, Words, and a wrap with fmt.Errorf's %w. A class is
// fixed text alone, so none of them prints a URL, a path or an id that the error came from. Every
// secret here is made up, and every URL that carries one is built from parts.

package redact_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// walkSecret stands for a credential in a URL, a path or an answer, which no printed text may
// hold. walkTyped stands for an id a caller typed into a broker route: Error's own line for that
// call names it, as Call prints a plain path, and no other line made from the error does.
const (
	walkSecret = "hunter2pw" + "WALK" + "5d0"
	walkTyped  = "typed-" + "walk" + "-id-8f2"
)

// walkRow is one error this package makes, with what every line made from it prints.
type walkRow struct {
	name string
	err  error
	// class is the failure's class, which Cause and Class print beside every address.
	class string
	// line is the error's own text where Error named a call: every Error over it prints that
	// line again, and never names the URL beneath it anew.
	line string
	// op is the operation of the *url.Error beneath an error whose call Error did not name: an
	// Error over it names that call, as NotPrinted beside every address here, before the class.
	op string
}

// TestNoAccessorCopiesACallsURLIntoAnotherLine: no line made from an error this package made -
// Error's call line or class, Cause, Class, Masked, File - holds the URL, path or id the error
// came from. Cause and Class print the class alone, exactly; Error over a call Error named prints
// that same line, and over any other error the class, after its call's operation and NotPrinted
// where a *url.Error is beneath; BrokerAnswer finds no broker's answer in any of them; Words
// prints each line whole beside a clean address and not at all beside any other. The same holds
// for each error wrapped once more with %w. Control: Error's own line for the typed route still
// names that route, id and all, so the check for the id can see it.
func TestNoAccessorCopiesACallsURLIntoAnotherLine(t *testing.T) {
	const base = "http://127.0.0.1:1"
	addresses := map[string]cipheraddr.Address{
		"the address":                          cipheraddr.New(base),
		"another clean address":                cipheraddr.New("https://other-broker.example"),
		"an address that carries a credential": cipheraddr.New("http://" + "walkuser" + ":" + walkSecret + "@127.0.0.1:1"),
	}
	addr, dirty := addresses["the address"], addresses["an address that carries a credential"]
	issuer := "https://" + "walkuser" + ":" + walkSecret + "@issuer.example"
	brokenHTTP := errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP status code "not-a-status"`)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	_, missing := os.ReadFile(filepath.Join(t.TempDir(), walkSecret))
	typedRoute := base + "/v1/enrollments/" + walkTyped
	query := base + "/v1/enrollments/self?token=" + walkSecret

	for _, row := range []walkRow{
		{
			name:  "Error's call line, a typed id in its route",
			err:   addr.Error(&url.Error{Op: "Delete", URL: typedRoute, Err: brokenHTTP}),
			class: "the request failed",
			line:  `Delete "` + typedRoute + `": the request failed`,
		},
		{
			name:  "Error's call line, a credential in its query",
			err:   addr.Error(&url.Error{Op: "Delete", URL: query, Err: brokenHTTP}),
			class: "the request failed",
			line:  `Delete "` + redact.NotPrinted + `": the request failed`,
		},
		{
			name:  "Error's call line beside an address that carries a credential",
			err:   dirty.Error(&url.Error{Op: "Get", URL: dirty.Raw() + "/v1/enrollments/self", Err: refused}),
			class: "connection refused",
			line:  `Get "` + redact.NotPrinted + `": connection refused`,
		},
		{
			name:  "Error's class, with no call",
			err:   addr.Error(errors.New("the answer quoted " + walkSecret)),
			class: "the request failed",
		},
		{
			name: "Error's class, an OAuth answer with an error_uri",
			err: addr.Error(&oauth2.RetrieveError{
				Response:  &http.Response{StatusCode: http.StatusBadRequest},
				ErrorCode: "invalid_request", ErrorDescription: "see the link", ErrorURI: "https://errors.example/" + walkSecret,
			}),
			class: "the broker answered HTTP 400",
		},
		{
			name:  "Cause",
			err:   addr.Cause(&url.Error{Op: "Delete", URL: query, Err: brokenHTTP}),
			class: "the request failed",
			op:    "Delete",
		},
		{
			name:  "Class of a call to another server",
			err:   redact.Class(&url.Error{Op: "Post", URL: issuer + "/token", Err: refused}),
			class: "connection refused",
			op:    "Post",
		},
		{
			name: "Class of another server's answer",
			err: redact.Class(&oauth2.RetrieveError{
				Response:  &http.Response{StatusCode: http.StatusBadRequest},
				ErrorCode: "invalid_client", ErrorDescription: "echo of " + walkSecret,
			}),
			class: "the server answered HTTP 400 invalid_client",
		},
		{
			name:  "Masked",
			err:   redact.Masked(fmt.Errorf("read %s: %w", issuer, errors.New("oidc: unexpected answer")), "the server answered HTTP 404"),
			class: "the server answered HTTP 404",
		},
		{
			name:  "File",
			err:   redact.File(missing),
			class: "open: no such file or directory",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			own := row.line
			if own == "" {
				own = row.class
			}
			if got := row.err.Error(); got != own {
				t.Errorf("the error itself = %q, want %q", got, own)
			}
			walkAccessors(t, row, addresses, row.err)
			walkAccessors(t, row, addresses, fmt.Errorf("walk: %w", row.err))
		})
	}
}

// walkAccessors passes err, row's error or a wrap of it, through every accessor that can copy its
// text into another line, beside each of addresses, and checks what each prints.
func walkAccessors(t *testing.T, row walkRow, addresses map[string]cipheraddr.Address, err error) {
	t.Helper()
	errorLine := row.class
	switch {
	case row.line != "":
		errorLine = row.line
	case row.op != "":
		errorLine = row.op + " " + strconv.Quote(redact.NotPrinted) + ": " + row.class
	}
	printed := map[string]string{}
	check := func(what string, out error, want string) {
		t.Helper()
		if out == nil {
			t.Errorf("%s of %q is nil", what, err)
			return
		}
		if got := out.Error(); got != want {
			t.Errorf("%s of %q = %q, want %q", what, err, got, want)
		}
		if !errors.Is(out, err) {
			t.Errorf("%s of %q does not unwrap to it", what, err)
		}
		printed[what] = out.Error()
	}
	for name, a := range addresses {
		check("Error beside "+name, a.Error(err), errorLine)
		check("Cause beside "+name, a.Cause(err), row.class)
	}
	check("Class", redact.Class(err), row.class)
	if re, ok := redact.BrokerAnswer(err); ok {
		t.Errorf("BrokerAnswer of %q found %v, which a printer would take for the broker's own words", err, re)
	}
	words := map[string]string{}
	for what, text := range printed {
		for name, a := range addresses {
			got := a.Words(text)
			if a.Clean() && got != text || !a.Clean() && got != cipheraddr.WordsNotPrinted {
				t.Errorf("Words beside %s of %s's %q = %q", name, what, text, got)
			}
			words["Words beside "+name+" of "+what] = got
		}
	}
	for what, text := range words {
		printed[what] = text
	}
	for what, text := range printed {
		if strings.Contains(text, walkSecret) || strings.Contains(text, "walkuser") {
			t.Errorf("%s of %q = %q, which holds the credential", what, err, text)
		}
		if strings.Contains(text, walkTyped) && (row.line == "" || text != row.line) {
			t.Errorf("%s of %q = %q, which holds the typed id outside Error's own line for its call", what, err, text)
		}
	}
}
