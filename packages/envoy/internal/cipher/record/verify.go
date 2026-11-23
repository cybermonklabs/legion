package record

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// ErrBodyIDMismatch is VerifyBodyReproducesID's error when a stored body parses cleanly but its
// own content-addressed id does not equal the id it was stored under — the one failure ParseBody
// itself cannot detect, since ParseBody has no id to compare against.
var ErrBodyIDMismatch = errors.New("stored body does not reproduce its own id")

// VerifyBodyReproducesID parses canonical and confirms it reproduces recordID's own hash. Every
// caller holding a record id and its stored body shares this exact "parse, then compare id" check
// — ChainVerifier.Verify, requests.Machine's pre-decision check, and machine.Service's own — rather
// than each re-implementing record.ParseBody plus a hand-rolled ID comparison.
func VerifyBodyReproducesID(canonical, recordID string) (Body, error) {
	body, err := ParseBody(canonical)
	if err != nil {
		return Body{}, err
	}
	if body.ID() != recordID {
		return Body{}, ErrBodyIDMismatch
	}
	return body, nil
}

// VerifyStoredRequestObject is VerifyRequestObject for the request object a stored record embeds,
// as of createdAt, the record's own creation, which every reader of a stored record calls: its
// lookup, its decision, the pending lists and the chain a grant or a launcher credential is
// verified by. The object is this broker's when its aud is the origin of audience, the broker's
// own address (BROKER_PUBLIC_URL), and, for a machine login alone (a launcher_credential detail),
// the origin of edgeAudience, the broker's address at its edge (BROKER_EDGE_URL) and "" for a
// broker with no edge. The edge opens machine logins and nothing else, so a session's request
// signed for the edge is no request of this broker. An aud is the address it names when it is
// that address's text or, as SameOrigin compares them, its origin, so a record whose object names
// https://secrets.example:443 is read back by a broker whose setting is https://secrets.example.
// An object whose aud is anything else is ErrAudienceMismatch.
func VerifyStoredRequestObject(compact, audience, edgeAudience string, skew time.Duration, createdAt time.Time) (RequestObject, error) {
	names := func(aud, address string) bool { return aud == address || SameOrigin(aud, address) }
	return verifyRequestObject(compact, func(aud string, details []AuthorizationDetail) bool {
		if names(aud, audience) {
			return true
		}
		return edgeAudience != "" && len(details) > 0 && details[0].Type == KindLauncherCredential && names(aud, edgeAudience)
	}, skew, createdAt)
}

// SameOrigin reports whether a and b, two of a broker's addresses (BROKER_PUBLIC_URL,
// BROKER_EDGE_URL, or the aud a request object names), are one origin as a DPoP proof's htu is
// compared (RFC 9449 section 4.3, after RFC 3986 section 6.2's normalization): each an absolute
// http or https URL with a host and a path of "" or "/", the two with one scheme and one host
// compared without regard to case, the scheme's default port (443 for https, 80 for http) the
// same as none. A user, a query or a fragment is not compared: the broker rebuilds the URL a
// proof names from an address's scheme and host alone. Anything else, a URL with another path
// among it, is the same as nothing.
func SameOrigin(a, b string) bool {
	x, ok := origin(a)
	if !ok {
		return false
	}
	y, ok := origin(b)
	return ok && x == y
}

// defaultPorts are the ports an origin leaves out, by scheme, each with its colon.
var defaultPorts = map[string]string{"http": ":80", "https": ":443"}

// origin is raw's scheme and host as SameOrigin compares them, and whether raw has an origin.
func origin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	// url.Parse lowers the scheme itself.
	if err != nil || defaultPorts[u.Scheme] == "" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	return u.Scheme + "://" + strings.TrimSuffix(strings.ToLower(u.Host), defaultPorts[u.Scheme]), true
}
