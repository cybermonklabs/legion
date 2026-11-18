package brokeraddr_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// markers stand for each part of a configured address that can carry a credential: its user
// name, its password, its query and its fragment. No output of an Address holding one may contain
// it, as written or hex-encoded.
var markers = []string{"demouser7q", "demopass9z-not-a-secret", "demoquery4k", "demofrag8w"}

// credentialed are addresses that carry a credential, each of which prints as redact.NotPrinted.
var credentialed = []string{
	"http://demouser7q:demopass9z-not-a-secret@127.0.0.1:42203",
	"https://demouser7q:demopass9z-not-a-secret@broker.example.com/v1?token=demoquery4k#demofrag8w",
	"http://demouser7q:demopass9z-not-a-secret@[::1]:13380/v1/requests",
	"https://demouser7q@broker.example.com",
	"https://broker.example.com/?token=demoquery4k",
	"https://broker.example.com/#demofrag8w",
	"demouser7q:demopass9z-not-a-secret@broker.example.com",
}

// cleanAddresses are plain http(s) URLs, each of which prints as written.
var cleanAddresses = []string{
	"https://broker.example.com",
	"https://broker.example.com/",
	"http://127.0.0.1:42203",
}

// verbs are the directives a message may print an Address under, each with what fmt prints for a
// string under it: an Address prints exactly that for String, its printed form.
var verbs = []struct {
	format string
	want   func(printed string) string
}{
	{"%s", same},
	{"%v", same},
	{"%+v", same},
	{"%#v", strconv.Quote},
	{"%q", strconv.Quote},
	{"%x", lowerHex},
	{"%X", upperHex},
}

func same(s string) string     { return s }
func lowerHex(s string) string { return hex.EncodeToString([]byte(s)) }
func upperHex(s string) string { return strings.ToUpper(lowerHex(s)) }

// leaked is the first marker printed holds, as written or hex-encoded in either case, and "" when
// it holds none.
func leaked(printed string) string {
	lower := strings.ToLower(printed)
	for _, m := range markers {
		if strings.Contains(lower, m) || strings.Contains(lower, lowerHex(m)) {
			return m
		}
	}
	return ""
}

// TestAnAddressCarryingACredentialPrintsNoPartOfIt: an address holding a user name, a password,
// a query or a fragment prints as redact.NotPrinted through String, under every verb (through a
// pointer to it too) and as a slog value, and none of those outputs holds a marker.
func TestAnAddressCarryingACredentialPrintsNoPartOfIt(t *testing.T) {
	for _, raw := range credentialed {
		a := brokeraddr.New(raw)
		if got := a.String(); got != redact.NotPrinted || leaked(got) != "" {
			t.Errorf("New(%q).String() = %q, want redact.NotPrinted", raw, got)
		}
		for _, v := range verbs {
			want := v.want(redact.NotPrinted)
			for _, arg := range []any{a, &a} {
				got := fmt.Sprintf(v.format, arg)
				if got != want {
					t.Errorf("Sprintf(%q) of a %T holding %q = %q, want %q", v.format, arg, raw, got, want)
				}
				if m := leaked(got); m != "" {
					t.Errorf("Sprintf(%q) of a %T holding %q prints %q: %q", v.format, arg, raw, m, got)
				}
			}
		}
		if got := a.LogValue(); got.Kind() != slog.KindString || got.String() != redact.NotPrinted {
			t.Errorf("New(%q).LogValue() = %v %q, want the string redact.NotPrinted", raw, got.Kind(), got.String())
		}
	}
}

// TestACleanAddressPrintsAsWritten: a plain http(s) URL prints unchanged through String, under
// every verb as the string itself would, and as a slog value, so the type leaves an ordinary
// log line as useful as it was.
func TestACleanAddressPrintsAsWritten(t *testing.T) {
	for _, raw := range cleanAddresses {
		a := brokeraddr.New(raw)
		if got := a.String(); got != raw {
			t.Errorf("New(%q).String() = %q, want it as written", raw, got)
		}
		for _, v := range verbs {
			want := v.want(raw)
			for _, arg := range []any{a, &a} {
				if got := fmt.Sprintf(v.format, arg); got != want {
					t.Errorf("Sprintf(%q) of a %T holding %q = %q, want %q", v.format, arg, raw, got, want)
				}
			}
		}
		if got := a.LogValue(); got.Kind() != slog.KindString || got.String() != raw {
			t.Errorf("New(%q).LogValue() = %v %q, want the string as written", raw, got.Kind(), got.String())
		}
	}
}

// TestFormatKeepsTheFlagsWidthAndPrecision: Format applies the flags, width and precision of its
// directive to the printed form, as fmt applies them to a string, and a credentialed address
// prints no marker under any of them.
func TestFormatKeepsTheFlagsWidthAndPrecision(t *testing.T) {
	formats := map[string]string{
		"%12.8s": "    https://",
		"%-28s|": "https://broker.example.com  |",
		"%#q":    "`https://broker.example.com`",
		"%+q":    `"https://broker.example.com"`,
		"%.4x":   "68747470",
		"% .2X":  "68 74",
	}
	clean := brokeraddr.New("https://broker.example.com")
	for format, want := range formats {
		if got := fmt.Sprintf(format, clean); got != want {
			t.Errorf("Sprintf(%q) of %q = %q, want %q", format, clean.Raw(), got, want)
		}
		for _, raw := range credentialed {
			if got := fmt.Sprintf(format, brokeraddr.New(raw)); leaked(got) != "" {
				t.Errorf("Sprintf(%q) of an Address holding %q prints %q: %q", format, raw, leaked(got), got)
			}
		}
	}
}

// TestTheZeroAddressPrintsEmpty: an Address nobody set, which New of the empty string is too,
// prints as the empty string does under String, every verb and slog rather than panicking, holds
// no raw value, and reports itself empty and not clean.
func TestTheZeroAddressPrintsEmpty(t *testing.T) {
	if brokeraddr.New("") != (brokeraddr.Address{}) {
		t.Error(`New("") is not the zero Address`)
	}
	var a brokeraddr.Address
	if got := a.String(); got != "" {
		t.Errorf("the zero Address's String() = %q, want empty", got)
	}
	for _, v := range verbs {
		want := v.want("")
		for _, arg := range []any{a, &a} {
			if got := fmt.Sprintf(v.format, arg); got != want {
				t.Errorf("Sprintf(%q) of the zero %T = %q, want %q", v.format, arg, got, want)
			}
		}
	}
	if got := a.LogValue(); got.Kind() != slog.KindString || got.String() != "" {
		t.Errorf("the zero Address's LogValue() = %v %q, want the empty string", got.Kind(), got.String())
	}
	if got := a.Raw(); got != "" {
		t.Errorf("the zero Address's Raw() = %q, want empty", got)
	}
	if !a.Empty() || a.Clean() {
		t.Errorf("the zero Address reports Empty() %v and Clean() %v, want true and false", a.Empty(), a.Clean())
	}
}

// TestASlogFieldPrintsTheAddressAsAMessageMay: a log line whose field holds an Address names it
// as String does under both of slog's handlers, so a credentialed address logs as
// redact.NotPrinted with no marker and a clean one logs as written.
func TestASlogFieldPrintsTheAddressAsAMessageMay(t *testing.T) {
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(out *bytes.Buffer) slog.Handler { return slog.NewTextHandler(out, nil) },
		"json": func(out *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(out, nil) },
	}
	for name, handler := range handlers {
		for _, raw := range slices.Concat(credentialed, cleanAddresses) {
			var out bytes.Buffer
			slog.New(handler(&out)).Info("traded this machine's identity", "broker", brokeraddr.New(raw))
			line := out.String()
			if m := leaked(line); m != "" {
				t.Errorf("the %s handler logs an Address holding %q with %q: %s", name, raw, m, line)
			}
			if want := brokeraddr.New(raw).String(); !strings.Contains(line, want) {
				t.Errorf("the %s handler logs an Address holding %q without %q: %s", name, raw, want, line)
			}
		}
	}
}

// TestAnAddressFmtReadsByReflectionPrintsNoCredential: fmt asks a value for no method where it
// reads it through reflection - in a struct's unexported field, in a slice held there, or under
// %p - and an Address prints no marker there either.
func TestAnAddressFmtReadsByReflectionPrintsNoCredential(t *testing.T) {
	type holder struct {
		Broker  brokeraddr.Address
		broker  brokeraddr.Address
		brokers []brokeraddr.Address
	}
	for _, raw := range credentialed {
		a := brokeraddr.New(raw)
		h := holder{a, a, []brokeraddr.Address{a}}
		for _, v := range verbs {
			if got := fmt.Sprintf(v.format, h); leaked(got) != "" {
				t.Errorf("Sprintf(%q) of a struct holding an Address of %q prints %q: %q", v.format, raw, leaked(got), got)
			}
		}
		if got := fmt.Sprintf("%p", a); leaked(got) != "" {
			t.Errorf("Sprintf(%%p) of an Address holding %q prints %q: %q", raw, leaked(got), got)
		}
	}
}

// TestTheLeakCheckFindsAStringHolder is the control for the checks above: the address as a plain
// string, and a struct holding it in a string field, print a marker of every credentialed address
// under every verb and under %p, so a check that finds none in an Address's output is not
// vacuous, the hex verbs included.
func TestTheLeakCheckFindsAStringHolder(t *testing.T) {
	type holder struct{ broker string }
	for _, raw := range credentialed {
		for _, arg := range []any{raw, holder{raw}} {
			for _, v := range verbs {
				if got := fmt.Sprintf(v.format, arg); leaked(got) == "" {
					t.Errorf("Sprintf(%q) of a %T holding %q prints no marker: %q", v.format, arg, raw, got)
				}
			}
			if got := fmt.Sprintf("%p", arg); leaked(got) == "" {
				t.Errorf("Sprintf(%%p) of a %T holding %q prints no marker: %q", arg, raw, got)
			}
		}
	}
}

// TestRawEmptyAndClean: Raw answers the configured text whole, credential included, since a
// request to the broker is still built from it; Empty reports that none is configured and Clean
// that redact.Clean accepts it.
func TestRawEmptyAndClean(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		empty, clean bool
	}{
		{"", true, false},
		{" ", false, false},
		{"https://broker.example.com", false, true},
		{"http://127.0.0.1:42203", false, true},
		{"http://demouser7q:demopass9z-not-a-secret@127.0.0.1:42203", false, false},
		{"https://broker.example.com/?token=demoquery4k", false, false},
		{"https://broker.example.com/#demofrag8w", false, false},
		{"not a url", false, false},
	} {
		a := brokeraddr.New(tc.raw)
		if got := a.Raw(); got != tc.raw {
			t.Errorf("New(%q).Raw() = %q, want it as configured", tc.raw, got)
		}
		if a.Empty() != tc.empty || a.Clean() != tc.clean {
			t.Errorf("New(%q) reports Empty() %v and Clean() %v, want %v and %v", tc.raw, a.Empty(), a.Clean(), tc.empty, tc.clean)
		}
	}
}

// TestAnAnswersWordsPrintOnlyBesideACleanAddress: Words prints an answer's text as it came beside
// a clean address, and beside one that is not clean - a credential, a query, a fragment, no plain
// URL, none at all - prints WordsNotPrinted, which holds no marker, whatever the text repeats.
func TestAnAnswersWordsPrintOnlyBesideACleanAddress(t *testing.T) {
	for _, raw := range cleanAddresses {
		const words = "the request object names another broker"
		if got := brokeraddr.New(raw).Words(words); got != words {
			t.Errorf("New(%q).Words(%q) = %q, want the text as it came", raw, words, got)
		}
	}
	for _, raw := range append(slices.Clone(credentialed), "not a url", "") {
		words := "the request object names " + raw
		if got := brokeraddr.New(raw).Words(words); got != brokeraddr.WordsNotPrinted || leaked(got) != "" {
			t.Errorf("New(%q).Words(%q) = %q, want %q", raw, words, got, brokeraddr.WordsNotPrinted)
		}
	}
}

// TestAnAnswersWordsPrintOnOneLineBesideACleanAddress: beside a clean address Words prints words
// with no control character as they came, and words that hold one - a line feed, a carriage
// return, an escape sequence, a DEL, a C1 next line, bytes that are not UTF-8 - Go-quoted, on one
// line with every such character escaped, so no answer starts a line of its own or moves the
// cursor in the message that prints it. Beside an address that is not clean they still print as
// WordsNotPrinted.
func TestAnAnswersWordsPrintOnOneLineBesideACleanAddress(t *testing.T) {
	clean := brokeraddr.New("http://127.0.0.1:1")
	for _, words := range []string{"the request object names another broker", "naïve — café, 東京: ok", ""} {
		if got := clean.Words(words); got != words {
			t.Errorf("Words(%q) = %q, want the words as they came", words, got)
		}
	}
	for _, words := range []string{
		"refused\nagent-secrets: FORGED from the broker",
		"refused\r\nagent-secrets: FORGED",
		"refused\ragent-secrets: FORGED",
		"refused\x1b[2Kagent-secrets: FORGED",
		"refused\x7f",
		"refused\u0085agent-secrets: FORGED",
		"refused \xff\xfe",
	} {
		got := clean.Words(words)
		if got != strconv.Quote(words) {
			t.Errorf("Words(%q) = %q, want it Go-quoted, %q", words, got, strconv.Quote(words))
		}
		if strings.ContainsAny(got, "\n\r\x1b\x7f\u0085") || !utf8.ValidString(got) {
			t.Errorf("Words(%q) = %q, which still holds a control character or bytes that are not UTF-8", words, got)
		}
		if again := clean.Words(got); again != got {
			t.Errorf("Words of its own answer %q = %q, want it unchanged", got, again)
		}
		if dirty := brokeraddr.New(credentialed[0]).Words(words); dirty != brokeraddr.WordsNotPrinted {
			t.Errorf("Words(%q) beside a credential = %q, want %q", words, dirty, brokeraddr.WordsNotPrinted)
		}
	}
}

// TestARequestsURLAndErrorPrintAsTheRedactorPrintsThem: Call, Error and Cause are redact's own
// with the address as the base. On a credentialed address the request URL prints as
// redact.NotPrinted, the error as its operation, that URL and its class, and the cause as its
// class, with no marker, each still unwrapping to the failure; on a clean one each is as written.
func TestARequestsURLAndErrorPrintAsTheRedactorPrintsThem(t *testing.T) {
	const credentialedBase = "http://demouser7q:demopass9z-not-a-secret@127.0.0.1:42203"
	const cleanBase = "https://broker.example.com"
	for _, tc := range []struct {
		base               string
		call, error, cause string
	}{
		{credentialedBase, redact.NotPrinted, "Post " + strconv.Quote(redact.NotPrinted) + ": connection refused", "connection refused"},
		{cleanBase, cleanBase + "/v1/oauth/token", `Post "` + cleanBase + `/v1/oauth/token": connection refused`, "connection refused"},
	} {
		a := brokeraddr.New(tc.base)
		callURL := tc.base + "/v1/oauth/token"
		failure := &url.Error{Op: "Post", URL: callURL, Err: syscall.ECONNREFUSED}
		if got := a.Call(callURL); got != tc.call {
			t.Errorf("New(%q).Call(%q) = %q, want %q", tc.base, callURL, got, tc.call)
		}
		for _, printed := range []struct {
			name, want string
			err        error
		}{
			{"Error", tc.error, a.Error(failure)},
			{"Cause", tc.cause, a.Cause(failure)},
		} {
			if got := printed.err.Error(); got != printed.want || leaked(got) != "" {
				t.Errorf("New(%q).%s of a refused call prints %q, want %q", tc.base, printed.name, got, printed.want)
			}
			if !errors.Is(printed.err, syscall.ECONNREFUSED) {
				t.Errorf("New(%q).%s of a refused call no longer unwraps to it: %v", tc.base, printed.name, printed.err)
			}
		}
		if a.Error(nil) != nil || a.Cause(nil) != nil {
			t.Errorf("New(%q) makes an error of no error", tc.base)
		}
	}
}
