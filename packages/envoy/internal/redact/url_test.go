package redact

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/oauth2"
)

// secretsIn are the credential-like values the probes below put in a URL; no output may hold
// one.
var secretsIn = []string{"tokuser9f3", "hunter2pw", "hunt", "er2pw", "qtok", "frag9z", "AbCtok9f3", "dEfgh", "ij=", "1234", "12/34",
	"2pwSECRET", "77SECRET", "9zSECRET", "klSECRET", "p@ss", "f@g", "t@k", "cd==", "GRANT_ME", "ada@"}

// cleanURLs are plain http(s) URLs URL prints unchanged.
var cleanURLs = []string{
	"https://secrets.internal.example",
	"https://secrets.internal.example/",
	"http://127.0.0.1:13380/v1/requests",
	"http://127.0.0.1:1/v1/enrollments//renew",
	"http://[::1]:13380/v1/requests",
	"https://[fe80::1]",
	"HTTPS://Secrets.Example:443/v1/a-b_c.d~e",
	"http://localhost:13380",
}

// probes are every input the three gate rounds on this redactor printed a credential, a part of
// one, or a forged line from, and the shapes around them: none is provably a plain http(s) URL,
// so each prints as NotPrinted.
var probes = []string{
	// User info, a query, a fragment, and both.
	"https://tokuser9f3:hunter2pw@secrets.internal.example/v1/requests",
	"https://tokuser9f3@secrets.internal.example",
	"https://secrets.internal.example/v1/requests?token=qtok77",
	"https://secrets.internal.example#frag9z",
	"https://secrets.internal.example/?token=qtok77#frag9z",
	"http://127.0.0.1:1/?token=qtok77",
	// An '@' in the password, escaped or not, and a bad escape.
	"https://tokuser9f3:hunt@er2pw@secrets.internal.example",
	"https://tokuser9f3:hu%40nter2pw@secrets.internal.example",
	"http://tokuser9f3:hunter2pw%zz@127.0.0.1:1",
	"http://tokuser9f3:hunter2pw@127.0.0.1:badport",
	// IPv6 with user info, a zone, a bad port, an unclosed bracket.
	"http://tokuser9f3:hunter2pw@[::1]:13380/v1/requests",
	"http://tokuser9f3:hunter2pw@[fe80::1%25eth0]:1",
	"http://[fe80::1%25eth0]:1",
	"http://tokuser9f3:hunter2pw@[::1]:x",
	"http://tokuser9f3:hunter2pw@[::1:1",
	// Round one: a mistyped separator, a token with '/', '?' or '#', a password that begins with
	// digits, "://" inside a password, no scheme.
	"http:/tokuser9f3:hunter2pw@127.0.0.1:1/v1",
	"https//tokuser9f3:hunter2pw@secrets.example",
	"http:tokuser9f3:hunter2pw@secrets.example",
	"//tokuser9f3:hunter2pw@secrets.example",
	"://tokuser9f3:hunter2pw@secrets.example",
	"HTTPS://tokuser9f3:hunter2pw@secrets.example",
	"https://AbCtok9f3/dEfgh+ij=@secrets.example",
	"https://AbCtok9f3?dEfgh@secrets.example",
	"https://AbCtok9f3#dEfgh@secrets.example",
	"https://tokuser9f3:1234/hunter2pw@secrets.example",
	"https://tokuser9f3:12/34@secrets.example",
	"https://ab/cd==@secrets.example",
	"https://tokuser9f3?hunter2pw@secrets.example",
	"tokuser9f3:hu://nter2pw@secrets.example",
	"tokuser9f3:hunter2pw@secrets.internal.example",
	"tokuser9f3:hunter2pw@127.0.0.1:1",
	"tokuser9f3:hunter2pw@secrets.internal.example/?next=https://x",
	"secrets.internal.example/v1?next=https://tokuser9f3:hunter2pw@x",
	"https://secrets.internal.example/v1/users/ada@example.com",
	// Round two: an '@' in a query value or fragment, an escaped '@', a whole URL escaped.
	"https://secrets.internal.example/?next=ada@example.com",
	"https://secrets.example/?password=hunter@2pwSECRET",
	"https://secrets.example/?password=p@ss",
	"https://secrets.example/#f@g",
	"https://secrets.example/#frag@9zSECRET",
	"https://secrets.example/?a=1&token=t@k&b=2",
	"https://secrets.example/?a=1&token=qtok@77SECRET&b=2",
	"https://tokuser9f3:hunter2pw@secrets.example/?next=ada@example.com",
	"http://tokuser9f3:hunter2pw%40127.0.0.1:1",
	"https%3A%2F%2Ftokuser9f3%3Ahunter2pw%40secrets.example",
	"https://secrets.example/?token=qtok77%40x",
	// Round three: a literal '@' with an escaped one, double escaping, a decoded control byte
	// that forges a line, a short token head, and bad escapes beside an '@'.
	"http://ada@corp.example:hunter2pw%40127.0.0.1:1",
	"http://tokuser9f3:hun@ter2pw%40127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%2540127.0.0.1:1",
	"https%253A%252F%252Ftokuser9f3%253Ahunter2pw%2540secrets.example",
	"http://tokuser9f3:hunter2pw%40127.0.0.1:1%0Aagent-secrets: GRANT_ME from the broker",
	"http://127.0.0.1:1\nagent-secrets: GRANT_ME from the broker",
	"http://127.0.0.1:1/\x1b[2Kagent-secrets: GRANT_ME",
	"http://Ab?cdefghijklSECRET@127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%zz%40127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%%40127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%4@127.0.0.1:1",
	"http://tokuser9f3:hunter2pw@127.0.0.1:1%40",
	// A backslash, whitespace, a host net/url takes that is no DNS name, a bad port, a path
	// character outside the allow-list.
	`http://tokuser9f3:hunter2pw\@secrets.example`,
	"http://secrets.example/ tokuser9f3",
	"http://secrets.example.com;token=qtok77",
	"https://tokuser9f3_hunter2pw.example",
	"https://secrets.example:",
	"https://secrets.example:123456",
	"https://-tokuser9f3.example",
	"https://secrets.example/v1/tokuser9f3=hunter2pw",
	"ftp://secrets.example",
	"secrets.example:13380",
}

// TestURLPrintsOnlyAPlainHTTPURL: a provably plain http(s) URL prints unchanged, the empty
// value prints empty, and every probe prints as NotPrinted, holding none of its secrets.
func TestURLPrintsOnlyAPlainHTTPURL(t *testing.T) {
	for _, raw := range cleanURLs {
		if got := URL(raw); got != raw {
			t.Errorf("URL(%q) = %q, want it unchanged", raw, got)
		}
	}
	if got := URL(""); got != "" {
		t.Errorf("URL(\"\") = %q, want empty", got)
	}
	for _, raw := range probes {
		if got := URL(raw); got != NotPrinted {
			t.Errorf("URL(%q) = %q, want %q", raw, got, NotPrinted)
		}
	}
	for _, secret := range secretsIn {
		if strings.Contains(NotPrinted, secret) {
			t.Fatalf("NotPrinted holds %q", secret)
		}
	}
}

// leakyTimeout is a net timeout whose text repeats a credential.
type leakyTimeout struct{}

func (leakyTimeout) Error() string   { return "dial tokuser9f3:hunter2pw@127.0.0.1:1: i/o timeout" }
func (leakyTimeout) Timeout() bool   { return true }
func (leakyTimeout) Temporary() bool { return true }

// TestErrorPrintsOnlyTheClassForAURLItWillNotPrint: for a configured URL URL will not print, the
// error names its operation, the URL as NotPrinted, and the class of failure, and no text of the
// error itself; errors.Is and errors.As still find the cause, and a timeout still answers
// Timeout through the *url.Error.
func TestErrorPrintsOnlyTheClassForAURLItWillNotPrint(t *testing.T) {
	const raw = "http://tokuser9f3:hunter2pw@127.0.0.1:1"
	_, parseErr := url.Parse(raw + "%zz/v1")
	urlErr := func(cause error) error { return &url.Error{Op: "Get", URL: raw + "/v1/enrollments/self", Err: cause} }
	dns := &net.DNSError{Err: "no such host", Name: "tokuser9f3", IsNotFound: true}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "x", Addr: "tokuser9f3"}}
	refusedErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	retrieve := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}, Body: []byte("tokuser9f3")}
	want := func(class string) string { return `Get "` + NotPrinted + `": ` + class }
	for _, row := range []struct {
		name, raw string
		err       error
		want      string
		is        error // a cause errors.Is must still find, nil for none
	}{
		{"name resolution", raw, urlErr(dns), want("name resolution failed"), nil},
		{"a refused connection", raw, urlErr(refusedErr), want("connection refused"), syscall.ECONNREFUSED},
		{"a timeout", raw, urlErr(leakyTimeout{}), want("timed out"), nil},
		{"a deadline", raw, urlErr(context.DeadlineExceeded), want("timed out"), context.DeadlineExceeded},
		{"TLS", raw, urlErr(tls.RecordHeaderError{Msg: "tokuser9f3"}), want("TLS failed"), nil},
		{"an HTTP status", raw, urlErr(retrieve), want("the broker answered HTTP 429"), nil},
		{"anything else", raw, urlErr(refused), want("the request failed"), nil},
		{"a URL that did not parse", raw + "%zz", parseErr, `parse "` + NotPrinted + `": not a valid URL`, nil},
		{"no *url.Error", raw, errors.New("dial tokuser9f3: refused"), "the request failed", nil},
		{"a one-letter user name", "http://u:p@127.0.0.1:1", &url.Error{Op: "Get", URL: "http://u:p@127.0.0.1:1/v1", Err: refusedErr},
			want("connection refused"), syscall.ECONNREFUSED},
		{"a forged line", "http://127.0.0.1:1%0Aagent-secrets: GRANT_ME from", &url.Error{Op: "Get", URL: "http://127.0.0.1:1\nagent-secrets: GRANT_ME from/v1", Err: refusedErr},
			want("connection refused"), syscall.ECONNREFUSED},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.err == nil {
				t.Fatal("the fixture made no error")
			}
			got := Error(row.err, row.raw)
			if got.Error() != row.want {
				t.Fatalf("Error = %q, want %q", got, row.want)
			}
			for _, secret := range append(secretsIn, "%zz", "\n") {
				if strings.Contains(got.Error(), secret) {
					t.Fatalf("Error = %q holds %q", got, secret)
				}
			}
			if !errors.Is(got, row.err) || (row.is != nil && !errors.Is(got, row.is)) {
				t.Fatalf("errors.Is(%v, cause) = false; the cause must stay reachable", got)
			}
		})
	}
	var ue *url.Error
	if got := Error(urlErr(leakyTimeout{}), raw); !errors.As(got, &ue) || !ue.Timeout() {
		t.Fatal("a timeout must still answer Timeout through the *url.Error it came in")
	}
	var found *net.DNSError
	if got := Error(urlErr(dns), raw); !errors.As(got, &found) || !found.IsNotFound {
		t.Fatal("errors.As must still find the *net.DNSError")
	}
}

// TestErrorLeavesACleanURLsErrorAsItIs: for a configured URL URL prints unchanged, a DNS, a
// refused, a timeout, an http-to-https, an EOF, a reset and a broken-pipe error come back as
// they are, with their own text.
func TestErrorLeavesACleanURLsErrorAsItIs(t *testing.T) {
	const raw = "http://127.0.0.1:1"
	call := func(cause error) error { return &url.Error{Op: "Get", URL: raw + "/v1/enrollments/self", Err: cause} }
	for _, err := range []error{
		call(&net.DNSError{Err: "no such host", Name: "broker.invalid", IsNotFound: true}),
		call(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}),
		call(context.DeadlineExceeded),
		call(http.ErrSchemeMismatch),
		call(io.EOF),
		call(io.ErrUnexpectedEOF),
		call(syscall.ECONNRESET),
		call(syscall.EPIPE),
	} {
		if got := Error(err, raw); got != err {
			t.Errorf("Error(%v) = %v, want the error as it is", err, got)
		}
	}
	if Error(nil, "http://tokuser9f3:hunter2pw@127.0.0.1:1") != nil {
		t.Fatal("a nil error must stay nil")
	}
}

// closingServer is a server that takes each connection and closes it without an answer.
func closingServer(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// TestErrorKeepsARealPeersCause: against real servers - an http server asked over https, and
// one that closes the connection - net/http's error for a clean URL comes back as it is, through
// Error, and its cause through Cause.
func TestErrorKeepsARealPeersCause(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	for _, row := range []struct{ base, want string }{
		{"https://" + strings.TrimPrefix(plain.URL, "http://"), "http: server gave HTTP response to HTTPS client"},
		{closingServer(t), "EOF"},
	} {
		_, err := http.Get(row.base + "/v1/enrollments/self")
		if err == nil || !strings.HasSuffix(err.Error(), row.want) {
			t.Fatalf("the fixture's error %v does not end with %q", err, row.want)
		}
		if got := Error(err, row.base); got != err {
			t.Errorf("Error(%v) = %v, want the error as it is", err, got)
		}
		if got := Cause(err, row.base); got == nil || got.Error() != row.want {
			t.Errorf("Cause(%v) = %v, want %q", err, got, row.want)
		}
	}
}

// TestCallPrintsAPlainPathOnACleanBase: a call URL a program built on a clean base prints
// unchanged when it is that base, its scheme in any case, followed by a plain path, and as
// NotPrinted otherwise: an escape, a query, another host or path, a base that is not clean.
func TestCallPrintsAPlainPathOnACleanBase(t *testing.T) {
	const base = "http://127.0.0.1:1"
	for _, row := range []struct{ base, call, want string }{
		{base, base + "/v1/enrollments//renew", base + "/v1/enrollments//renew"},
		{base, base, base},
		{base + "/", base + "/v1/x", base + "/v1/x"},
		{"HTTP://Secrets.Example", "http://Secrets.Example/v1/x", "http://Secrets.Example/v1/x"},
		{"HTTP://Secrets.Example", "http://secrets.example/v1/x", NotPrinted},
		{base, base + "/v1/requests/req%25zz", NotPrinted},
		{base, base + "/v1/requests/req%zz", NotPrinted},
		{base, base + "/v1/requests?state=qtok77", NotPrinted},
		{base, base + "0/v1/x", NotPrinted},
		{base, "http://other.example/v1/x", NotPrinted},
		{base, base + "/magic/AbCtok9f3SECRET", NotPrinted},
		{base, base + "/v1x/AbCtok9f3SECRET", NotPrinted},
		{"http://tokuser9f3:hunter2pw@127.0.0.1:1", "http://tokuser9f3:hunter2pw@127.0.0.1:1/v1/x", NotPrinted},
		{"http://127.0.0.1:1%0A", "http://127.0.0.1:1%0A/v1/x", NotPrinted},
	} {
		if got := Call(row.base, row.call); got != row.want {
			t.Errorf("Call(%q, %q) = %q, want %q", row.base, row.call, got, row.want)
		}
	}
}

// redirectingTo is a server that answers every request with a 302 whose Location is location,
// set as written.
func redirectingTo(t *testing.T, location string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Location"] = []string{location}
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// TestErrorPrintsOnlyTheClassAfterARedirect: with a clean base, net/http's error for a request
// that followed a redirect names the redirect's target, on another host or another path, with a
// query or none; a malformed Location puts the whole target in the wrapped error's text while the
// *url.Error names the clean call. Each prints the class alone through Error and Cause, and the
// same call that never redirected keeps its text.
func TestErrorPrintsOnlyTheClassAfterARedirect(t *testing.T) {
	for _, row := range []struct {
		name, location, wantError, wantCause string
		is                                   error
	}{
		{"a query on another port", "http://127.0.0.1:1/signin?state=qtok77SECRET", `Get "` + NotPrinted + `": connection refused`, "connection refused", syscall.ECONNREFUSED},
		{"a path on another port", "http://127.0.0.1:1/magic/AbCtok9f3SECRET", `Get "` + NotPrinted + `": connection refused`, "connection refused", syscall.ECONNREFUSED},
		{"another path on the same broker", "/magic/AbCtok9f3SECRET", `Get "` + NotPrinted + `": the request failed`, "the request failed", nil},
		{"a malformed Location", "http://127.0.0.1:1/signin%zz?state=qtok77SECRET", "", "the request failed", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			base := redirectingTo(t, row.location)
			_, err := http.Get(base + "/v1/enrollments/self")
			if err == nil || !strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("the fixture's error %v does not name the redirect's target", err)
			}
			wantError := row.wantError
			if wantError == "" {
				wantError = `Get "` + base + `/v1/enrollments/self": the request failed`
			}
			if got := Error(err, base); got.Error() != wantError || !errors.Is(got, err) || (row.is != nil && !errors.Is(got, row.is)) {
				t.Fatalf("Error = %q, want %q, still unwrapping to the cause", got, wantError)
			}
			if got := Cause(err, base); got.Error() != row.wantCause {
				t.Fatalf("Cause = %q, want %q", got, row.wantCause)
			}
		})
	}
	_, err := http.Get("http://127.0.0.1:1/v1/enrollments/self")
	if got := Error(err, "http://127.0.0.1:1"); got != err {
		t.Fatalf("Error = %v, want the clean call's error as it is", got)
	}
	var ue *url.Error
	if got := Cause(err, "http://127.0.0.1:1"); !errors.As(err, &ue) || got != ue.Err {
		t.Fatalf("Cause = %v, want the error the *url.Error wraps", got)
	}
	if got := Error(err, "HTTP://127.0.0.1:1"); got != err {
		t.Fatalf("Error = %v, want the clean call's error as it is with an upper-case scheme", got)
	}
}

// oauthAnswer is the error x/oauth2 returns when a token endpoint answers HTTP 400 with
// contentType and body: an *oauth2.RetrieveError holding the fields the library itself read from
// that body, so each case below is what a broker's answer really yields.
func oauthAnswer(t *testing.T, contentType, body string) *oauth2.RetrieveError {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer s.Close()
	cfg := &oauth2.Config{ClientID: "demo-client", Endpoint: oauth2.Endpoint{TokenURL: s.URL + "/v1/oauth/token", AuthStyle: oauth2.AuthStyleInParams}}
	_, err := cfg.Exchange(context.Background(), "demo-code")
	var answer *oauth2.RetrieveError
	if !errors.As(err, &answer) {
		t.Fatalf("the token endpoint's answer came back as %v, not an OAuth error answer", err)
	}
	return answer
}

// TestErrorPrintsAnOAuthAnswerWholeOnlyWhenItIsTheBrokersOwnWords: beside a clean base and a
// request URL Call prints, an OAuth error answer with an error code and no error_uri prints whole
// through Error and Cause, alone or inside its *url.Error: the broker's code and description as it
// wrote them, a colon, a mailbox and a URL in the description included, since that is the reason
// a person acts on. An answer that carries an error_uri (a URL, which may hold a credential or
// point anywhere) and one with no error code (whose text x/oauth2 makes of the body it kept, line
// breaks and all) print as the class alone in the same places, so neither marker reaches the
// output. Beside a request URL Call will not print, or a base that is not clean, every answer is
// the class alone. Every result still unwraps to the answer itself.
func TestErrorPrintsAnOAuthAnswerWholeOnlyWhenItIsTheBrokersOwnWords(t *testing.T) {
	const (
		clean     = "http://127.0.0.1:1"
		dirty     = "http://demouser7q:demopass9z-not-a-secret@127.0.0.1:1"
		tokenPath = "/v1/oauth/token"
		class     = "the broker answered HTTP 400"
	)
	answers := []struct {
		name, contentType, body string
		// whole is the answer's own text, which Error and Cause print where its text may print;
		// "" is an answer whose text never prints.
		whole string
		// marker is a part of the answer's own text that must never print.
		marker string
	}{
		{"the broker's refusal", "application/json",
			`{"error":"invalid_request","error_description":"the fleet example-fleet is listed with no session of its own, so the request names one"}`,
			`oauth2: "invalid_request" "the fleet example-fleet is listed with no session of its own, so the request names one"`, ""},
		{"a refusal naming a mailbox and a page after a colon", "application/json",
			`{"error":"access_denied","error_description":"no entry lists ops@example.test: ask at https://docs.example.test/fleets"}`,
			`oauth2: "access_denied" "no entry lists ops@example.test: ask at https://docs.example.test/fleets"`, ""},
		{"an error_uri", "application/json",
			`{"error":"invalid_request","error_description":"see the page","error_uri":"https://demouser7q:demopass9z-not-a-secret@files.example.test/demouri5v"}`,
			"", "demouri5v"},
		{"no error code, a plain body", "text/plain", "upstream refused demobody3n\nagent-secrets: FORGED from the broker", "", "demobody3n"},
		{"no error code, a JSON body", "application/json", `{"error_description":"upstream refused demobody3n"}`, "", "demobody3n"},
	}
	placements := []struct {
		name, base string
		// url is the URL of the *url.Error the answer comes in; "" is the answer alone.
		url string
		// errorPrefix is what Error prints before the answer's text or its class.
		errorPrefix string
		// passes is whether the base and the request URL let any answer's text print.
		passes bool
	}{
		{"alone, clean base", clean, "", "", true},
		{"in a *url.Error, clean base", clean, clean + tokenPath, `Post "` + clean + tokenPath + `": `, true},
		{"in a *url.Error naming another host, clean base", clean, "http://192.0.2.10" + tokenPath, `Post "` + NotPrinted + `": `, false},
		{"alone, base with a credential", dirty, "", "", false},
		{"in a *url.Error, base with a credential", dirty, dirty + tokenPath, `Post "` + NotPrinted + `": `, false},
	}
	for _, a := range answers {
		answer := oauthAnswer(t, a.contentType, a.body)
		// The premise: the library's own text is the trusted answer whole, or holds the marker.
		if a.whole != "" && answer.Error() != a.whole || a.marker != "" && !strings.Contains(answer.Error(), a.marker) {
			t.Fatalf("%s: the fixture's answer reads %q", a.name, answer)
		}
		for _, p := range placements {
			t.Run(a.name+", "+p.name, func(t *testing.T) {
				var err error = answer
				if p.url != "" {
					err = &url.Error{Op: "Post", URL: p.url, Err: answer}
				}
				text := class
				if p.passes && a.whole != "" {
					text = a.whole
				}
				gotError, gotCause := Error(err, p.base), Cause(err, p.base)
				if gotError.Error() != p.errorPrefix+text {
					t.Errorf("Error = %q, want %q", gotError, p.errorPrefix+text)
				}
				if gotCause.Error() != text {
					t.Errorf("Cause = %q, want %q", gotCause, text)
				}
				if !errors.Is(gotError, err) {
					t.Errorf("errors.Is(Error, err) = false; the request's error must stay reachable")
				}
				for _, got := range []error{gotError, gotCause} {
					for _, leak := range []string{"demouri5v", "demobody3n", "FORGED", "demouser7q", "demopass9z-not-a-secret", "\n"} {
						if strings.Contains(got.Error(), leak) {
							t.Errorf("%q holds %q", got, leak)
						}
					}
					var reached *oauth2.RetrieveError
					if !errors.As(got, &reached) || reached != answer {
						t.Errorf("errors.As(%q) does not reach the answer; a caller must still read its status and fields", got)
					}
				}
			})
		}
	}
}

// grantID is a caller's typed id, the kind of value a caller puts in the words it wraps around a
// failure, and which must never print through Error or Cause.
type grantID string

// releaseError is a caller's own error type: its text is the caller's message around the failure
// it wraps.
type releaseError struct {
	grant grantID
	err   error
}

func (e *releaseError) Error() string {
	return "release of grant " + string(e.grant) + " failed: " + e.err.Error()
}
func (e *releaseError) Unwrap() error { return e.err }

// TestErrorPrintsNoWordsACallerWrappedAroundTheFailure: what Error and Cause print of a failure
// printable checked is that failure's own text, so a caller that has wrapped words of its own
// around it - fmt.Errorf with %w, or an error type of the caller's - inside the *url.Error, around
// it, or with no *url.Error at all, gets none of them printed: Error prints the *url.Error's
// operation and URL and then the failure's text, and Cause the failure's text alone. The caller's
// typed id is in neither, and the failure is still the one errors.Is finds. The same failures
// unwrapped are the error itself (TestErrorLeavesACleanURLsErrorAsItIs, and the broker's own
// answer in TestErrorPrintsAnOAuthAnswerWholeOnlyWhenItIsTheBrokersOwnWords).
func TestErrorPrintsNoWordsACallerWrappedAroundTheFailure(t *testing.T) {
	const (
		raw  = "http://127.0.0.1:1"
		call = raw + "/v1/grants/x/values"
		id   = grantID("grant-7f3c9a-demo-not-a-secret")
	)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	answer := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, ErrorCode: "invalid_grant", ErrorDescription: "the grant has ended"}
	wrapped := func(cause error) error { return fmt.Errorf("grant %s: %w", id, cause) }
	released := func(cause error) error { return &releaseError{grant: id, err: cause} }
	inCall := func(cause error) error { return &url.Error{Op: "Post", URL: call, Err: cause} }
	prefix := `Post "` + call + `": `
	for _, row := range []struct {
		name  string
		err   error
		cause error // the failure: its own text is what prints, and errors.Is must still find it
		call  bool  // whether the failure came in a *url.Error, whose operation and URL Error prints
	}{
		{"a refused dial wrapped with %w inside the call", inCall(wrapped(refused)), refused, true},
		{"a refused dial in a caller's error type inside the call", inCall(released(refused)), refused, true},
		{"the call wrapped with %w", wrapped(inCall(refused)), refused, true},
		{"the call in a caller's error type", released(inCall(refused)), refused, true},
		{"an EOF wrapped with %w, no call", wrapped(io.EOF), io.EOF, false},
		{"a reset in a caller's error type, no call", released(syscall.ECONNRESET), syscall.ECONNRESET, false},
		{"a timeout wrapped with %w inside the call", inCall(wrapped(context.DeadlineExceeded)), context.DeadlineExceeded, true},
		{"the broker's answer wrapped with %w inside the call", inCall(wrapped(answer)), answer, true},
		{"the broker's answer in a caller's error type, no call", released(answer), answer, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The premise: each error's own text carries the caller's id.
			if !strings.Contains(row.err.Error(), string(id)) {
				t.Fatalf("the fixture's error %q does not carry the id", row.err)
			}
			gotError, gotCause := Error(row.err, raw), Cause(row.err, raw)
			for what, got := range map[string]error{"Error": gotError, "Cause": gotCause} {
				if strings.Contains(got.Error(), string(id)) {
					t.Errorf("%s = %q, which holds the caller's id %q", what, got, id)
				}
				if !errors.Is(got, row.cause) {
					t.Errorf("errors.Is(%s = %q, the failure) = false; the failure must stay reachable", what, got)
				}
			}
			if !errors.Is(gotError, row.err) {
				t.Errorf("errors.Is(Error = %q, the caller's error) = false; Error must unwrap to what it was given", gotError)
			}
			wantError := row.cause.Error()
			if row.call {
				wantError = prefix + wantError
			}
			if gotError.Error() != wantError {
				t.Errorf("Error = %q, want %q", gotError, wantError)
			}
			if gotCause.Error() != row.cause.Error() {
				t.Errorf("Cause = %q, want %q", gotCause, row.cause.Error())
			}
		})
	}
}

// vouched is a Printable of the test's own: its PrintableText is fixed words and its answer's
// description, and its Error names a caller's typed id as well, which must never print.
type vouched struct {
	grant grantID
	err   error
}

func (v *vouched) Error() string { return "grant " + string(v.grant) + ": " + v.PrintableText() }
func (v *vouched) PrintableText() string {
	var re *oauth2.RetrieveError
	if errors.As(v.err, &re) && re.ErrorDescription != "the grant has ended" {
		return "the stand-in broker refused the grant: " + re.ErrorDescription
	}
	return "the stand-in broker refused the grant"
}
func (v *vouched) Unwrap() error { return v.err }

// TestErrorPrintsAPrintablesTextOnlyDirectlyOverATrustedAnswer: a Printable directly over the
// broker's trusted answer prints its PrintableText, never its Error, alone, in a call and under a
// caller's %w, and the id in its Error and in the caller's words prints nowhere. Over an answer
// that is not trusted (it carries an error_uri) it prints as that answer's class, and over a
// trusted answer it does not sit directly on it prints as that answer, since what it vouches for
// is only the answer it unwraps to. A PrintableText that holds a control character - a line
// break from the answer's description, which would forge a line where it prints - prints as the
// answer's own text, which x/oauth2 escapes.
func TestErrorPrintsAPrintablesTextOnlyDirectlyOverATrustedAnswer(t *testing.T) {
	const (
		raw  = "http://127.0.0.1:1"
		call = raw + "/v1/oauth/token"
		id   = grantID("grant-2b8e41-demo-not-a-secret")
		text = "the stand-in broker refused the grant"
	)
	status := &http.Response{StatusCode: http.StatusBadRequest}
	trusted := &oauth2.RetrieveError{Response: status, ErrorCode: "invalid_grant", ErrorDescription: "the grant has ended"}
	forging := &oauth2.RetrieveError{Response: status, ErrorCode: "invalid_grant", ErrorDescription: "ended\nagent-secrets: FORGED from the broker"}
	escaping := &oauth2.RetrieveError{Response: status, ErrorCode: "invalid_grant", ErrorDescription: "ended\x1b[2Kagent-secrets: FORGED"}
	withURI := &oauth2.RetrieveError{Response: status, ErrorCode: "invalid_grant", ErrorURI: "https://docs.example.test/grants"}
	inCall := func(cause error) error { return &url.Error{Op: "Post", URL: call, Err: cause} }
	prefix := `Post "` + call + `": `
	for _, row := range []struct {
		name             string
		err              error
		wantError, cause string
	}{
		{"directly over a trusted answer", &vouched{id, trusted}, text, text},
		{"in a call", inCall(&vouched{id, trusted}), prefix + text, text},
		{"under a caller's %w in a call", inCall(fmt.Errorf("grant %s: %w", id, &vouched{id, trusted})), prefix + text, text},
		{"over an answer with an error_uri", &vouched{id, withURI}, "the broker answered HTTP 400", "the broker answered HTTP 400"},
		{"over a trusted answer under a wrapper of its own", &vouched{id, fmt.Errorf("grant %s: %w", id, trusted)}, trusted.Error(), trusted.Error()},
		{"with a line break from the answer", &vouched{id, forging}, forging.Error(), forging.Error()},
		{"with an escape sequence from the answer, in a call", inCall(&vouched{id, escaping}), prefix + escaping.Error(), escaping.Error()},
	} {
		t.Run(row.name, func(t *testing.T) {
			gotError, gotCause := Error(row.err, raw), Cause(row.err, raw)
			if gotError.Error() != row.wantError {
				t.Errorf("Error = %q, want %q", gotError, row.wantError)
			}
			if gotCause.Error() != row.cause {
				t.Errorf("Cause = %q, want %q", gotCause, row.cause)
			}
			for _, got := range []error{gotError, gotCause} {
				if strings.Contains(got.Error(), string(id)) {
					t.Errorf("%q holds the caller's id %q", got, id)
				}
				if strings.ContainsAny(got.Error(), "\n\x1b") {
					t.Errorf("%q holds a control character from the answer", got)
				}
				var reached *oauth2.RetrieveError
				if !errors.As(got, &reached) {
					t.Errorf("errors.As(%q) does not reach the answer", got)
				}
			}
		})
	}
}

// TestARedactedErrorRedactedAgainBesideACleanAddressPrintsNoTextTheFirstWithheld: Error and Cause
// beside an address that carries a credential print the broker's answer as its class, because
// whoever answers there can repeat the credential. What they return still unwraps to that answer,
// so a caller that redacts the result again - beside a clean address, as another layer does with
// an address of its own - must not find the answer under it and print it then. Every second
// redaction, of each first one alone, under a caller's %w and in a call, prints a class and never
// the echoed credential.
func TestARedactedErrorRedactedAgainBesideACleanAddressPrintsNoTextTheFirstWithheld(t *testing.T) {
	const (
		dirty  = "http://demouser7q:demopass9z-not-a-secret@127.0.0.1:1"
		clean  = "http://127.0.0.1:1"
		echoed = "demopass9z-not-a-secret"
	)
	answer := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_request", ErrorDescription: "echo of " + echoed}
	inCall := &url.Error{Op: "Post", URL: dirty + "/v1/oauth/token", Err: answer}
	firsts := map[string]error{
		"Error of the answer":                Error(answer, dirty),
		"Cause of the answer":                Cause(answer, dirty),
		"Error of the call":                  Error(inCall, dirty),
		"Cause of the call":                  Cause(inCall, dirty),
		"Error of the answer under a caller": fmt.Errorf("step: %w", Error(answer, dirty)),
		"Cause of the call under a caller":   fmt.Errorf("step: %w", Cause(inCall, dirty)),
		"Error of the answer, in a new call": &url.Error{Op: "Post", URL: clean + "/v1/oauth/token", Err: Error(answer, dirty)},
		"Cause of the answer, in a new call": &url.Error{Op: "Post", URL: clean + "/v1/oauth/token", Err: Cause(answer, dirty)},
	}
	for name, first := range firsts {
		t.Run(name, func(t *testing.T) {
			// The premise: the first redaction withheld the answer, and the answer is still under it.
			var reached *oauth2.RetrieveError
			if strings.Contains(first.Error(), echoed) || !errors.As(first, &reached) || reached != answer {
				t.Fatalf("the first redaction is %q; want the class, with the answer under it", first)
			}
			for what, got := range map[string]error{"Error": Error(first, clean), "Cause": Cause(first, clean)} {
				if strings.Contains(got.Error(), echoed) {
					t.Errorf("%s beside a clean address = %q, which holds the credential the first redaction withheld", what, got)
				}
				if !strings.HasSuffix(got.Error(), "the broker answered HTTP 400") {
					t.Errorf("%s beside a clean address = %q, want it to end with the answer's class", what, got)
				}
				if !errors.Is(got, first) {
					t.Errorf("errors.Is(%s = %q, the first redaction) = false", what, got)
				}
			}
		})
	}
}

// TestOneLineQuotesOnlyTextWithAControlCharacter: OneLine is text as it came where it holds no
// control character and is valid UTF-8, and Go-quoted otherwise, under the rule a Printable's
// text is held to (hasControl); its answer is one line and OneLine of it is that answer again.
func TestOneLineQuotesOnlyTextWithAControlCharacter(t *testing.T) {
	for _, text := range []string{"", "the grant has ended", "naïve — café: ok", `already "quoted"`} {
		if got := OneLine(text); got != text {
			t.Errorf("OneLine(%q) = %q, want it as it came", text, got)
		}
	}
	for _, text := range []string{"a\nb", "a\r\nb", "a\rb", "a\x1b[2Kb", "a\tb", "a\x00b", "a\x7fb", "a\u0085b", "a\u009bb", "a\xffb"} {
		got := OneLine(text)
		if got != strconv.Quote(text) || hasControl(got) || OneLine(got) != got {
			t.Errorf("OneLine(%q) = %q, want %q, which holds no control character and stays as it is", text, got, strconv.Quote(text))
		}
	}
}
