package api

import (
	"crypto/rand"
	"net/http"
	"testing"
)

// TestADPoPProofForAPathTooLongToRecordIsTheCallersRefusal (the final review's item 1): a broker
// token's proof names the request's URL as its htu, which the library does not bound, and the
// replay store keys its record on jti and htu. A path long enough that the two do not fit the
// primary key's btree failed the insert; the library reported that as its server_error, and the
// check answered 503 BROKER_UNAVAILABLE with a warning blaming the broker. The store now refuses
// such a proof before the insert (store.ErrUnstorableProof), and the check answers it 401
// BROKER_TOKEN_PROOF_INVALID, the caller's refusal. A long path that fits still reaches its
// handler: GET /api/v1/issues/<key> of a key nobody holds answers 404. The keys are random text
// (rand.Text), which PostgreSQL cannot compress inside the index row.
//
// Control in store/dpop_proofs.go: the length arm dropped (the 3,000-character key answers 503
// BROKER_UNAVAILABLE).
func TestADPoPProofForAPathTooLongToRecordIsTheCallersRefusal(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-whose-requests-name-long-paths"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	key := func(length int) string {
		var key string
		for len(key) < length {
			key += rand.Text()
		}
		return key[:length]
	}
	// The fitting key keeps the proof's jti and htu (this server's URL, the route's prefix and the
	// key) inside the store's bound of 2,048 bytes; the long one is the review's 3,000 characters.
	fits := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/issues/"+key(1900), token, nil))
	if fits.Code != http.StatusNotFound {
		t.Fatalf("GET of an issue key of 1,900 random characters answered %d %s, want 404: a path the store records reaches its handler", fits.Code, fits.Body.String())
	}
	long := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/issues/"+key(3000), token, nil))
	refusal := decodeBody[refusalAnswer](t, long)
	if long.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("GET of an issue key of 3,000 random characters answered %d %+v, want 401 BROKER_TOKEN_PROOF_INVALID, the caller's refusal, never the broker's 503", long.Code, refusal)
	}
}
