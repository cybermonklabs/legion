package proxy

import (
	"crypto/ecdsa"
	"errors"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/dpopproof"
)

// mintedTransport is what a request to a host whose rules entry is of kind dpop leaves the proxy
// through: a capability the broker mints for a grant and holds (requests.MintedKind), so that no
// process of the command ever holds the access token or the key it is bound to. serveBound has
// already swapped the grant's token in for the placeholder (proxyrules.Rewrite); this signs the
// DPoP proof that token is bound to, with the pod's own key (Options.DPoPKey), and hands the
// request to base.
//
// The proof names origin, which is https:// and the bound host, and the request's path, and
// never the address the request is sent to (dpopproof.Sign): Dispatch checks a proof's htu
// against its own public URL, wherever the request arrives. It replaces any DPoP header the
// command sent (Header.Set), so the command's own proof never reaches Dispatch.
//
// It is the second guard behind the swap, and it costs one comparison: a request whose
// Authorization is not exactly one DPoP <token>, or whose token is still a placeholder, is
// refused with nothing sent, so a swap that ever left the placeholder in, or left a second
// Authorization beside the token, never reaches Dispatch.
type mintedTransport struct {
	// key is the pod's DPoP key; nil refuses every request, since no proof can be made.
	key *ecdsa.PrivateKey
	// origin is https:// and the bound host, the origin every proof names.
	origin string
	// base sends the signed request: the proxy's upstream transport, or the private destination's
	// for its bound host (serveBound).
	base http.RoundTripper
}

// errMintedAuthorization is mintedTransport's refusal of a request whose Authorization is not one
// DPoP header carrying a token the broker minted.
var errMintedAuthorization = errors.New("the airlock sends a minted capability's request only with one Authorization header, DPoP and the token the broker minted, and this one is not that, so nothing was sent")

// errNoDPoPKey is mintedTransport's refusal when the proxy was given no key to sign with.
var errNoDPoPKey = errors.New("the broker holds no DPoP key to sign this request with, so nothing was sent")

// RoundTrip signs r's proof and sends a copy of r: the caller's request is left as it was, as
// http.RoundTripper asks.
func (t *mintedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	values := r.Header.Values("Authorization")
	token, scheme := "", false
	if len(values) == 1 {
		token, scheme = strings.CutPrefix(values[0], "DPoP ")
	}
	if !scheme || token == "" || strings.HasPrefix(token, placeholder.Prefix) {
		closeBody(r)
		return nil, errMintedAuthorization
	}
	if t.key == nil {
		closeBody(r)
		return nil, errNoDPoPKey
	}
	proof, err := dpopproof.Sign(t.key, r.Method, t.origin+r.URL.EscapedPath(), "", dpopproof.TokenHash(token))
	if err != nil {
		closeBody(r)
		return nil, err
	}
	signed := r.Clone(r.Context())
	signed.Header.Set("DPoP", proof)
	return t.base.RoundTrip(signed)
}

// closeBody closes a request body a RoundTrip refused, which http.RoundTripper asks of every
// RoundTrip, including one that returns an error.
func closeBody(r *http.Request) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
}
