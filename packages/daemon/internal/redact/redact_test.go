package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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

// urlCredentialsCases are the rule's cases: each a text, and what the rule leaves of it.
var urlCredentialsCases = []struct {
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
}

func TestURLCredentialsRemovesTheQueryAndTheUserinfoOfEveryURL(t *testing.T) {
	for _, tc := range urlCredentialsCases {
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

// urlCredentialsQuadratic is URLCredentials as it was before it kept its searches: the same rule,
// which for each "://" searches again to the URL's end, three times, so a run of "://" that
// nothing ends (`a://a://…`) takes it time quadratic in the run's length. It is kept as the
// reference URLCredentials's output is held to
// (TestURLCredentialsGivesWhatTheRuleAsFirstWrittenGives): URLCredentials's kept searches are
// right only under an order of the positions it asks from, and this one asks every search afresh.
func urlCredentialsQuadratic(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	var out strings.Builder
	// text[:kept] is written to out or removed. Every removal starts after a "://", so kept is 0
	// until the first one.
	kept := 0
	remove := func(from, to int) {
		if kept == 0 {
			out.Grow(len(text))
		}
		out.WriteString(text[kept:from])
		kept = to
	}
	// A URL's query is removed once the search for the next "://" passes its start: a URL in the
	// path before it has the same query, and a userinfo of that URL comes first in the text.
	query, fragment := -1, -1
	for at := 0; ; {
		i := strings.Index(text[at:], "://")
		if query >= 0 && (i < 0 || at+i >= query) {
			remove(query, fragment)
			at, query = fragment, -1
			continue
		}
		if i < 0 {
			break
		}
		sep := at + i
		at = sep + len("://")
		if !schemeEndsAt(text, sep) {
			continue
		}
		end := at + orLen(strings.IndexFunc(text[at:], endsURL), len(text)-at)
		host := at + orLen(strings.IndexAny(text[at:end], "/?#"), end-at)
		if u := strings.LastIndexByte(text[at:host], '@'); u >= 0 {
			remove(at, at+u+1)
		}
		frag := host + orLen(strings.IndexByte(text[host:end], '#'), end-host)
		if q := strings.IndexByte(text[host:frag], '?'); q >= 0 {
			query, fragment = host+q, frag
		}
	}
	if kept == 0 {
		return text
	}
	out.WriteString(text[kept:])
	return out.String()
}

// URLCredentials leaves of every text what the rule as first written leaves of it
// (urlCredentialsQuadratic): of each text of the cases, and of 5000 texts of up to 300 pieces
// built under a fixed seed, each from "a", "://" and about half of the other pieces the rule
// reads, so that URLs in the paths, the queries and the fragments of others, userinfos, invalid
// UTF-8 and every character that ends a URL meet in many orders. What it leaves, it leaves as it
// is.
func TestURLCredentialsGivesWhatTheRuleAsFirstWrittenGives(t *testing.T) {
	var texts []string
	for _, tc := range urlCredentialsCases {
		texts = append(texts, tc.text)
	}
	pieces := []string{"https", "9", "@", ":", "/", "?", "#", "=", " ", "\u00a0", `"`, "'", "`", "<", ">", "é", "\xff"}
	random := rand.New(rand.NewPCG(4193, 4463))
	for range 5000 {
		some := []string{"a", "://"}
		for _, piece := range pieces {
			if random.IntN(2) == 0 {
				some = append(some, piece)
			}
		}
		var text strings.Builder
		for range random.IntN(301) {
			text.WriteString(some[random.IntN(len(some))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		got, want := URLCredentials(text), urlCredentialsQuadratic(text)
		if got != want {
			t.Fatalf("URLCredentials(%q) = %q, want %q, what the rule as first written gives", text, got, want)
		}
		if again := URLCredentials(got); again != got {
			t.Fatalf("URLCredentials(%q) = %q, want it kept as the rule left it", got, again)
		}
	}
}

// The rule's time grows as its text's length does, not as its square. A line of "a://" repeated
// is the rule as first written at its worst (urlCredentialsQuadratic searched to the line's end
// again for each "://"): on 1 MiB of it, a rule linear in the length returns in tens of
// milliseconds, and that one took about 15 minutes. The run is held to a deadline of 30 s, far
// from both, rather than to a ratio of the times of two lengths: a machine under load moves such
// a ratio for a correct rule as well, and a quadratic rule would run past go test's 10-minute
// timeout before any ratio was read, where the deadline fails it in 30 s.
func TestURLCredentialsTakesTimeLinearInTheTextsLength(t *testing.T) {
	const deadline = 30 * time.Second
	line := strings.Repeat("a://", (1<<20)/len("a://"))
	done := make(chan string, 1)
	go func() { done <- URLCredentials(line) }()
	select {
	case got := <-done:
		if got != line {
			t.Fatalf("URLCredentials changed a line of a:// repeated, which holds no credential, to one of %d bytes", len(got))
		}
	case <-time.After(deadline):
		t.Fatalf("URLCredentials of 1 MiB of a:// did not return within %v; a rule linear in the length takes tens of milliseconds", deadline)
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
