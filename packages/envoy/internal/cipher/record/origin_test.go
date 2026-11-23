package record

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proof"
)

// TestSameOriginComparesAsTheProofsHtuIs: two of the broker's addresses are one origin when a
// DPoP proof made for one verifies at the other, as the library's htu check normalizes them: the
// scheme and the host in any case, the scheme's default port written or not, and an empty path or
// "/". Another scheme, host or port, a path, or something that is no absolute http(s) URL is
// another origin, or none.
//
// The named revert: compare the two strings, and every row of the first list fails.
func TestSameOriginComparesAsTheProofsHtuIs(t *testing.T) {
	const address = "https://secrets.example"
	for _, same := range []string{
		"https://secrets.example",
		"https://secrets.example:443",
		"https://secrets.example/",
		"https://secrets.example:443/",
		"HTTPS://Secrets.Example",
		"https://SECRETS.EXAMPLE:443/",
	} {
		if !SameOrigin(same, address) || !SameOrigin(address, same) {
			t.Errorf("SameOrigin(%q, %q) = false, want true", same, address)
		}
	}
	if !SameOrigin("http://secrets.example:80", "http://secrets.example/") {
		t.Error("SameOrigin(http://secrets.example:80, http://secrets.example/) = false, want true")
	}
	for _, other := range []string{
		"http://secrets.example",
		"https://secrets.example:8443",
		"https://secrets.example:80",
		"https://other.example",
		"https://secrets.example.evil.example",
		"https://secrets.example/v1",
		"secrets.example",
		"ftp://secrets.example",
		"",
		"https://",
	} {
		if SameOrigin(other, address) || SameOrigin(address, other) {
			t.Errorf("SameOrigin(%q, %q) = true, want false", other, address)
		}
	}
}

// TestAStoredRequestObjectIsReadByTheOriginOfItsAddressForItsKind: a stored record's request
// object is this broker's when its aud names the broker's own address, in any spelling of its
// origin; and, for a machine login alone, when it names the broker's edge address so. A session's
// request signed for the edge is ErrAudienceMismatch, since the edge opens nothing else, and so
// is either kind signed for a third address, or signed for the edge on a broker with no edge.
//
// The named reverts: compare the aud with each address as text, and the spelling rows fail; take
// the edge address for every kind, and the session's request signed for the edge verifies.
func TestAStoredRequestObjectIsReadByTheOriginOfItsAddressForItsKind(t *testing.T) {
	const own, edge = "https://secrets.internal.example", "https://secrets.edge.example"
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	login := []AuthorizationDetail{{Type: KindLauncherCredential, Identifier: "example-host-devbox"}}
	sign := func(aud string, details []AuthorizationDetail) string {
		t.Helper()
		hint := ""
		if details[0].Type == KindLauncherCredential {
			hint = "outside@outside.example"
		}
		compact, err := Sign(key, aud, details, "", hint, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		return compact
	}
	for _, row := range []struct {
		name, aud    string
		details      []AuthorizationDetail
		edgeAudience string
		ours         bool
	}{
		{"a session's request for the broker's own address", own, secretDetail(), edge, true},
		{"a session's request for the own address with its default port", own + ":443", secretDetail(), edge, true},
		{"a machine login for the own address in capitals with a slash", "HTTPS://SECRETS.INTERNAL.EXAMPLE/", login, edge, true},
		{"a machine login for the edge", edge, login, edge, true},
		{"a machine login for the edge with its default port", edge + ":443/", login, edge, true},
		{"a session's request for the edge", edge, secretDetail(), edge, false},
		{"a machine login for the edge of a broker with no edge", edge, login, "", false},
		{"a machine login for a third address", "https://another-broker.internal.example", login, edge, false},
		{"a session's request for another port of the own host", own + ":8443", secretDetail(), edge, false},
		{"a machine login for a path of the edge", edge + "/v1", login, edge, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			obj, err := VerifyStoredRequestObject(sign(row.aud, row.details), own, row.edgeAudience, time.Minute, now)
			switch {
			case row.ours && err != nil:
				t.Fatalf("VerifyStoredRequestObject(aud %s) = %v, want it verified", row.aud, err)
			case row.ours && obj.Audience != row.aud:
				t.Fatalf("VerifyStoredRequestObject(aud %s) answered aud %s, want the object's own", row.aud, obj.Audience)
			case !row.ours && !errors.Is(err, ErrAudienceMismatch):
				t.Fatalf("VerifyStoredRequestObject(aud %s) = %v, want ErrAudienceMismatch", row.aud, err)
			}
		})
	}
	// A new request is still held to its address exactly: VerifyRequestObject is the check
	// Create and LoginDevice make, and it takes no other spelling.
	if _, err := VerifyRequestObject(sign(own+":443", secretDetail()), own, time.Minute, now); !errors.Is(err, ErrAudienceMismatch) {
		t.Fatalf("VerifyRequestObject(aud %s:443) for %s = %v, want ErrAudienceMismatch", own, own, err)
	}
}
