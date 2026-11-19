package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// planted is the credential every case writes into a URL, so that one search of an output finds
// any part of a credential the rule left.
const planted = "SECRET"

// plantedUserinfo is a userinfo that carries planted as its password. A case joins it to a URL
// with "+" so the line that writes the URL holds no literal user:password@host, which the push's
// leak check refuses wherever it is written.
const plantedUserinfo = "u:" + planted

// redirectError is the error Go's HTTP client gives when a tarball read that GitHub redirected to
// its codeload host fails: the redirect's URL, with its token in the query, quoted in the text.
func redirectError() error {
	return &url.Error{
		Op:  "Get",
		URL: "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1?token=" + planted,
		Err: errors.New("dial tcp 127.0.0.1:9: connect: connection refused"),
	}
}

func TestURLCredentialsRemovesTheQueryAndTheUserinfoOfEveryURL(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{
			name: "a redirect's url.Error",
			text: redirectError().Error(),
			want: `Get "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1": dial tcp 127.0.0.1:9: connect: connection refused`,
		},
		{
			name: "a userinfo with a password",
			text: "clone https://user:" + planted + "@github.com/o/r.git failed",
			want: "clone https://github.com/o/r.git failed",
		},
		{
			name: "a token as the user name",
			text: "fetch https://" + planted + "@github.com/o/r.git: exit status 128",
			want: "fetch https://github.com/o/r.git: exit status 128",
		},
		{
			name: "everything up to the authority's last @",
			text: "https://a@" + planted + "@host.example:8443/p",
			want: "https://host.example:8443/p",
		},
		{
			name: "a URL inside %q quotes",
			text: fmt.Sprintf("parse %q: invalid port", "postgres://"+plantedUserinfo+"@db.example:5432/legion?sslmode=disable&password="+planted),
			want: `parse "postgres://db.example:5432/legion": invalid port`,
		},
		{
			name: "a URL inside <…>",
			text: "see <https://u:" + planted + "@host.example/x?sig=" + planted + "> now",
			want: "see <https://host.example/x> now",
		},
		{
			name: "a URL inside single quotes and one inside backticks",
			text: "'https://host.example/?t=" + planted + "' and `https://host.example/?t=" + planted + "`",
			want: "'https://host.example/' and `https://host.example/`",
		},
		{
			name: "two URLs in one text",
			text: "a https://h.example/?t=" + planted + " then HTTP://" + planted + "@g.example:8080/p?q=" + planted + "#f",
			want: "a https://h.example/ then HTTP://g.example:8080/p#f",
		},
		{
			name: "a query after a port with no path",
			text: "dial http://h.example:8080?key=" + planted,
			want: "dial http://h.example:8080",
		},
		{
			name: "a query before a fragment",
			text: "https://github.com/o/r/blob/main/f.go?token=" + planted + "#L10",
			want: "https://github.com/o/r/blob/main/f.go#L10",
		},
		{
			name: "a query that ends the text",
			text: "go to https://h.example/p?" + planted,
			want: "go to https://h.example/p",
		},
		{
			name: "a URL that ends at a Unicode white space",
			text: "https://h.example/?t=" + planted + "\u00a0more",
			want: "https://h.example/\u00a0more",
		},
		{
			name: "a URL in the path of another",
			text: "https://archive.example/web/https://u:" + planted + "@h.example/x?t=" + planted,
			want: "https://archive.example/web/https://h.example/x",
		},
		{
			name: "a URL in the fragment of another",
			text: "https://h.example/#next=https://" + planted + "@g.example/?t=" + planted + "#top",
			want: "https://h.example/#next=https://g.example/#top",
		},
		{
			name: "a URL in the query of another",
			text: "https://h.example/login?next=https://" + planted + "@g.example/ done",
			want: "https://h.example/login done",
		},
		{
			name: "a fragment kept",
			text: "dispatch://KEY/spec#b-r1",
			want: "dispatch://KEY/spec#b-r1",
		},
		{
			name: "a ? in the fragment kept",
			text: "https://h.example/p#which?that",
			want: "https://h.example/p#which?that",
		},
		{
			name: "an @ in the path kept",
			text: "https://h.example/@user/p",
			want: "https://h.example/@user/p",
		},
		{
			name: "a ? in prose with no URL",
			text: "is it done? yes, at 10:30 // or later?",
			want: "is it done? yes, at 10:30 // or later?",
		},
		{
			name: "a :// after no scheme",
			text: "9://u@h.example/?q and ://u@h.example/?q",
			want: "9://u@h.example/?q and ://u@h.example/?q",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URLCredentials(tc.text)
			if got != tc.want {
				t.Fatalf("URLCredentials(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if strings.Contains(got, planted) {
				t.Fatalf("URLCredentials(%q) = %q, which still holds the planted credential", tc.text, got)
			}
			if again := URLCredentials(got); again != got {
				t.Fatalf("URLCredentials(%q) = %q, want it kept as the rule left it", got, again)
			}
		})
	}
}

func TestErrorIsTheErrorsTextWithItsURLCredentialsRemoved(t *testing.T) {
	if got := Error(nil); got != "" {
		t.Errorf("Error(nil) = %q, want empty", got)
	}
	wrapped := fmt.Errorf("leak check: reading the tarball of v1: %w", redirectError())
	want := `leak check: reading the tarball of v1: Get "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1": dial tcp 127.0.0.1:9: connect: connection refused`
	if got := Error(wrapped); got != want {
		t.Errorf("Error(%q) = %q, want %q", wrapped, got, want)
	}
}

// stringer is a fmt.Stringer that is no error, as a *url.URL is.
type stringer struct{ text string }

func (s stringer) String() string { return s.text }

// Every text of a line takes the rule: the message, a string, an error, a fmt.Stringer, a grouped
// attribute and one a Logger.With carries. A number stays a number, and the caller's own
// ReplaceAttr still runs, before the rule, which reads what it returns.
func TestHandlerOptionsGiveEveryTextOfALineThroughTheURLRule(t *testing.T) {
	var ownCalls []string
	own := func(groups []string, a slog.Attr) slog.Attr {
		ownCalls = append(ownCalls, strings.Join(append(slices.Clone(groups), a.Key), "."))
		switch a.Key {
		case slog.TimeKey:
			return slog.Attr{}
		case "replaced":
			return slog.String("replaced", "by the caller's own https://"+planted+"@h.example/")
		}
		return a
	}
	opts := &slog.HandlerOptions{ReplaceAttr: own}
	var logged bytes.Buffer
	target, err := url.Parse("https://u:" + planted + "@h.example/p?token=" + planted)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(&logged, HandlerOptions(opts))).With("remote", "https://"+planted+"@g.example/o/r.git")
	log.Info("fetch https://h.example/a?token="+planted+" failed",
		"url", "https://"+plantedUserinfo+"@h.example/p?q="+planted,
		"error", redirectError(),
		"target", target,
		"named", stringer{"see https://h.example/?sig=" + planted},
		"count", 3,
		"replaced", "as it was",
		slog.Group("g", "inner", "https://h.example/?t="+planted),
	)
	if strings.Contains(logged.String(), planted) {
		t.Fatalf("the line holds the planted credential: %s", logged.String())
	}
	var line map[string]any
	if err := json.Unmarshal(logged.Bytes(), &line); err != nil {
		t.Fatalf("the line %q is no JSON: %v", logged.String(), err)
	}
	want := map[string]any{
		"level":    "INFO",
		"msg":      "fetch https://h.example/a failed",
		"remote":   "https://g.example/o/r.git",
		"url":      "https://h.example/p",
		"error":    `Get "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1": dial tcp 127.0.0.1:9: connect: connection refused`,
		"target":   "https://h.example/p",
		"named":    "see https://h.example/",
		"count":    3.0,
		"replaced": "by the caller's own https://h.example/",
		"g":        map[string]any{"inner": "https://h.example/"},
	}
	if !reflect.DeepEqual(line, want) {
		t.Fatalf("the line is %v, want %v", line, want)
	}
	for _, key := range []string{slog.TimeKey, slog.MessageKey, "remote", "error", "replaced", "g.inner"} {
		if !slices.Contains(ownCalls, key) {
			t.Fatalf("the caller's own ReplaceAttr saw %q, want %q among them", ownCalls, key)
		}
	}
	// The options given are the caller's still: their ReplaceAttr is the caller's own, unwrapped.
	if a := opts.ReplaceAttr(nil, slog.String("url", "https://"+planted+"@h.example/")); a.Value.String() != "https://"+planted+"@h.example/" {
		t.Fatalf("the caller's options were changed: their ReplaceAttr gave %q", a.Value.String())
	}
}

// Nil options are the zero options with the rule: the default level, and every text cleaned.
func TestHandlerOptionsOfNilAreTheZeroOptionsWithTheURLRule(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, HandlerOptions(nil)))
	log.Debug("not written")
	log.Info("read", "url", "https://"+planted+"@h.example/")
	got := logged.String()
	if strings.Contains(got, "not written") || strings.Contains(got, planted) || !strings.Contains(got, "url=https://h.example/") {
		t.Fatalf("logged %q, want the Info line alone, with the URL's userinfo removed", got)
	}
}

// A Stringer whose method panics (a nil *url.URL) is written as the handler writes it with no
// rule, and the rule does not turn it into a panic of the caller's log call.
func TestHandlerOptionsLeaveAPanickingStringerToTheHandler(t *testing.T) {
	var logged bytes.Buffer
	var target *url.URL
	slog.New(slog.NewJSONHandler(&logged, HandlerOptions(nil))).Info("read", "target", target)
	if got := logged.String(); !strings.Contains(got, `"target":null`) {
		t.Fatalf("logged %q, want the nil URL written as null", got)
	}
}
