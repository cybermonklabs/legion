package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// shownMost is the longest text ShownBody makes: shownBodyRunes runes of at most utf8.UTFMax bytes
// each, and `… (<n> bytes)` for the largest n there is.
var shownMost = shownBodyRunes*utf8.UTFMax + len(fmt.Sprintf("… (%d bytes)", math.MaxInt))

// longTimeout is a timeout whose text passes ShownBody's bound. Its Timeout and Temporary answer
// true, as net.Error's do for a timeout.
type longTimeout struct{}

func (longTimeout) Error() string   { return "i/o timeout: " + strings.Repeat("t", 2*shownBodyRunes) }
func (longTimeout) Timeout() bool   { return true }
func (longTimeout) Temporary() bool { return true }

// decodeErr is the error json.Unmarshal answers for answer decoded into into. That error must be
// an E itself, not an error that wraps an E.
func decodeErr[E error](t *testing.T, answer string, into any) E {
	t.Helper()
	err := json.Unmarshal([]byte(answer), into)
	decoded, ok := err.(E)
	if !ok {
		t.Fatalf("json.Unmarshal of %.60q… answers %T %.120v; want a %T", answer, err, err, decoded)
	}
	return decoded
}

// ShownErr answers nil for nil, and an error ShownBody shows whole as that error itself, so the
// error keeps its own type and every method a caller type-asserts. It cuts a longer error's text
// as ShownBody cuts GitHub's, naming the size of the error's own text, and keeps the error in
// reach of errors.Is. A cut timeout still answers Timeout and Temporary, also as the cause of a
// url.Error, whose Timeout and Temporary type-assert the cause and do not read its chain. A
// time.ParseError and a json.UnmarshalTypeError that json.Unmarshal answers keep their types: each
// value past the bound is cut alone, with the value's size, and every other field stays, so the
// error still names the layout and the reason, or the field and the type. A time.ParseError of a
// valid time with text after it also has its reason cut, which quotes that text, with the
// reason's size. A time.ParseError still longer than shownErrMost once its fields are cut is cut
// whole, and its answer still reaches it. Each of them within the bound, and a url.Error within
// the bound, comes back as itself.
func TestShownErrCutsOnlyAnErrorPastTheBound(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := ShownErr(nil); got != nil {
			t.Errorf("ShownErr(nil) = %#v, want nil", got)
		}
	})

	t.Run("an error within the bound", func(t *testing.T) {
		short := fmt.Errorf("dial tcp: %w", context.DeadlineExceeded)
		if got := ShownErr(short); got != short {
			t.Errorf("ShownErr of a %d-byte error = %#v, want the error itself", len(short.Error()), got)
		}
	})

	t.Run("an error past the bound", func(t *testing.T) {
		long := fmt.Errorf("%s: %w", strings.Repeat("x", 2*shownBodyRunes), context.DeadlineExceeded)
		text := long.Error()
		want := ShownBody(text)
		if want == text {
			t.Fatalf("the error's text is %d bytes, which ShownBody shows whole; the test needs one it cuts", len(text))
		}
		cut := ShownErr(long)
		if cut.Error() != want {
			t.Errorf("ShownErr's text is %.60q… (%d bytes), want %.60q… (%d bytes)", cut.Error(), len(cut.Error()), want, len(want))
		}
		if end := fmt.Sprintf("… (%d bytes)", len(text)); !strings.HasSuffix(cut.Error(), end) {
			t.Errorf("ShownErr's text ends %q, want %q, the size of the error's own text", cut.Error()[max(0, len(cut.Error())-40):], end)
		}
		if !errors.Is(cut, context.DeadlineExceeded) {
			t.Errorf("errors.Is(ShownErr(err), context.DeadlineExceeded) is false; want the cut error to reach the cause")
		}
	})

	t.Run("a timeout past the bound as a url.Error's cause", func(t *testing.T) {
		cut := ShownErr(longTimeout{})
		if cut.Error() == (longTimeout{}).Error() {
			t.Fatalf("ShownErr shows the %d-byte timeout whole; the test needs one it cuts", len((longTimeout{}).Error()))
		}
		transport := &url.Error{Op: "Get", URL: "https://api.github.com/", Err: cut}
		var netErr net.Error
		if !errors.As(error(transport), &netErr) || !netErr.Timeout() {
			t.Errorf("the url.Error whose cause is the cut timeout answers no timeout; want Timeout true")
		}
		if !transport.Temporary() {
			t.Errorf("the url.Error whose cause is the cut timeout answers Temporary false; want true, as the timeout answers")
		}
	})

	t.Run("a time parse error past the bound", func(t *testing.T) {
		value := strings.Repeat("v", 2*shownBodyRunes)
		parse := decodeErr[*time.ParseError](t, `{"at":"`+value+`"}`, &struct {
			At time.Time `json:"at"`
		}{})
		if parse.Value != value || parse.ValueElem != value {
			t.Fatalf("the parse error's Value is %.60q… (%d bytes) and its ValueElem %.60q… (%d bytes); want the %d-byte value in each", parse.Value, len(parse.Value), parse.ValueElem, len(parse.ValueElem), len(value))
		}
		shown, ok := ShownErr(parse).(*time.ParseError)
		if !ok {
			t.Fatalf("ShownErr of a *time.ParseError answers %T; want a *time.ParseError", ShownErr(parse))
		}
		want := *parse
		want.Value, want.ValueElem = ShownBody(parse.Value), ShownBody(parse.ValueElem)
		if *shown != want {
			t.Errorf("ShownErr answers Value %.60q…, ValueElem %.60q…, Layout %q, LayoutElem %q, Message %q; want Value and ValueElem %.60q…, and Layout %q, LayoutElem %q and Message %q as the parse error has them",
				shown.Value, shown.ValueElem, shown.Layout, shown.LayoutElem, shown.Message, want.Value, parse.Layout, parse.LayoutElem, parse.Message)
		}
		if parse.Value != value || parse.ValueElem != value {
			t.Errorf("ShownErr changed the parse error it was given; want it to cut a copy")
		}
		text := shown.Error()
		if strings.Contains(text, value) {
			t.Errorf("the cut parse error (%d bytes) holds the %d-byte value whole", len(text), len(value))
		}
		if size := fmt.Sprintf(" (%d bytes)", len(value)); !strings.Contains(text, size) {
			t.Errorf("the cut parse error %.120q… names no %q, the size of the value", text, size)
		}
		if reason := ` as "2006-01-02T15:04:05Z07:00": cannot parse `; !strings.Contains(text, reason) {
			t.Errorf("the cut parse error %.120q… does not hold %q; want it to name the layout and the reason", text, reason)
		}
	})

	// time.Parse reads a valid time and fails on the text after it, with a reason that quotes that
	// text (Message, `: extra text: "<text>"`). ShownErr cuts the reason as it cuts the value, so
	// the answer still names the reason (review of cybermonklabs/legion#4373 at a9c02f7a).
	t.Run("a time parse error of a valid time and extra text past the bound", func(t *testing.T) {
		extra := strings.Repeat("x", 100_000)
		value := "2026-11-20T00:00:00Z" + extra
		parse := decodeErr[*time.ParseError](t, `{"t":"`+value+`"}`, &struct {
			T time.Time `json:"t"`
		}{})
		if reason := `: extra text: "` + extra + `"`; parse.Value != value || parse.Message != reason {
			t.Fatalf("the parse error's Value is %.60q… (%d bytes) and its Message %.60q… (%d bytes); want the %d-byte value, and the reason that quotes the %d bytes after the time",
				parse.Value, len(parse.Value), parse.Message, len(parse.Message), len(value), len(extra))
		}
		answer := ShownErr(parse)
		text := answer.Error()
		switch {
		case len(text) > shownErrMost:
			t.Errorf("ShownErr's text is %d bytes; want at most shownErrMost, %d", len(text), shownErrMost)
		case strings.Contains(text, extra):
			t.Errorf("ShownErr's text (%d bytes) holds the %d bytes after the time whole", len(text), len(extra))
		}
		if reason := `": extra text: "` + extra[:256]; !strings.Contains(text, reason) {
			t.Errorf("ShownErr's text %.120q… does not hold %.40q…; want it to name the reason and the start of the text after the time", text, reason)
		}
		if end := fmt.Sprintf("… (%d bytes)", len(parse.Message)); !strings.HasSuffix(text, end) {
			t.Errorf("ShownErr's text ends %q; want the reason as ShownBody shows it, ending %q, the reason's size", text[max(0, len(text)-40):], end)
		}
		var reached *time.ParseError
		if !errors.As(answer, &reached) {
			t.Fatalf("ShownErr's answer, a %T, reaches no *time.ParseError", answer)
		}
		shown, ok := answer.(*time.ParseError)
		if !ok {
			t.Fatalf("ShownErr of a *time.ParseError answers %T; want a *time.ParseError whose fields it cut", answer)
		}
		want := *parse
		want.Value, want.ValueElem, want.Message = ShownBody(parse.Value), ShownBody(parse.ValueElem), ShownBody(parse.Message)
		if *shown != want {
			t.Errorf("ShownErr answers Value %.60q…, ValueElem %.60q…, Message %.60q…, Layout %q, LayoutElem %q; want Value, ValueElem and Message as ShownBody shows them, %.60q…, %.60q… and %.60q…, and Layout %q and LayoutElem %q as the parse error has them",
				shown.Value, shown.ValueElem, shown.Message, shown.Layout, shown.LayoutElem, want.Value, want.ValueElem, want.Message, parse.Layout, parse.LayoutElem)
		}
	})

	// Package time writes each byte past ASCII as \xNN, four bytes in its quotes for each byte, so
	// a value of runes of four bytes leaves a parse error past shownErrMost once ShownErr cuts its
	// fields: about 16,500 bytes for a value that is no time, whose text quotes Value and
	// ValueElem, and about 8,500 for a valid time with such runes after it, whose text quotes
	// Value and the reason. ShownErr cuts each whole, with the size of its text with the fields cut,
	// and the answer still reaches the parse error.
	t.Run("a time parse error still past shownErrMost once its fields are cut", func(t *testing.T) {
		runes := strings.Repeat("𝒱", 25_000)
		for _, row := range []struct{ name, value string }{
			{"a value of four-byte runes that is no time", runes},
			{"a valid time and four-byte runes after it", "2026-11-20T00:00:00Z" + runes},
		} {
			parse := decodeErr[*time.ParseError](t, `{"t":"`+row.value+`"}`, &struct {
				T time.Time `json:"t"`
			}{})
			fields := *parse
			fields.Value, fields.ValueElem, fields.Message = ShownBody(parse.Value), ShownBody(parse.ValueElem), ShownBody(parse.Message)
			cutFields := fields.Error()
			if len(cutFields) <= shownErrMost {
				t.Fatalf("%s: the parse error with its fields cut is %d bytes, at most shownErrMost (%d); the row needs one past it", row.name, len(cutFields), shownErrMost)
			}
			answer := ShownErr(parse)
			text := answer.Error()
			if _, ok := answer.(*shownError); !ok {
				t.Errorf("%s: ShownErr answers a %T of %d bytes; want a *shownError, the parse error cut whole", row.name, answer, len(text))
			}
			if want := ShownBody(cutFields); text != want {
				t.Errorf("%s: ShownErr's text is %.60q… (%d bytes); want the text with the fields cut as ShownBody shows it, %.60q… (%d bytes)", row.name, text, len(text), want, len(want))
			}
			if len(text) > shownMost {
				t.Errorf("%s: ShownErr's text is %d bytes; want at most %d, the longest text ShownBody makes", row.name, len(text), shownMost)
			}
			if end := fmt.Sprintf("… (%d bytes)", len(cutFields)); !strings.HasSuffix(text, end) {
				t.Errorf("%s: ShownErr's text ends %q; want %q, the size of the text with the fields cut", row.name, text[max(0, len(text)-40):], end)
			}
			var reached *time.ParseError
			if !errors.As(answer, &reached) || reached != parse {
				t.Errorf("%s: ShownErr's answer reaches %p, not the parse error %p; want errors.As to reach the parse error", row.name, reached, parse)
			}
		}
	})

	t.Run("a JSON type error past the bound", func(t *testing.T) {
		literal := strings.Repeat("9", 2*shownBodyRunes)
		typeErr := decodeErr[*json.UnmarshalTypeError](t, `{"id":`+literal+`}`, &struct {
			ID int64 `json:"id"`
		}{})
		if typeErr.Value != "number "+literal {
			t.Fatalf("the type error's Value is %.60q… (%d bytes); want `number ` and the %d-byte literal", typeErr.Value, len(typeErr.Value), len(literal))
		}
		shown, ok := ShownErr(typeErr).(*json.UnmarshalTypeError)
		if !ok {
			t.Fatalf("ShownErr of a *json.UnmarshalTypeError answers %T; want a *json.UnmarshalTypeError", ShownErr(typeErr))
		}
		want := *typeErr
		want.Value = "number " + ShownBody(literal)
		if *shown != want {
			t.Errorf("ShownErr answers Value %.60q…, Field %q, Struct %q, Type %v, Offset %d; want Value %.60q… and the other fields of the type error",
				shown.Value, shown.Field, shown.Struct, shown.Type, shown.Offset, want.Value)
		}
		if typeErr.Value != "number "+literal {
			t.Errorf("ShownErr changed the type error it was given; want it to cut a copy")
		}
		text := shown.Error()
		switch {
		case strings.Contains(text, literal):
			t.Errorf("the cut type error (%d bytes) holds the %d-byte literal whole", len(text), len(literal))
		case !strings.Contains(text, "number "+ShownBody(literal)+" "):
			t.Errorf("the cut type error %.120q… does not hold the literal as ShownBody shows it, with the literal's size", text)
		case !strings.HasSuffix(text, ".id of type int64"):
			t.Errorf("the cut type error ends %q; want it to name the field and the type, `.id of type int64`", text[max(0, len(text)-60):])
		}
	})

	t.Run("a time parse error, a JSON type error and a url.Error within the bound", func(t *testing.T) {
		for _, short := range []error{
			decodeErr[*time.ParseError](t, `{"at":"soon"}`, &struct {
				At time.Time `json:"at"`
			}{}),
			decodeErr[*json.UnmarshalTypeError](t, `{"id":`+strings.Repeat("9", 40)+`}`, &struct {
				ID int64 `json:"id"`
			}{}),
			&url.Error{Op: "Get", URL: "https://api.github.com/app/hook/deliveries", Err: context.DeadlineExceeded},
		} {
			if got := ShownErr(short); got != short {
				t.Errorf("ShownErr of the %T %q answers %#v; want the error itself", short, short, got)
			}
		}
	})
}

// A transport error after a URL past the bound still names its cause. http.Client's Do answers a
// url.Error, `Get "<URL>": <cause>`, and send cuts its URL and its cause each (ShownErr):
// a cut of the whole url.Error would keep the URL's first 512 runes and push the cause out (review
// of cybermonklabs/legion#4373 at e185c085). send's own words before the url.Error name the URL as
// ShownBody shows it too, so the error names the whole URL nowhere. The branch here is 60
// characters of three bytes, so its URL, each byte escaped as %XX, passes 512 runes. The context
// has ended before the request, so the cause is context.DeadlineExceeded, and the error still
// reaches it and still answers net.Error's Timeout.
func TestATransportErrorAfterALongURLStillNamesItsCause(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub saw %s %.80s…; want no request, since the context had ended", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	branch := strings.Repeat("枝", 60)
	whole := client.base + "/repos/acme/web/git/ref/heads/" + url.PathEscape(branch)
	shownURL := ShownBody(whole)
	if shownURL == whole {
		t.Fatalf("the URL is %d bytes, which ShownBody shows whole; the test needs one it cuts", len(whole))
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := client.Branch(ctx, "ghs_test", "acme", "web", branch)
	if err == nil {
		t.Fatal("a Branch read on an ended context answered no error")
	}
	text := err.Error()
	if !strings.Contains(text, "context deadline exceeded") {
		t.Errorf("the error names no cause: %.120q… ends %q; want it to name context deadline exceeded", text, text[max(0, len(text)-80):])
	}
	if end := fmt.Sprintf("Get %q: context deadline exceeded", shownURL); !strings.HasSuffix(text, end) {
		t.Errorf("the error ends %q; want the URL as ShownBody shows it, then the cause: %q", text[max(0, len(text)-120):], end[max(0, len(end)-120):])
	}
	if strings.Contains(text, whole) {
		t.Errorf("the error names the %d-byte URL whole; want it only as ShownBody shows it", len(whole))
	}
	if want := fmt.Sprintf("GET %s: ", shownURL); !strings.HasPrefix(text, want) {
		t.Errorf("the error begins %.120q…; want send's words to name the URL as ShownBody shows it, %.120q…", text, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) is false for %.120q…; want the error to reach its cause", text)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("the error answers no net.Error timeout; want Timeout true for a deadline")
	}
}

// A value of an answer Go cannot decode is GitHub's text, which the decode's error quotes. The
// client cuts each field of the time.ParseError that holds such text alone (ShownErr), each with
// its own size, so the error never holds the long text whole. The error is still a
// *time.ParseError, and it still names the reason. Package time quotes each value in the error's
// text and writes each byte past ASCII as \xNN, so the `…` of the cut shows as \xe2\x80\xa6
// there. Each row is an expires_at in the 201 GitHub answers an installation token mint with:
//
//   - 100,000 bytes that are no time. The parse error quotes the value twice, as Value and as
//     ValueElem, and names the layout. The error is no longer than the client's words and the
//     longest text ShownBody makes.
//   - A valid time and 100,000 bytes after it. time.Parse reads the time and fails on the text
//     after it, with a reason that quotes that text (Message, `: extra text: "<text>"`), and
//     ValueElem is that text. The client cuts the reason as it cuts the value (review of
//     cybermonklabs/legion#4373 at a9c02f7a). The error quotes the value once, and it is no
//     longer than the client's words and shownErrMost.
func TestADecodeErrorOfALongValueIsCutToTheBound(t *testing.T) {
	const prefix = "decode installation token: "
	long := strings.Repeat("v", 100_000)
	extra := strings.Repeat("x", 100_000)
	for _, row := range []struct {
		name string
		// expiresAt is the expires_at the mint answers, and long is the text of it that the error
		// must never hold whole.
		expiresAt, long string
		// most is the longest the error may be after the client's words, and mostName names it.
		most     int
		mostName string
		// value, valueElem and message are the parse error's fields as the client must cut them.
		value, valueElem, message string
		// quoted is how many times the error holds expiresAt as ShownBody shows it, in time's
		// quotes.
		quoted int
		// start is the start of long in the error, and words are other texts the error must hold.
		start string
		words []string
	}{{
		name:      "a value of 100,000 bytes that is no time",
		expiresAt: long,
		long:      long,
		most:      shownMost,
		mostName:  "the longest text ShownBody makes",
		value:     ShownBody(long),
		valueElem: ShownBody(long),
		quoted:    2,
		start:     long[:256],
		words:     []string{` as "2006-01-02T15:04:05Z07:00"`, "cannot parse"},
	}, {
		name:      "a valid time and 100,000 bytes after it",
		expiresAt: "2026-11-20T00:00:00Z" + extra,
		long:      extra,
		most:      shownErrMost,
		mostName:  "shownErrMost",
		value:     ShownBody("2026-11-20T00:00:00Z" + extra),
		valueElem: ShownBody(extra),
		message:   ShownBody(`: extra text: "` + extra + `"`),
		quoted:    1,
		start:     `": extra text: "` + extra[:256],
	}} {
		t.Run(strings.ReplaceAll(row.name, " ", "_"), func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/app/installations/7/access_tokens" {
					t.Errorf("GitHub saw %s %s; want the token mint only", r.Method, r.URL.Path)
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, row.expiresAt)
			})

			_, err := client.Token(context.Background(), 7)
			if err == nil {
				t.Fatal("a token mint whose answer does not decode answered no error")
			}
			text := err.Error()
			switch {
			case strings.Contains(text, row.long):
				t.Fatalf("the error (%d bytes) holds the %d-byte text whole; want it cut to a bound", len(text), len(row.long))
			case len(text) > len(prefix)+row.most:
				t.Fatalf("the error is %d bytes; want at most %d, the client's words and %s", len(text), len(prefix)+row.most, row.mostName)
			case !strings.HasPrefix(text, prefix):
				t.Fatalf("the error is %.120q…; want it to start %q", text, prefix)
			case !strings.Contains(text, row.start):
				t.Fatalf("the error is %.200q…; want it to hold %.40q…, the start of the long text", text, row.start)
			}
			var decode *time.ParseError
			if !errors.As(err, &decode) {
				t.Fatalf("the error %.120q… reaches no *time.ParseError", text)
			}
			if decode.Value != row.value || decode.ValueElem != row.valueElem || decode.Message != row.message {
				t.Errorf("the decode error's Value is %.60q… (%d bytes), its ValueElem %.60q… (%d bytes) and its Message %.60q… (%d bytes); want %.60q… (%d bytes), %.60q… (%d bytes) and %.60q… (%d bytes), each as ShownBody shows it, with its own size",
					decode.Value, len(decode.Value), decode.ValueElem, len(decode.ValueElem), decode.Message, len(decode.Message),
					row.value, len(row.value), row.valueElem, len(row.valueElem), row.message, len(row.message))
			}
			if want := prefix + decode.Error(); text != want {
				t.Errorf("the error is %.120q… (%d bytes); want the client's words and the decode error, %.120q… (%d bytes)", text, len(text), want, len(want))
			}
			// quoted is expiresAt as ShownBody shows it, in the quotes of package time.
			quoted := `"` + strings.ReplaceAll(ShownBody(row.expiresAt), "…", `\xe2\x80\xa6`) + `"`
			if n := strings.Count(text, quoted); n != row.quoted {
				t.Errorf("the error holds the value as ShownBody shows it (%q, in time's quotes) %d times; want %d", quoted[len(quoted)-40:], n, row.quoted)
			}
			for _, word := range row.words {
				if !strings.Contains(text, word) {
					t.Errorf("the error %.120q… does not hold %q; want it to name the layout and the reason", text, word)
				}
			}
		})
	}
}

// No error ShownErr answers is longer than shownErrMost, whatever field of the three types that it
// cuts the fields of is long. That holds for a field it cuts, for a field it keeps (a url.Error's
// Op, a time.ParseError's Layout and LayoutElem, a json.UnmarshalTypeError's Struct, Field and
// Err), and for a field past ASCII, which package time writes as \xNN for each byte. Each type
// gets 100,000 bytes in its exported string fields and as the text of its exported error fields:
// in every such field at once, and then in each alone. The 100,000 bytes are ASCII, then a rune
// of two bytes, then a rune of four. A json.UnmarshalTypeError's Type stays a real reflect.Type.
// The answer still reaches an error of the type (errors.As).
func TestShownErrIsNeverLongerThanShownErrMost(t *testing.T) {
	errorType := reflect.TypeFor[error]()
	for _, newErr := range []func() error{
		func() error {
			return &url.Error{Op: "Get", URL: "https://api.github.com/app/hook/deliveries", Err: context.DeadlineExceeded}
		},
		func() error {
			_, err := time.Parse(time.RFC3339, "soon")
			return err
		},
		func() error {
			return &json.UnmarshalTypeError{Value: "number 1e999", Type: reflect.TypeFor[int64](), Offset: 7, Struct: "installationPayload", Field: "id"}
		},
	} {
		typ := reflect.TypeOf(newErr())
		// long is each field of the type that the test gives 100,000 bytes: every exported string
		// field and every exported field of type error.
		var long []reflect.StructField
		for _, field := range reflect.VisibleFields(typ.Elem()) {
			if field.IsExported() && (field.Type.Kind() == reflect.String || field.Type == errorType) {
				long = append(long, field)
			}
		}
		if len(long) == 0 {
			t.Fatalf("the %s has no exported string or error field; the test needs one", typ)
		}
		// sets is each set of fields that one error gets 100,000 bytes in: all of them, then each
		// alone.
		sets := [][]reflect.StructField{long}
		for _, field := range long {
			sets = append(sets, []reflect.StructField{field})
		}
		for _, unit := range []string{"x", "é", "𝒱"} {
			t.Run(fmt.Sprintf("%s in %s", typ, unit), func(t *testing.T) {
				text := strings.Repeat(unit, 100_000/len(unit))
				for _, set := range sets {
					err := newErr()
					fields := reflect.ValueOf(err).Elem()
					var names []string
					for _, field := range set {
						if field.Type == errorType {
							fields.FieldByIndex(field.Index).Set(reflect.ValueOf(errors.New(text)))
						} else {
							fields.FieldByIndex(field.Index).SetString(text)
						}
						names = append(names, field.Name)
					}
					shown := ShownErr(err)
					if n := len(shown.Error()); n > shownErrMost {
						t.Errorf("ShownErr of a %s with %d bytes in %s answers %d bytes; want at most shownErrMost, %d", typ, len(text), strings.Join(names, ", "), n, shownErrMost)
					}
					if !errors.As(shown, reflect.New(typ).Interface()) {
						t.Errorf("ShownErr of a %s with %d bytes in %s answers a %T, which reaches no %s", typ, len(text), strings.Join(names, ", "), shown, typ)
					}
				}
			})
		}
	}
}

// A number past the range of its Go type is GitHub's text, which the decode's error copies: the
// Value of a json.UnmarshalTypeError is `number <literal>`. The client cuts the literal alone
// (ShownErr), so the error holds it as ShownBody shows it, with the literal's size, and never
// whole. The error is still a *json.UnmarshalTypeError, and it still names the field and its type.
// The number here is an id of 1,000 digits in the 200 GitHub answers an installation lookup with
// (installationPayload's ID is an int64).
func TestATypeErrorOfALongNumberIsCutToTheBound(t *testing.T) {
	literal := strings.Repeat("9", 1000)
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/web/installation" {
			t.Errorf("GitHub saw %s %s; want the installation lookup only", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, `{"id":%s,"app_slug":"dispatch","permissions":{"contents":"read"}}`, literal)
	})

	_, err := client.Installation(context.Background(), "acme", "web")
	if err == nil {
		t.Fatal("an installation lookup whose answer does not decode answered no error")
	}
	text := err.Error()
	const prefix = "decode installation: "
	cut := ShownBody(literal)
	var decode *json.UnmarshalTypeError
	switch {
	case strings.Contains(text, literal):
		t.Fatalf("the error (%d bytes) holds the %d-byte literal whole; want it as ShownBody shows it", len(text), len(literal))
	case !errors.As(err, &decode):
		t.Fatalf("the error %.120q… reaches no *json.UnmarshalTypeError", text)
	case decode.Value != "number "+cut:
		t.Errorf("the type error's Value is %.60q… (%d bytes); want `number ` and the literal as ShownBody shows it, %d bytes ending %q",
			decode.Value, len(decode.Value), len(cut), cut[strings.LastIndex(cut, "…"):])
	case decode.Field != "id":
		t.Errorf("the type error's Field is %q; want id", decode.Field)
	case text != prefix+decode.Error():
		t.Errorf("the error is %.120q… (%d bytes); want the client's words and the type error, %.120q…", text, len(text), prefix+decode.Error())
	}
	if want := "number " + cut + " into Go struct field "; !strings.Contains(text, want) {
		t.Errorf("the error %.120q… does not hold the literal as ShownBody shows it, with the literal's size, ending %q", text, want[len(want)-60:])
	}
	if end := "installationPayload.id of type int64"; !strings.HasSuffix(text, end) {
		t.Errorf("the error ends %q; want it to name the field and its type, %q", text[max(0, len(text)-60):], end)
	}
}

// Every error blob answers shows the blob's sha, its path and the target that holds the sha as
// ShownBody shows them, since sha and path come from GitHub's tree answer: a status other than 200
// for a sha past the bound, an encoding other than base64 for a path past the bound, and a listed
// size past notes.MaxFileSize for a path past the bound (notes.CheckSize, which DirFiles asks
// before any blob read).
func TestABlobErrorShowsTheTreesShaAndPathToABound(t *testing.T) {
	long := strings.Repeat("a", 2*shownBodyRunes)
	for _, row := range []struct {
		name, sha, path string
		size            int64
		status          int
		answer          string
		// want is the error's text; {target} stands for the blob's target as ShownBody shows it.
		want string
		// is is an error the answer must still reach, nil for none.
		is error
	}{{
		name:   "a status other than 200 for a sha past the bound",
		sha:    long,
		path:   "docs/a.md",
		size:   1,
		status: http.StatusInternalServerError,
		want:   "GET {target} (docs/a.md): status 500",
	}, {
		name:   "an encoding other than base64 for a path past the bound",
		sha:    "b1",
		path:   "docs/" + long + ".md",
		size:   1,
		status: http.StatusOK,
		answer: `{"content":"","encoding":"utf-8"}`,
		want:   "blob b1 (" + ShownBody("docs/"+long+".md") + `) has encoding "utf-8", want base64`,
	}, {
		name: "a listed size past the notes bound for a path past the bound",
		sha:  "b1",
		path: "docs/" + long + ".md",
		size: notes.MaxFileSize + 1,
		want: fmt.Sprintf("%v: %s is %d bytes (limit %d)", notes.ErrFileTooLarge, ShownBody("docs/"+long+".md"), notes.MaxFileSize+1, notes.MaxFileSize),
		is:   notes.ErrFileTooLarge,
	}} {
		t.Run(strings.ReplaceAll(row.name, " ", "_"), func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/repos/acme/web/git/blobs/") {
					t.Errorf("GitHub saw %s %.80s…; want a blob read only", r.Method, r.URL.Path)
				}
				w.WriteHeader(row.status)
				fmt.Fprint(w, row.answer)
			})
			listing := Dir{SHA: "t1", Files: []DirFile{{Name: "a.md", Path: row.path, SHA: row.sha, Size: row.size}}}
			target := client.base + "/repos/acme/web/git/blobs/" + url.PathEscape(row.sha)
			want := strings.ReplaceAll(row.want, "{target}", ShownBody(target))

			_, err := client.DirFiles(context.Background(), "ghs_test", "acme", "web", listing)
			if err == nil {
				t.Fatal("a blob read of a file GitHub's tree names past the bound answered no error")
			}
			if strings.Contains(err.Error(), long) {
				t.Errorf("the error (%d bytes) holds the %d-byte text from GitHub's tree whole; want it as ShownBody shows it", len(err.Error()), len(long))
			}
			if err.Error() != want {
				t.Errorf("the error is %.120q… (%d bytes); want %.120q… (%d bytes)", err.Error(), len(err.Error()), want, len(want))
			}
			if row.is != nil && !errors.Is(err, row.is) {
				t.Errorf("the error %.120q… does not reach %v; want errors.Is to hold", err.Error(), row.is)
			}
		})
	}
}

// A status error names its request's target, and a target holds names Dispatch keeps and text
// GitHub gave. So each status error that a githubapp caller words itself names the target as
// ShownBody shows it: whole within the bound, and past it the first 512 runes and the target's
// size, never the target whole. Each row's request is answered 500:
//
//   - The deliveries listing's next page. The first page answers 200 with a Link header that
//     names a next page link on the API origin of about 1,500 bytes, under nextLinkMaxBytes.
//   - A Branch read of a name of 60 characters of three bytes, each byte path-escaped as %XX, so
//     its target passes 512 runes (review of cybermonklabs/legion#4373 at e185c085).
func TestAStatusErrorShowsItsTargetToABound(t *testing.T) {
	const failure = `{"message":"Server Error"}`
	branch := strings.Repeat("枝", 60)
	for _, row := range []struct {
		name string
		// first is the listing's first page, which names target as its next page, and empty where
		// the row's one request is to target.
		first string
		// target is the failed request's target after the client's base.
		target string
		// read is the client's read that fails.
		read func(*Client) error
	}{{
		name:   "the deliveries listing's next page",
		first:  "/app/hook/deliveries?per_page=100",
		target: "/app/hook/deliveries?per_page=100&cursor=" + strings.Repeat("c", 1_450),
		read: func(client *Client) error {
			_, err := client.Deliveries(context.Background(), time.Time{}, nil)
			return err
		},
	}, {
		name:   "a branch of 60 characters of three bytes",
		target: "/repos/acme/web/git/ref/heads/" + url.PathEscape(branch),
		read: func(client *Client) error {
			_, err := client.Branch(context.Background(), "ghs_test", "acme", "web", branch)
			return err
		},
	}} {
		t.Run(strings.ReplaceAll(row.name, " ", "_"), func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.RequestURI() {
				case row.first:
					w.Header().Set("Link", "<http://"+r.Host+row.target+`>; rel="next"`)
					fmt.Fprint(w, "[]")
				case row.target:
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, failure)
				default:
					githubtest.Unexpected(t, w, r)
				}
			})
			whole := client.base + row.target
			shown := ShownBody(whole)
			if shown == whole || len(whole) > nextLinkMaxBytes {
				t.Fatalf("the target is %d bytes; the test needs one that ShownBody cuts, of at most nextLinkMaxBytes (%d)", len(whole), nextLinkMaxBytes)
			}

			err := row.read(client)
			if err == nil {
				t.Fatal("a read GitHub answered 500 answered no error")
			}
			text := err.Error()
			if strings.Contains(text, whole) {
				t.Errorf("the error (%d bytes) names the %d-byte target whole; want it only as ShownBody shows it", len(text), len(whole))
			}
			if want := "GET " + shown + ": status 500: " + failure; text != want {
				t.Errorf("the error is %.120q… (%d bytes); want the target as ShownBody shows it and the status, %.120q… (%d bytes)", text, len(text), want, len(want))
			}
		})
	}
}

// nextLink refuses a next page link that leaves the API origin, and one that url.Parse cannot
// parse. A link is GitHub's text, so each refusal names the link as ShownBody shows it, never
// whole. url.Parse's *url.Error quotes the link again, so the refusal holds that error as
// ShownErr shows it, whose URL is the link as ShownBody shows it. Each link is about 1,500 bytes,
// under nextLinkMaxBytes and past 512 runes. The link that url.Parse cannot parse holds a control
// byte (DEL), which %q writes as \x7f. Each refusal is no longer than shownErrMost: two texts as
// ShownBody shows them, and 256 bytes of words.
func TestANextLinkRefusalShowsTheLinkToABound(t *testing.T) {
	const base = "https://api.github.com"
	cursor := strings.Repeat("v", 1_450)
	for _, row := range []struct {
		name, link string
		// want is the refusal's text. {link} stands for the link as ShownBody shows it, quoted
		// with %q.
		want string
		// parse is whether the refusal reaches url.Parse's *url.Error.
		parse bool
	}{{
		name: "a link that leaves the API origin",
		link: "https://collector.example/app/hook/deliveries?per_page=100&cursor=" + cursor,
		want: "next page link {link} leaves the API origin " + base,
	}, {
		name:  "a link that url.Parse cannot parse",
		link:  base + "/app/hook/deliveries?per_page=100&cursor=\x7f" + cursor,
		want:  "parse next page link {link}: parse {link}: net/url: invalid control character in URL",
		parse: true,
	}} {
		t.Run(strings.ReplaceAll(row.name, " ", "_"), func(t *testing.T) {
			shown := ShownBody(row.link)
			if shown == row.link || len(row.link) > nextLinkMaxBytes {
				t.Fatalf("the link is %d bytes; the test needs one that ShownBody cuts, of at most nextLinkMaxBytes (%d)", len(row.link), nextLinkMaxBytes)
			}

			next, err := nextLink("<"+row.link+`>; rel="next"`, base)
			if err == nil {
				t.Fatalf("the %d-byte link was followed (%d bytes); want a refusal", len(row.link), len(next))
			}
			text := err.Error()
			for _, whole := range []struct{ form, text string }{{"as it is", row.link}, {"quoted with %q", fmt.Sprintf("%q", row.link)}} {
				if strings.Contains(text, whole.text) {
					t.Errorf("the refusal (%d bytes) holds the %d-byte link whole, %s; want it only as ShownBody shows it", len(text), len(row.link), whole.form)
				}
			}
			if want := strings.ReplaceAll(row.want, "{link}", fmt.Sprintf("%q", shown)); text != want {
				t.Errorf("the refusal is %.160q… (%d bytes); want %.160q… (%d bytes)", text, len(text), want, len(want))
			}
			if len(text) > shownErrMost {
				t.Errorf("the refusal is %d bytes; want at most shownErrMost, %d", len(text), shownErrMost)
			}
			var parse *url.Error
			switch reached := errors.As(err, &parse); {
			case reached != row.parse:
				t.Errorf("errors.As reaches a *url.Error: %t; want %t", reached, row.parse)
			case reached && parse.URL != shown:
				t.Errorf("url.Parse's error holds the link as %.80q… (%d bytes); want it as ShownBody shows it, %.80q… (%d bytes)", parse.URL, len(parse.URL), shown, len(shown))
			}
		})
	}
}
