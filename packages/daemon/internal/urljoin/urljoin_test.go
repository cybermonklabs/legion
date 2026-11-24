package urljoin_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// marker is the credential the refused bases and refs below carry in their user information and
// their query; no error of Join may hold it, or any other part of either text.
const marker = "not-a-secret-token"

// TestJoinJoinsThePathOntoTheBasePath: the ref's path is joined onto the base's path, a leading
// slash or none, the base's query is kept ahead of the ref's own, user information and port stay,
// and the fragment goes.
func TestJoinJoinsThePathOntoTheBasePath(t *testing.T) {
	for _, c := range []struct{ name, base, ref, want, path string }{
		{"leading-slash", "http://host/api?key=v", "/v1/x", "http://host/api/v1/x?key=v", "/api/v1/x"},
		{"no-leading-slash", "http://host/api?key=v", "v1/x", "http://host/api/v1/x?key=v", "/api/v1/x"},
		{"ref-query", "http://host/api?key=v", "/v1/x?a=b", "http://host/api/v1/x?key=v&a=b", "/api/v1/x"},
		{"base-no-query", "http://host/api", "v1/x?a=b", "http://host/api/v1/x?a=b", "/api/v1/x"},
		{"userinfo-port", "https://user:pass@host:8443/api", "v1/x", "https://user:pass@host:8443/api/v1/x", "/api/v1/x"},
		{"trailing-slash", "http://host/api/", "v1/x", "http://host/api/v1/x", "/api/v1/x"},
		{"trailing-slash-query", "http://host/api/?key=v", "/v1/x", "http://host/api/v1/x?key=v", "/api/v1/x"},
		{"ref-trailing-slash", "http://host/api", "v1/x/", "http://host/api/v1/x/", "/api/v1/x/"},
		{"no-path", "http://host", "sse", "http://host/sse", "/sse"},
		{"root-path", "http://host/", "/sse", "http://host/sse", "/sse"},
		{"fragment", "http://host/api?key=v#frag", "v1/x", "http://host/api/v1/x?key=v", "/api/v1/x"},
		{"query-as-written", "http://host/api?sig=a%2Bb+c&flag", "v1?y=1", "http://host/api/v1?sig=a%2Bb+c&flag&y=1", "/api/v1"},
		{"empty-ref", "http://host/api/?key=v#frag", "", "http://host/api/?key=v", "/api/"},
		{"query-only", "http://host/api/?key=v", "?a=b", "http://host/api/?key=v&a=b", "/api/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			joined, err := urljoin.Join(c.base, c.ref)
			if err != nil {
				t.Fatalf("Join(%q, %q): %v", c.base, c.ref, err)
			}
			if got := joined.String(); got != c.want {
				t.Errorf("Join(%q, %q) = %q; want %q", c.base, c.ref, got, c.want)
			}
			if joined.Path != c.path {
				t.Errorf("Join(%q, %q).Path = %q; want %q", c.base, c.ref, joined.Path, c.path)
			}
		})
	}
}

// TestJoinKeepsAnEscapedElementAsWritten: an element url.PathEscape wrote reaches the request as
// the concatenation it replaces wrote it - an "@" as is, an escaped slash as %2F, a space and a
// percent escaped once - and the path the server reads is the same.
func TestJoinKeepsAnEscapedElementAsWritten(t *testing.T) {
	for _, c := range []struct{ name, element string }{
		{"at", "example-fleet@example-project.iam.gserviceaccount.com"},
		{"escaped-slash", "a/b"},
		{"space-percent", "a b%c"},
		{"query-fragment", "a?b#c"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ref := "v1/things/" + url.PathEscape(c.element) + ":do"
			joined, err := urljoin.Join("http://host/base", ref)
			if err != nil {
				t.Fatalf("Join(%q): %v", ref, err)
			}
			concatenated := "http://host/base/" + ref
			if got := joined.String(); got != concatenated {
				t.Errorf("Join(%q) = %q; want %q, as the concatenation wrote it", ref, got, concatenated)
			}
			asked, err := url.Parse(joined.String())
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", joined.String(), err)
			}
			if want := "/base/v1/things/" + c.element + ":do"; asked.Path != want {
				t.Errorf("the server reads the path %q; want %q", asked.Path, want)
			}
		})
	}
}

// TestJoinRefusesInWordsThatQuoteNeitherText: a base that does not parse, has no scheme or has no
// host is ErrBase, and a ref that does not parse or holds a scheme, a host or a fragment is ErrRef.
// The texts carry marker in their user information and their query, and neither error holds it.
func TestJoinRefusesInWordsThatQuoteNeitherText(t *testing.T) {
	good := "http://not-a-secret-user:" + marker + "@host/api?token=" + marker
	for _, c := range []struct {
		name, base, ref string
		want            error
	}{
		{"base-unparsed", "http://not-a-secret-user:" + marker + "@host/api%zz?token=" + marker, "v1/x", urljoin.ErrBase},
		{"base-no-scheme", "//not-a-secret-user:" + marker + "@host/api?token=" + marker, "v1/x", urljoin.ErrBase},
		{"base-relative", "/api?token=" + marker, "v1/x", urljoin.ErrBase},
		{"base-no-host", "http:///api?token=" + marker, "v1/x", urljoin.ErrBase},
		{"base-opaque", "mailto:" + marker + "@example.com", "v1/x", urljoin.ErrBase},
		{"base-host-port-only", "host:8080/api?token=" + marker, "v1/x", urljoin.ErrBase},
		{"base-empty", "", "v1/x", urljoin.ErrBase},
		{"ref-unparsed", good, "v1/x%zz?token=" + marker, urljoin.ErrRef},
		{"ref-scheme", good, "http://other/v1/x?token=" + marker, urljoin.ErrRef},
		{"ref-host", good, "//other/v1/x?token=" + marker, urljoin.ErrRef},
		{"ref-empty-host", good, "//", urljoin.ErrRef},
		{"ref-fragment", good, "/v1/x#" + marker, urljoin.ErrRef},
		{"ref-empty-fragment", good, "/v1/x#", urljoin.ErrRef},
		{"ref-colon-first-segment", good, "1a:" + marker, urljoin.ErrRef},
		{"ref-control-byte", good, "v1/x\x7f" + marker, urljoin.ErrRef},
	} {
		t.Run(c.name, func(t *testing.T) {
			joined, err := urljoin.Join(c.base, c.ref)
			if !errors.Is(err, c.want) || joined != nil {
				t.Fatalf("Join(%q, %q) = %v, %v; want nil, %v", c.base, c.ref, joined, err, c.want)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "not-a-secret-user") {
				t.Errorf("Join's error %q quotes the text it refused", err)
			}
		})
	}
}

// TestAnUnparsedTextsErrorQuotesIt: the twin of the refusals: url.Parse's own error for the
// unparsed base quotes its user information and its query, so the row above can see one printed.
func TestAnUnparsedTextsErrorQuotesIt(t *testing.T) {
	base := "http://not-a-secret-user:" + marker + "@host/api%zz?token=" + marker
	if _, err := url.Parse(base); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("url.Parse(%q) = %v; want an error that quotes the marker", base, err)
	}
}

// TestJoinURLLeavesTheBaseAsItWas: JoinURL joins onto a base already parsed and changes nothing
// of it, and refuses a ref Join refuses with ErrRef.
func TestJoinURLLeavesTheBaseAsItWas(t *testing.T) {
	base, err := url.Parse("http://user:pass@host:8080/api/?key=v#frag")
	if err != nil {
		t.Fatal(err)
	}
	before := *base
	joined, err := urljoin.JoinURL(base, "/v1/x?a=b")
	if err != nil {
		t.Fatalf("JoinURL: %v", err)
	}
	if got, want := joined.String(), "http://user:pass@host:8080/api/v1/x?key=v&a=b"; got != want {
		t.Errorf("JoinURL = %q; want %q", got, want)
	}
	if *base != before {
		t.Errorf("JoinURL changed its base to %q", base)
	}
	if joined, err := urljoin.JoinURL(base, "//other/x"); !errors.Is(err, urljoin.ErrRef) || joined != nil {
		t.Errorf("JoinURL(base, %q) = %v, %v; want nil, ErrRef", "//other/x", joined, err)
	}
}
