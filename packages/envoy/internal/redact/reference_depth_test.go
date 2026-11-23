package redact_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// nested is the character reference of name written so that a reader of the page decodes it in
// passes passes: its '&' written as &amp; passes-1 times over (&amp;amp;quest; for 3).
func nested(name string, passes int) string {
	return "&" + strings.Repeat("amp;", passes-1) + name + ";"
}

// An issuer's refusal page whose references nest as deep as redact.ReferencePasses reads, and
// prints with its URL's credential removed; one nested a level deeper, which no page writes, is a
// body the rule cannot read: the boundary prints it as redact.WordsNotReadable, never its words
// (redact.UnescapeHTML answers it is not readable).
func TestTheBoundaryPrintsABodyWhoseReferencesNestPastTheBoundAsNotReadable(t *testing.T) {
	for _, row := range []struct {
		name     string
		passes   int
		readable bool
	}{
		{"at-bound", redact.ReferencePasses, true},
		{"past-bound", redact.ReferencePasses + 1, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			body := "<p>clone https://" + redacttest.Token + nested("commat", row.passes) + "github.example/o/r failed</p>"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			cfg := &oauth2.Config{ClientID: "probe-client", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
			_, err := redact.OAuth2(cfg.Exchange(context.Background(), "the-code"))
			if err == nil {
				t.Fatal("OAuth2 answered no error")
			}
			printed := err.Error()
			redacttest.RequireNone(t, "redact.body-reference-depth/"+row.name, printed)
			if row.readable && !strings.Contains(printed, "<p>clone https://github.example/o/r failed</p>") {
				t.Errorf("the body nested %d deep printed %q, want its words with the URL's user information removed", row.passes, printed)
			}
			if !row.readable && (!strings.Contains(printed, redact.WordsNotReadable) || strings.Contains(printed, "clone")) {
				t.Errorf("the body nested %d deep printed %q, want %q in place of its words", row.passes, printed, redact.WordsNotReadable)
			}
		})
	}
}

// readBound is how long one read of a hostile field below may take. Each read makes at most
// redact.ReferencePasses+1 passes over its field; at a load average of 130 to 180 on 288 CPUs
// (this box runs at 100 to 1200), GOMAXPROCS 8, the slowest, Remote of the 256 KiB &ampamp...;
// field, took at most 177 ms, and the bound is more than 100 times that. Decoding until no
// reference was left, as the rule did before, made a pass for each level the field nests: at a
// load average of 100 to 226, Remote took 5 min 24 s on the 1 MiB &amp;amp;... field and 21 min
// 47 s on the 256 KiB &ampamp...; one, UnescapeHTML 7 min 51 s on the 2 MiB &amp;amp;... field
// and 4 min 4 s on the 1 MiB &ampamp...; one, so each row fails at the bound rather than waiting
// that out.
const readBound = 20 * time.Second

// Another party writes the field, so a page or an answer can nest one reference as deep as its
// length allows: &amp;amp;amp;... or &ampampamp...;, which a decoding until no reference is left
// reads one level per pass, each pass the field's whole length, a time quadratic in it. Each of
// the rule's decodings, Remote's step 0 and UnescapeHTML, makes at most redact.ReferencePasses+1
// passes, so each field below, 256 KiB to 2 MiB (the review timed 64 KiB and 32 KiB), reads
// within readBound, and Remote answers it is not readable. Each read runs in a goroutine of its
// own, so a read past the bound fails its row at the bound.
func TestNestedReferencesAreReadInBoundedTime(t *testing.T) {
	ampSemicolon := func(size int) string { return "&" + strings.Repeat("amp;", size/4) }
	legacyAmp := func(size int) string { return "&" + strings.Repeat("amp", size/3) + ";" }
	// Each read answers what is wrong with its answer, or "".
	remote := func(field string) string {
		if shown, ok := redact.Remote(field); ok || shown != "" {
			return fmt.Sprintf("Remote read it as readable=%t, %.40q..., want not readable", ok, shown)
		}
		return ""
	}
	unescape := func(field string) string { redact.UnescapeHTML(field); return "" }
	for _, row := range []struct {
		read, shape, field string
		check              func(string) string
	}{
		{"Remote", "&amp;amp;...", ampSemicolon(1 << 20), remote},
		{"Remote", "&ampamp...;", legacyAmp(256 << 10), remote},
		{"UnescapeHTML", "&amp;amp;...", ampSemicolon(2 << 20), unescape},
		{"UnescapeHTML", "&ampamp...;", legacyAmp(1 << 20), unescape},
	} {
		began := time.Now()
		done := make(chan string, 1)
		go func() { done <- row.check(row.field) }()
		select {
		case wrong := <-done:
			t.Logf("%s of the %s field (%d bytes) took %s", row.read, row.shape, len(row.field), time.Since(began))
			if wrong != "" {
				t.Errorf("the %s field: %s", row.shape, wrong)
			}
		case <-time.After(readBound):
			t.Errorf("%s of the %s field (%d bytes) took more than %s", row.read, row.shape, len(row.field), readBound)
		}
	}
}
