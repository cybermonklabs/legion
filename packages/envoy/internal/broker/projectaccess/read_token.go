package projectaccess

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// ReadTokenConfig is where the broker trades for its own read token, the Credential its client of
// Dispatch presents.
type ReadTokenConfig struct {
	// BrokerURL is the broker's own public URL (BROKER_PUBLIC_URL), which every trade's proof
	// names, since the token endpoint checks a proof against it whatever address the trade
	// reached it at.
	BrokerURL string
	// HTTP is the client every trade goes through; nil is http.DefaultClient. The broker gives one
	// whose transport connects to its own listener (brokerclient.AtAddress), so a trade reaches
	// this process and never waits on a Service that does not yet route to it.
	HTTP *http.Client
	// SubjectFile is BROKER_PROJECT_ACCESS_TOKEN_FILE: the broker's projected Kubernetes token for
	// the exchange's audience (BROKER_EXCHANGE_K8S_AUDIENCE), read at each trade, never at start,
	// since the kubelet rewrites it in place. It is traded, never presented to Dispatch.
	SubjectFile string
	// Log records each trade and each failed renewal, never a token. Nil logs nothing.
	Log *slog.Logger
	// Now and After are time.Now and time.After unless a test replaces them.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

// ReadToken is the broker's own read token at Dispatch (Credential): an access token the broker
// issues to itself at its own token endpoint, trading the projected token in its subject file
// under its read entry (BROKER_EXCHANGE_READ_ACCOUNTS), whose service is the broker's account and
// which fixes the token's session, so a trade names none. The token has the scope read alone, no
// person and no project, which is also why no trade of it asks Dispatch anything: the token
// endpoint asks Dispatch about a person or a project a token names, and this one names neither.
//
// It is brokerclient's own exchange, renewal and key: the key is made here and never leaves the
// process; a token in hand is answered while it is good for longer than
// brokerclient.DefaultRenewBefore, five minutes, and a request finding none trades within its own
// deadline; Run trades again on a timer five minutes before each token expires.
type ReadToken struct {
	client *brokerclient.Client
}

// NewReadToken is the read token cfg describes. It makes its key and asks nothing of the network:
// the broker builds it before it serves, and trades only once it does (Run, or the first request
// that needs a token).
func NewReadToken(cfg ReadTokenConfig) (*ReadToken, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("projectaccess: make the read token's key: %w", err)
	}
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: brokeraddr.New(cfg.BrokerURL),
		Key:       key,
		Subject:   subjectFile{brokerclient.NewProjectedToken(cfg.SubjectFile)},
		// A Kubernetes subject's trade mints no launcher credential, and declining says so
		// plainly: the read token enrols nothing.
		NoLauncherCredential: true,
		HTTP:                 cfg.HTTP,
		Log:                  cfg.Log,
		Now:                  cfg.Now,
		After:                cfg.After,
	})
	if err != nil {
		return nil, fmt.Errorf("projectaccess: the read token's client: %w", err)
	}
	return &ReadToken{client: client}, nil
}

// Key is the key every read token is bound to.
func (r *ReadToken) Key() *ecdsa.PrivateKey { return r.client.Key() }

// Token is a live read token, had within ctx: the token in hand, or one traded now. A caller whose
// ctx ends while another call's trade holds the client stops waiting then, so no request waits
// past its own deadline for a token. A projected token it cannot read, and a trade its token
// endpoint refused in a way no retry cures (brokerclient.RefusedError.Permanent), are the broker's
// own credential broken, ErrBrokerRefused; every other failure is a failure to ask, a trade the
// endpoint did not answer or answered 5xx or 429, or ctx's end.
func (r *ReadToken) Token(ctx context.Context) (string, error) {
	type traded struct {
		token *brokerclient.Token
		err   error
	}
	answered := make(chan traded, 1)
	go func() {
		token, err := r.client.Token(ctx)
		answered <- traded{token, err}
	}()
	var got traded
	select {
	case got = <-answered:
	case <-ctx.Done():
		return "", fmt.Errorf("projectaccess: have the broker's read token: %w", ctx.Err())
	}
	var refused *brokerclient.RefusedError
	switch {
	case got.err == nil:
		return got.token.AccessToken, nil
	case errors.Is(got.err, errSubjectUnreadable):
		return "", fmt.Errorf("%w: BROKER_PROJECT_ACCESS_TOKEN_FILE could not be read: %w", ErrBrokerRefused, got.err)
	case errors.As(got.err, &refused) && refused.Permanent():
		return "", fmt.Errorf("%w: the broker's token endpoint refused to trade the projected token in BROKER_PROJECT_ACCESS_TOKEN_FILE for its read token: %w", ErrBrokerRefused, got.err)
	}
	return "", fmt.Errorf("projectaccess: have the broker's read token: %w", got.err)
}

// Run keeps a read token live until ctx ends, trading again five minutes before each expires
// (brokerclient.Client.Run). It stops on a refusal no retry cures, which it answers; each later
// request then trades again itself. A cancelled ctx is no error.
func (r *ReadToken) Run(ctx context.Context) error {
	return r.client.Run(ctx, func(context.Context, *brokerclient.Token) error { return nil })
}

// errSubjectUnreadable marks a projected token the read token could not read, which no trade sent.
var errSubjectUnreadable = errors.New("the projected token could not be read")

// subjectFile is the projected token as the read token trades it, its read errors marked
// errSubjectUnreadable so Token can tell them from a trade's.
type subjectFile struct{ *brokerclient.ProjectedToken }

func (s subjectFile) Token(ctx context.Context) (string, string, error) {
	token, kind, err := s.ProjectedToken.Token(ctx)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errSubjectUnreadable, err)
	}
	return token, kind, nil
}
