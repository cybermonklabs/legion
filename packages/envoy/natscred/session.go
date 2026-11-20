// Package natscred is the credential a Go NATS client presents to a hub whose connections the
// auth service admits (internal/cipher/natscallout): a session's broker access token beside a
// proof of its key over the server's nonce (Session), or a service's platform identity token
// (Service). Each comes as the nats.Option a connection is dialled with and a Keeper that holds the
// open connection to a credential the server still admits, reconnecting it before the user the
// service answered with expires.
package natscred

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cipherclient"
)

// Keeper holds an open connection to a live credential: it reconnects nc with a fresh one before
// the user its current one was admitted as expires. Keep returns when ctx ends or nc closes,
// whoever closes it (untilClosed).
type Keeper interface {
	Keep(ctx context.Context, nc *nats.Conn)
}

// untilClosed is ctx, ended as well once nc closes, and the stop that ends it and hands nc's
// status listener back, which each Keep defers: a keeper lives no longer than its connection. It
// watches nc's own CLOSED status event (Conn.StatusChanged), which nats.go sends on every close
// (nats.go v1.53.1, close), and not nc's ClosedCB, which an owner can clear (internal/bus clears
// it on a replacement connection it discards) and which NoCallbacksAfterClientClose skips on the
// owner's own Close. nats.go puts the event in the listener's buffer without waiting for a reader
// (sendStatusEvent), so a close that lands while the keeper is busy is kept until it is read; a
// close before the listener was registered is seen by IsClosed.
func untilClosed(ctx context.Context, nc *nats.Conn) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	closed := nc.StatusChanged(nats.CLOSED)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	if nc.IsClosed() {
		cancel()
	}
	return ctx, func() {
		cancel()
		<-watched
		nc.RemoveStatusListener(closed)
	}
}

// placeholderJWT is the user JWT a session's CONNECT names. nats.go signs the server's nonce only
// for a connection that presents a user JWT or an nkey (connectProto), and a session signs it
// with its proof, so the CONNECT carries this placeholder; a server with no operator discards a
// CONNECT's JWT before it reads it (nats-server server/client.go, processConnect), so the auth
// service is never sent it.
const placeholderJWT = "-"

// signTimeout bounds the trade a CONNECT's signature waits for when the client holds no token
// live enough to present: nats.go signs inside its connect, so a broker that does not answer fails
// that connect, and nats.go dials again, rather than stalling it.
const signTimeout = 10 * time.Second

// Session is the credential of the session client trades for: its broker access token in the
// CONNECT's token field, and in the signature field the proof cipherclient.ConnectionProof signs
// with the client's key over the nonce the server sent this connection. The token is taken once
// per CONNECT: nats.go asks for the signature (SignatureCB) before the token (TokenHandler), so
// the signature stores the token it signed for and the token handler answers that one, and a
// renewal between the two can never send a token the proof does not name. The option sets
// IgnoreAuthErrorAbort: a refused credential is retried, not given up on, so a client whose token
// lapsed while the broker was down connects again once a live one is traded.
//
// The Keeper runs the client's renewal loop (cipherclient.Client.Run) and reconnects the
// connection on each token it lands that is not the one the connection presented, but for the
// first when the presented token outlives that one's renewal. The loop trades on its first pass,
// just after the connection presented a token the client held with more than RenewBefore left
// (cipherclient.Client.Token), and renews RenewBefore ahead of each token it lands
// (Client.RenewalAt). A presented token that lapses after the first token's renewal is kept until
// the loop's second token, so the connection is not cut for nothing; one that lapses before it
// moves to the first, since the server would drop the connection at its expiry before the second
// landed. The service answers a session's user with its token's expiry, so either way the
// connection moves to a later token before the server drops it.
func Session(client *cipherclient.Client) (nats.Option, Keeper) {
	credential := &sessionCredential{
		key:     client.Key(),
		current: client.Token,
		run:     client.Run,
		renewal: client.RenewalAt,
	}
	return credential.option(), credential
}

// sessionCredential is Session's state: where the session's token comes from, the key it is bound
// to, the renewal loop and when it renews a token, and the token the CONNECT in progress presents
// with that token's expiry.
type sessionCredential struct {
	key     *ecdsa.PrivateKey
	current func(context.Context) (*cipherclient.Token, error)
	run     func(context.Context, func(context.Context, *cipherclient.Token) error) error
	renewal func(*cipherclient.Token) time.Time

	mu        sync.Mutex
	presented string
	expiry    time.Time
}

func (c *sessionCredential) option() nats.Option {
	return allOf(
		nats.UserJWT(func() (string, error) { return placeholderJWT, nil }, c.sign),
		nats.TokenHandler(c.token),
		nats.IgnoreAuthErrorAbort(),
	)
}

// sign is the CONNECT's signature over nonce: the proof of the session's key for the token it
// presents, which it takes here, once, and stores with its expiry for token and Keep to read.
func (c *sessionCredential) sign(nonce []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), signTimeout)
	defer cancel()
	token, err := c.current(ctx)
	if err != nil {
		return nil, fmt.Errorf("natscred: no broker access token for this session: %w", err)
	}
	proof, err := cipherclient.ConnectionProof(c.key, token.AccessToken, string(nonce))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.presented, c.expiry = token.AccessToken, token.Expiry
	c.mu.Unlock()
	return []byte(proof), nil
}

// token is the CONNECT's token: the one sign signed for.
func (c *sessionCredential) token() string {
	token, _ := c.presentedToken()
	return token
}

// presentedToken is the token the last CONNECT presented and when it lapses.
func (c *sessionCredential) presentedToken() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presented, c.expiry
}

// Keep runs the renewal loop until ctx ends or nc closes, reconnecting nc on each token it lands
// that nc did not present. The loop's first pass trades at once (Client.Run), just after nc
// presented a token, so the first is skipped when the presented token outlives its renewal
// (Client.RenewalAt): nc then moves at the second, before the presented one lapses. It returns too
// when the broker refuses the session's identity in a way no retry cures
// (cipherclient.RefusedError.Permanent): the connection then keeps the user it has until that
// expires, and each reconnect after asks the broker again.
func (c *sessionCredential) Keep(ctx context.Context, nc *nats.Conn) {
	ctx, stop := untilClosed(ctx, nc)
	defer stop()
	first := true
	_ = c.run(ctx, func(_ context.Context, token *cipherclient.Token) error {
		presented, expiry := c.presentedToken()
		outlives := first && expiry.After(c.renewal(token))
		first = false
		if outlives || token.AccessToken == presented {
			return nil
		}
		if err := nc.ForceReconnect(); err != nil {
			return fmt.Errorf("natscred: reconnect with the renewed token: %w", err)
		}
		return nil
	})
}

// allOf is every option in order, as one.
func allOf(options ...nats.Option) nats.Option {
	return func(o *nats.Options) error {
		for _, option := range options {
			if err := option(o); err != nil {
				return err
			}
		}
		return nil
	}
}
