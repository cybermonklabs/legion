package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact"
)

// longTimeout is a timeout whose text passes ShownBody's bound. Its Timeout and Temporary answer
// true, as net.Error's do for a timeout.
type longTimeout struct{}

func (longTimeout) Error() string   { return "i/o timeout: " + strings.Repeat("t", 2*shownBodyRunes) }
func (longTimeout) Timeout() bool   { return true }
func (longTimeout) Temporary() bool { return true }

// ShownErr answers nil for nil, and an error ShownBody shows whole as that error itself, so the
// error keeps its own type and every method a caller type-asserts. It cuts a longer error's text
// as ShownBody cuts GitHub's, naming the size of the error's own text, and keeps the error in
// reach of errors.Is. A cut timeout still answers Timeout and Temporary, also as the cause of a
// url.Error, whose Timeout and Temporary type-assert the cause and do not read its chain.
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

	t.Run("a url.Error within the bound", func(t *testing.T) {
		short := &url.Error{Op: "Get", URL: "https://api.github.com/app/hook/deliveries", Err: context.DeadlineExceeded}
		if got := ShownErr(short); got != short {
			t.Errorf("ShownErr of the %T %q answers %#v; want the error itself", short, short, got)
		}
	})
}

// A transport error after a URL past the bound still names its cause. http.Client's Do answers a
// url.Error, `Get "<URL>": <cause>`, and send names the call by its target as ShownBody shows it
// and the url.Error by redact.Unanswered: the URL it asked by that one naming, and the cause by a
// transport failure's own words. A cut of the whole error would keep the URL's first 512 runes and
// push the cause out (review of cybermonklabs/legion#4373 at e185c085). The branch here is 60
// characters of three bytes, so its URL, each byte escaped as %XX, passes 512 runes, and
// Unanswered's name of it is redact.NotPrinted, since the URL rule prints no escape: the error
// names the whole URL nowhere. The context has ended before the request, so the cause is
// context.DeadlineExceeded, and the error still reaches it and still answers net.Error's Timeout.
func TestATransportErrorAfterALongURLStillNamesItsCause(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub saw %s %.80s…; want no request, since the context had ended", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	branch := strings.Repeat("枝", 60)
	whole := client.base.String() + "/repos/acme/web/git/ref/heads/" + url.PathEscape(branch)
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
	if end := fmt.Sprintf("Get %q: context deadline exceeded", redact.NotPrinted); !strings.HasSuffix(text, end) {
		t.Errorf("the error ends %q; want the URL as redact.Unanswered names one with an escape, then the cause: %q", text[max(0, len(text)-120):], end)
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

// A value of an answer Go cannot decode is GitHub's text. The answer decodes through
// redact.Answer.Decode, whose error names the member that did not decode and the type that holds
// it, and quotes none of the value, so the error never holds the long text, whole or cut, and is
// no longer than the client's words and the bound. It reaches none of the decoder's own errors
// (a *time.ParseError here), whose text quotes the value. Each row is an expires_at in the 201
// GitHub answers an installation token mint with: 100,000 bytes that are no time, and a valid
// time with 100,000 bytes after it.
func TestADecodeErrorOfALongValueHoldsNoneOfIt(t *testing.T) {
	const prefix = "decode installation token: "
	long := strings.Repeat("v", 100_000)
	extra := strings.Repeat("x", 100_000)
	for _, row := range []struct {
		name string
		// expiresAt is the expires_at the mint answers, and long is the text of it that the error
		// must never hold, in any part.
		expiresAt, long string
	}{
		{name: "a value of 100,000 bytes that is no time", expiresAt: long, long: long},
		{name: "a valid time and 100,000 bytes after it", expiresAt: "2026-11-20T00:00:00Z" + extra, long: extra},
	} {
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
			const want = prefix + `the answer's "expires_at" does not decode where the value holds a time.Time`
			switch {
			case strings.Contains(text, row.long[:8]):
				t.Fatalf("the error (%d bytes) holds the value's text; want none of it", len(text))
			case strings.Contains(text, "2026-11-20"):
				t.Fatalf("the error %.120q… holds the value's time; want none of the value", text)
			case len(text) > len(prefix)+shownErrMost:
				t.Fatalf("the error is %d bytes; want at most %d, the client's words and shownErrMost", len(text), len(prefix)+shownErrMost)
			case text != want:
				t.Errorf("the error is %.200q; want %q, the member and its type and no value", text, want)
			}
			var decode *time.ParseError
			if errors.As(err, &decode) {
				t.Errorf("the error %.120q… reaches a *time.ParseError, whose text quotes the value", text)
			}
		})
	}
}

func TestShownErrIsNeverLongerThanShownErrMost(t *testing.T) {
	errorType := reflect.TypeFor[error]()
	newErr := func() *url.Error {
		return &url.Error{Op: "Get", URL: "https://api.github.com/app/hook/deliveries", Err: context.DeadlineExceeded}
	}
	typ := reflect.TypeFor[*url.Error]()
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
				var reached *url.Error
				if !errors.As(shown, &reached) {
					t.Errorf("ShownErr of a %s with %d bytes in %s answers a %T, which reaches no %s", typ, len(text), strings.Join(names, ", "), shown, typ)
				}
			}
		})
	}
}

// A number past the range of its Go type is GitHub's text. The answer decodes through
// redact.Answer.Decode, whose error names the member, the JSON kind it held and the Go type that
// holds it, and none of the literal, so the error never holds the literal, whole or cut, and it
// reaches no *json.UnmarshalTypeError, whose Value copies it. The number here is an id of 1,000
// digits in the 200 GitHub answers an installation lookup with (installationPayload's ID is an
// int64).
func TestATypeErrorOfALongNumberHoldsNoneOfIt(t *testing.T) {
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
	const want = `decode installation: the answer's "id" is a JSON number where a githubapp.installationPayload holds a int64`
	var decode *json.UnmarshalTypeError
	switch {
	case strings.Contains(text, literal[:8]):
		t.Fatalf("the error (%d bytes) holds the literal; want none of it", len(text))
	case errors.As(err, &decode):
		t.Fatalf("the error %.120q… reaches a *json.UnmarshalTypeError, whose Value copies the literal", text)
	case text != want:
		t.Errorf("the error is %.160q; want %q, the field, its JSON kind and its Go type", text, want)
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
		want:   "blob b1 (" + ShownBody("docs/"+long+".md") + ") is in an encoding that is not base64",
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
			target := client.base.String() + "/repos/acme/web/git/blobs/" + url.PathEscape(row.sha)
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

// A status error names its request's call (Client.target, nextCall), and a call's name holds names
// Dispatch keeps and text GitHub gave. So each status error that a githubapp caller words itself
// names the call as ShownBody shows its name: whole within the bound, and past it the first 512
// runes and the name's size, never the name whole. Each row's request is answered 500:
//
//   - The deliveries listing's next page. The first page answers 200 with a Link header that
//     names a next page link on the API origin of about 1,500 bytes, under nextLinkMaxBytes, whose
//     path passes 512 runes. The error names the link by its path (nextCall), never by its query,
//     which holds GitHub's cursor.
//   - A Branch read of a name of 60 characters of three bytes, each byte path-escaped as %XX, so
//     its route passes 512 runes (review of cybermonklabs/legion#4373 at e185c085).
func TestAStatusErrorShowsItsTargetToABound(t *testing.T) {
	const failure = `{"message":"Server Error"}`
	branch := strings.Repeat("枝", 60)
	for _, row := range []struct {
		name string
		// first is the listing's first page, which names target as its next page, and empty where
		// the row's one request is to target.
		first string
		// target is the failed request's URI, its path and query, after the client's base.
		target string
		// named is the failed request's name after the client's base: target, or for a next page
		// link its path alone.
		named string
		// read is the client's read that fails.
		read func(*Client) error
	}{{
		name:   "the deliveries listing's next page",
		first:  "/app/hook/deliveries?per_page=100",
		target: "/app/hook/deliveries/" + strings.Repeat("c", 1_450) + "?per_page=100&cursor=v1_2",
		named:  "/app/hook/deliveries/" + strings.Repeat("c", 1_450),
		read: func(client *Client) error {
			_, err := client.Deliveries(context.Background(), time.Time{}, nil)
			return err
		},
	}, {
		name:   "a branch of 60 characters of three bytes",
		target: "/repos/acme/web/git/ref/heads/" + url.PathEscape(branch),
		named:  "/repos/acme/web/git/ref/heads/" + url.PathEscape(branch),
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
			whole := client.base.String() + row.named
			shown := ShownBody(whole)
			if shown == whole || len(whole) > nextLinkMaxBytes {
				t.Fatalf("the name is %d bytes; the test needs one that ShownBody cuts, of at most nextLinkMaxBytes (%d)", len(whole), nextLinkMaxBytes)
			}

			err := row.read(client)
			if err == nil {
				t.Fatal("a read GitHub answered 500 answered no error")
			}
			text := err.Error()
			if strings.Contains(text, whole) {
				t.Errorf("the error (%d bytes) names the %d-byte name whole; want it only as ShownBody shows it", len(text), len(whole))
			}
			if query := strings.TrimPrefix(row.target, row.named); query != "" && strings.Contains(text, query) {
				t.Errorf("the error (%d bytes) holds the next page link's query %q; want the link named by its path alone", len(text), query)
			}
			if want := "GET " + shown + ": status 500: " + failure; text != want {
				t.Errorf("the error is %.120q… (%d bytes); want the name as ShownBody shows it and the status, %.120q… (%d bytes)", text, len(text), want, len(want))
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

			next, err := nextLink("<"+row.link+`>; rel="next"`, gitHubOrigin)
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
