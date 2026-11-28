package text

import (
	"reflect"
	"testing"
)

// twoOrigins is a Dispatch that answers at a canonical origin and at a second served address.
var twoOrigins = NewOrigins("https://a.example.test", "https://b.example.test")

// A dashboard URL of either origin a Dispatch answers at is the reference its `dispatch://` form
// is: an issue page names the issue, and a spec page's block fragment names the block, whichever
// origin wrote the link. Every reader takes the same origins: Extract, ParseRef and Mentions.
func TestADashboardURLOfEitherOriginIsAReference(t *testing.T) {
	for _, tc := range []struct {
		dispatch string
		pages    []string
	}{
		{"dispatch://ABC-1", []string{"https://a.example.test/issues/ABC-1", "https://b.example.test/issues/ABC-1"}},
		{"dispatch://ABC-1/spec#b-x", []string{"https://a.example.test/issues/ABC-1/spec#b-x", "https://b.example.test/issues/ABC-1/spec#b-x"}},
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
	for _, page := range []string{"https://other.example.test/issues/ABC-1", "http://b.example.test/issues/ABC-1", "https://b.example.test.other.example.test/issues/ABC-1"} {
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
	got := Extract("dispatch://ABC-1 https://a.example.test/issues/ABC-1", Origins{})
	if len(got) != 2 || got[0].Kind != "issue" || got[0].IssueKey != "ABC-1" || got[1].Kind != "url" {
		t.Fatalf("Extract with no origins = %+v, want the issue, then the dashboard URL as a URL", got)
	}
}

// NewOrigins skips a value that is empty or names no scheme and host, so a list with such a value
// reads exactly the others, and a list of nothing else names no origin.
func TestNewOriginsSkipsWhatNamesNoOrigin(t *testing.T) {
	if hosts := NewOrigins("", "not a url").Hosts(); len(hosts) != 0 {
		t.Fatalf("NewOrigins of an empty value and one with no scheme or host names the hosts %q, want none", hosts)
	}
	origins := NewOrigins("https://a.example.test", "", "not a url", "https://b.example.test")
	if got := Extract("https://b.example.test/issues/ABC-1", origins); len(got) != 1 || got[0].Kind != "issue" {
		t.Fatalf("Extract of the second origin = %+v, want the issue", got)
	}
	if got := Extract("https://a.example.test/issues/ABC-1", NewOrigins("https://a.example.test")); len(got) != 1 || got[0].Kind != "issue" {
		t.Fatalf("Extract with the canonical origin alone = %+v, want the issue", got)
	}
}

// An origin with a base path keeps it: a URL is read against the origin whose scheme and host it
// has, with that origin's own path, so one Dispatch at a path and another at a host both read.
func TestEachOriginKeepsItsOwnBasePath(t *testing.T) {
	origins := NewOrigins("https://a.example.test/dispatch/", "https://b.example.test")
	for page, want := range map[string]string{
		"https://a.example.test/dispatch/issues/ABC-1": "issue",
		"https://a.example.test/issues/ABC-1":          "url",
		"https://b.example.test/issues/ABC-1":          "issue",
		"https://b.example.test/dispatch/issues/ABC-1": "url",
	} {
		if got := Extract(page, origins); len(got) != 1 || got[0].Kind != want {
			t.Errorf("Extract(%q) = %+v, want kind %s", page, got, want)
		}
	}
}

// The server's list (ServerOrigins) reads a dashboard link at its canonical origin, at a served
// address and at a former address as the reference its page names, the link's scheme and host
// compared without regard to case, as the web page compares them; and a link at another host, or at
// one of those hosts under another scheme, as the URL it is. Every reader takes the same list:
// Extract and ParseRef.
func TestALinkAtAServedOrFormerAddressIsAReferenceInAnyCase(t *testing.T) {
	origins := ServerOrigins("https://a.example.test", []string{"https://b.example.test"}, []string{"http://old.example.test:8080"})
	for _, row := range []struct {
		name, page string
		reference  bool
	}{
		{"the canonical origin", "https://a.example.test/issues/ABC-1", true},
		{"a served address", "https://b.example.test/issues/ABC-1", true},
		{"a former address", "http://old.example.test:8080/issues/ABC-1", true},
		{"the canonical origin's host in upper case", "https://A.EXAMPLE.TEST/issues/ABC-1", true},
		{"a served address's host in mixed case", "https://B.Example.Test/issues/ABC-1", true},
		{"a former address's host in mixed case", "http://Old.EXAMPLE.test:8080/issues/ABC-1", true},
		{"a served address with its scheme's default port written out", "https://b.example.test:443/issues/ABC-1", true},
		{"the canonical origin with its default port in upper case", "https://A.EXAMPLE.TEST:443/issues/ABC-1", true},
		{"a served address's host at another port", "https://b.example.test:8443/issues/ABC-1", false},
		{"another host", "https://other.example.test/issues/ABC-1", false},
		{"another host in upper case", "https://OTHER.EXAMPLE.TEST/issues/ABC-1", false},
		{"a served address's host under another scheme", "http://b.example.test/issues/ABC-1", false},
		{"a former address's host under another scheme", "https://old.example.test:8080/issues/ABC-1", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := Extract("see "+row.page+" now", origins)
			ref, ok := ParseRef(row.page, origins)
			if row.reference {
				if len(got) != 1 || got[0].Kind != "issue" || got[0].IssueKey != "ABC-1" {
					t.Errorf("Extract(%q) = %+v, want the issue ABC-1", row.page, got)
				}
				if !ok || ref.Kind != "issue" || ref.IssueKey != "ABC-1" {
					t.Errorf("ParseRef(%q) = %+v, %t; want the issue ABC-1", row.page, ref, ok)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != "url" || got[0].ID != row.page {
				t.Errorf("Extract(%q) = %+v, want the URL it is", row.page, got)
			}
			if ok {
				t.Errorf("ParseRef(%q) = %+v, want no reference", row.page, ref)
			}
		})
	}
}

// A head cut (HeadRefs: an Inbox subject, a candidate's first line, an excerpt) reads the server's
// list too: a cut that falls inside a link at the canonical, a served or a former address moves
// back before the link, as one inside any reference does, and a cut inside a link at another
// address is a cut by length, since that link names nothing Dispatch holds.
func TestAHeadCutKeepsALinkAtAServedOrFormerAddressWhole(t *testing.T) {
	origins := ServerOrigins("https://a.example.test", []string{"https://b.example.test"}, []string{"https://old.example.test"})
	const limit = 30
	for page, reference := range map[string]bool{
		"https://a.example.test/issues/ABC-1234":     true,
		"https://b.example.test/issues/ABC-1234":     true,
		"https://old.example.test/issues/ABC-1234":   true,
		"https://other.example.test/issues/ABC-1234": false,
	} {
		value := "see " + page + " now"
		want := HeadRunes(value, limit-1) + "…"
		if reference {
			want = "see…"
		}
		if got := HeadRefs(value, limit, "…", origins); got != want {
			t.Errorf("HeadRefs(%q, %d) = %q, want %q", value, limit, got, want)
		}
	}
}
