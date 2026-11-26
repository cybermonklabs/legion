package githubapp

import (
	"net/url"
	"strings"
	"testing"
)

// gitHubOrigin is GitHub's API origin as New parses the default base, the origin nextLink holds a
// link to.
var gitHubOrigin = &url.URL{Scheme: "https", Host: "api.github.com"}

// The next page is fetched with the App's JWT, so a Link header naming another origin is refused
// rather than followed; a header with no rel="next" is the last page.
func TestNextLinkStaysOnTheAPIOrigin(t *testing.T) {
	next, err := nextLink(`<https://api.github.com/app/hook/deliveries?cursor=v1_2&per_page=100>; rel="next", <https://api.github.com/app/hook/deliveries?per_page=100>; rel="first"`, gitHubOrigin)
	if err != nil || next != "https://api.github.com/app/hook/deliveries?cursor=v1_2&per_page=100" {
		t.Fatalf("next page: got %q, %v", next, err)
	}
	if next, err := nextLink(`<https://api.github.com/app/hook/deliveries?per_page=100>; rel="first"`, gitHubOrigin); err != nil || next != "" {
		t.Fatalf("last page: got %q, %v, want no next page", next, err)
	}
	if _, err := nextLink(`<https://collector.example/app/hook/deliveries?cursor=v1_2>; rel="next"`, gitHubOrigin); err == nil || !strings.Contains(err.Error(), "leaves the API origin") {
		t.Fatalf("foreign next page: got %v, want a refusal", err)
	}
}

// A next page link is GitHub's text, and once followed it is the URL of the next request, whose
// path each error that names that request names, so nextLink follows no link longer than
// nextLinkMaxBytes, and its refusal shows the link as ShownBody shows GitHub's text: the class of a
// line that holds text of a GitHub answer with no bound (the deep audit of cybermonklabs/legion#4209
// at 3a21edb2c9, note 1). The start of the link, and its size, are kept. A link at the bound is
// followed.
func TestANextLinkPastItsBoundIsRefusedAndShownToABound(t *testing.T) {
	const base = "https://api.github.com"
	start := base + "/app/hook/deliveries?per_page=100&cursor="
	long := start + strings.Repeat("v", 1<<20)
	next, err := nextLink("<"+long+`>; rel="next"`, gitHubOrigin)
	if err == nil {
		t.Fatalf("a next page link of %d bytes was followed (%d bytes), want a refusal", len(long), len(next))
	}
	text := err.Error()
	t.Logf("bytes=%d", len(text))
	if len(text) > 1024 || !strings.Contains(text, start) || !strings.Contains(text, "bytes)") {
		t.Errorf("the refusal of a next page link of %d bytes is %d bytes and begins %.160q, want at most 1,024 that keep the start of the link and its size", len(long), len(text), text)
	}
	atBound := start + strings.Repeat("v", nextLinkMaxBytes-len(start))
	if next, err := nextLink("<"+atBound+`>; rel="next"`, gitHubOrigin); err != nil || next != atBound {
		t.Errorf("a next page link of %d bytes, the bound: got %d bytes, %v, want it followed", len(atBound), len(next), err)
	}
}
