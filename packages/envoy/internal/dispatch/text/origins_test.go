package text

import (
	"reflect"
	"testing"
)

// twoOrigins is a Dispatch that answers at a canonical origin and at an outside origin.
var twoOrigins = NewOrigins("https://canon.test", "https://outside.test")

// A dashboard URL of either origin a Dispatch answers at is the reference its `dispatch://` form
// is: an issue page names the issue, and a spec page's block fragment names the block, whichever
// origin wrote the link. Every reader takes the same origins: Extract, ParseRef and Mentions.
func TestADashboardURLOfEitherOriginIsAReference(t *testing.T) {
	for _, tc := range []struct {
		dispatch string
		pages    []string
	}{
		{"dispatch://ABC-1", []string{"https://canon.test/issues/ABC-1", "https://outside.test/issues/ABC-1"}},
		{"dispatch://ABC-1/spec#b-x", []string{"https://canon.test/issues/ABC-1/spec#b-x", "https://outside.test/issues/ABC-1/spec#b-x"}},
	} {
		want := Extract(tc.dispatch, twoOrigins)
		if len(want) != 1 || want[0].Kind == "url" {
			t.Fatalf("Extract(%q) = %+v, want one reference", tc.dispatch, want)
		}
		for _, page := range tc.pages {
			if got := Extract("see "+page+" now", twoOrigins); !reflect.DeepEqual(got, want) {
				t.Errorf("Extract(%q) = %+v, want %+v, as %s reads", page, got, want, tc.dispatch)
			}
			if got, ok := ParseRef(page, twoOrigins); !ok || !reflect.DeepEqual(got, want[0]) {
				t.Errorf("ParseRef(%q) = %+v, %t; want %+v", page, got, ok, want[0])
			}
			located, err := Mentions(page, twoOrigins, nil)
			if err != nil || len(located) != 1 || !reflect.DeepEqual(located[0].Ref, want[0]) {
				t.Errorf("Mentions(%q) = %+v, %v; want %+v", page, located, err, want[0])
			}
		}
	}
}

// A URL of a host that is no origin of this Dispatch is an ordinary link, kept as a URL reference,
// however Dispatch-shaped its path; and so is one of an origin's host under another scheme.
func TestAURLOfAnotherHostIsNoReference(t *testing.T) {
	for _, page := range []string{"https://evil.test/issues/ABC-1", "http://outside.test/issues/ABC-1", "https://outside.test.evil/issues/ABC-1"} {
		got := Extract(page, twoOrigins)
		if len(got) != 1 || got[0].Kind != "url" || got[0].ID != page {
			t.Errorf("Extract(%q) = %+v, want the URL it is", page, got)
		}
		if ref, ok := ParseRef(page, twoOrigins); ok {
			t.Errorf("ParseRef(%q) = %+v, want no reference", page, ref)
		}
	}
}

// With no origins, text reads only `dispatch://` references: a dashboard URL is a URL.
func TestNoOriginsReadsOnlyDispatchReferences(t *testing.T) {
	got := Extract("dispatch://ABC-1 https://canon.test/issues/ABC-1", Origins{})
	if len(got) != 2 || got[0].Kind != "issue" || got[0].IssueKey != "ABC-1" || got[1].Kind != "url" {
		t.Fatalf("Extract with no origins = %+v, want the issue, then the dashboard URL as a URL", got)
	}
}

// NewOrigins skips a value that is empty or names no scheme and host, so a deployment with no
// outside origin reads exactly the canonical one.
func TestNewOriginsSkipsWhatNamesNoOrigin(t *testing.T) {
	origins := NewOrigins("https://canon.test", "", "not a url", "https://outside.test")
	if got := Extract("https://outside.test/issues/ABC-1", origins); len(got) != 1 || got[0].Kind != "issue" {
		t.Fatalf("Extract of the outside origin = %+v, want the issue", got)
	}
	if got := Extract("https://canon.test/issues/ABC-1", NewOrigins("https://canon.test")); len(got) != 1 || got[0].Kind != "issue" {
		t.Fatalf("Extract with the canonical origin alone = %+v, want the issue", got)
	}
}

// Canonical is the canonical origin alone, never the outside one: a canonical value that names no
// origin leaves Canonical empty, as a deployment with no DISPATCH_SERVER_URL had it, though the
// outside origin is still read. Every caller of Canonical builds the deployment's own links and
// first lines from it, so an outside origin taken for it would be written into them.
func TestCanonicalIsNeverTheOutsideOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins Origins
		want    string
	}{
		{"both origins", twoOrigins, "https://canon.test"},
		{"a canonical origin at a base path", NewOrigins("https://canon.test/dispatch/", "https://outside.test"), "https://canon.test/dispatch/"},
		{"no canonical value", NewOrigins("", "https://outside.test"), ""},
		{"a canonical value that names no origin", NewOrigins("not a url", "https://outside.test"), ""},
		{"no origins", Origins{}, ""},
	} {
		if got := tc.origins.Canonical(); got != tc.want {
			t.Errorf("%s: Canonical() = %q, want %q", tc.name, got, tc.want)
		}
	}
	outsideOnly := NewOrigins("", "https://outside.test")
	if got := Extract("https://outside.test/issues/ABC-1", outsideOnly); len(got) != 1 || got[0].Kind != "issue" {
		t.Errorf("Extract of the outside origin with no canonical one = %+v, want the issue", got)
	}
	if got := outsideOnly.Hosts(); !reflect.DeepEqual(got, []string{"outside.test"}) {
		t.Errorf("Hosts() with no canonical origin = %q, want the outside origin's host", got)
	}
}

// An origin with a base path keeps it: a URL is read against the origin whose scheme and host it
// has, with that origin's own path, so one Dispatch at a path and another at a host both read.
func TestEachOriginKeepsItsOwnBasePath(t *testing.T) {
	origins := NewOrigins("https://canon.test/dispatch/", "https://outside.test")
	for page, want := range map[string]string{
		"https://canon.test/dispatch/issues/ABC-1":   "issue",
		"https://canon.test/issues/ABC-1":            "url",
		"https://outside.test/issues/ABC-1":          "issue",
		"https://outside.test/dispatch/issues/ABC-1": "url",
	} {
		if got := Extract(page, origins); len(got) != 1 || got[0].Kind != want {
			t.Errorf("Extract(%q) = %+v, want kind %s", page, got, want)
		}
	}
}
