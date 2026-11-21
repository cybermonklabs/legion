// packages/envoy/internal/cipherclient/tokentransport.go

package cipherclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// refusalReadLimit bounds what tokenTransport reads of a 401's body to find its code: a refusal is
// one small JSON object, and whatever a longer body holds past this is handed on unread.
const refusalReadLimit = 64 << 10

// tokenTransport presents a Client's token, beside a proof of the key it is bound to, on every
// request it carries.
type tokenTransport struct {
	client *Client
	// signed is NewDPoPTransport over the client's key, one for the transport's life, so the
	// nonce a server sends is carried in every later proof.
	signed http.RoundTripper
}

// NewTokenTransport is a RoundTripper that presents client's token on every request it sends
// through base (http.DefaultTransport when nil): it sets `Authorization: DPoP <token>` on a copy
// of the request and sends that copy through NewDPoPTransport(client.Key(), base), so the
// request's proof names the request's own URL and carries ath, the hash of the token beside it.
// It is how a caller presents a Client's token to Dispatch or the listener.
//
// The token is the one client holds while that token is good for liveMargin more, read without
// waiting on a trade in flight, so a renewal the broker is slow to answer, or never answers,
// holds up no request while a live token is in hand. Only a request that finds none asks
// client.Token, which trades. A caller renews off the request path by running client.Run beside
// the transport, as the Legion daemon does; without it, the first request that finds no token
// good for liveMargin trades for the next one. Either way nothing of the caller restarts across
// a renewal.
//
// A trade that fails sends nothing: the request answers Token's error, so no request goes out
// without a token. A 401 whose JSON code is BROKER_TOKEN_NOT_ACTIVE, which the server answers a
// token the broker ended before it lapsed, has client drop the token it was presented (Drop), so
// the next request trades again rather than present a token the server no longer takes until its
// renewal comes due. Every answer is handed on as it came, its body whole.
func NewTokenTransport(client *Client, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &tokenTransport{client: client, signed: NewDPoPTransport(client.Key(), base)}
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	token := t.client.live()
	if token == nil {
		var err error
		if token, err = t.client.Token(r.Context()); err != nil {
			// A RoundTripper closes the body it is given, whether it sends the request or not.
			if r.Body != nil {
				r.Body.Close()
			}
			return nil, err
		}
	}
	presented := r.Clone(r.Context())
	presented.Header.Set("Authorization", "DPoP "+token.AccessToken)
	resp, err := t.signed.RoundTrip(presented)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && refusedAsNotActive(resp) {
		t.client.Drop(token)
		t.client.logf(r.Context(), "a server answered that the broker's token presented to it is not live; the next request trades again",
			"url", presented.URL.Redacted(), "code", oauthwire.TokenNotActiveCode)
	}
	return resp, err
}

// refusedAsNotActive reports whether resp's body is a refusal whose code is
// oauthwire.TokenNotActiveCode, the code Dispatch and the Envoy listener answer a token the broker
// does not know, revoked, or let expire. It
// reads at most refusalReadLimit of the body and puts what it read back in front of the rest, so
// whoever reads the answer reads all of it.
func refusedAsNotActive(resp *http.Response) bool {
	body := resp.Body
	read, err := io.ReadAll(io.LimitReader(body, refusalReadLimit))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(read), body), body}
	var answer struct {
		Code string `json:"code"`
	}
	return err == nil && json.Unmarshal(read, &answer) == nil && answer.Code == oauthwire.TokenNotActiveCode
}
