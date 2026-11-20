package record

import (
	"errors"
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
// as of createdAt, the record's own creation: against audience, the broker's own address
// (BROKER_PUBLIC_URL), and, where edgeAudience is set (BROKER_EDGE_URL, the broker's address at
// its edge), against that when the object names it instead. A machine login the edge opened was
// held to the edge's address when it was made and is this broker's as much as one made on its own
// address, so the routes that read it back — its lookup, its decision, the pending lists, the
// launcher credential's chain — take it. An edgeAudience of "" is a broker with no edge, which
// takes audience alone, as VerifyRequestObject does; an object of any third address is
// ErrAudienceMismatch either way.
func VerifyStoredRequestObject(compact, audience, edgeAudience string, skew time.Duration, createdAt time.Time) (RequestObject, error) {
	obj, err := VerifyRequestObject(compact, audience, skew, createdAt)
	if errors.Is(err, ErrAudienceMismatch) && edgeAudience != "" {
		return VerifyRequestObject(compact, edgeAudience, skew, createdAt)
	}
	return obj, err
}
