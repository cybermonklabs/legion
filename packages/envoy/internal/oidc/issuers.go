package oidc

import (
	"context"
	"crypto"
	"fmt"
	"sync"
	"time"
)

// NamedIssuer is one issuer of an IssuerSet: the name a deployment's configuration gives it, empty
// for the one issuer of a set written as a bare URL, and the URL a token of it names as its iss.
type NamedIssuer struct {
	Name, URL string
}

// IssuerName is the name of the issuer of issuers, a setting's list in its order, whose URL is
// url: the URL a row records as the issuer whose token made it, such as a token login's launcher
// credential or a pod's enrollment. A row that records none (nil) was written before rows recorded
// one, when the setting named one issuer, which stays its first. A URL the list no longer holds is
// no issuer's, and so is every row when the list is empty: ok is false, and the empty name an
// issuer of a bare-URL setting has is never answered for it.
func IssuerName(issuers []NamedIssuer, url *string) (name string, ok bool) {
	if len(issuers) == 0 {
		return "", false
	}
	if url == nil {
		return issuers[0].Name, true
	}
	for _, issuer := range issuers {
		if issuer.URL == *url {
			return issuer.Name, true
		}
	}
	return "", false
}

// IssuerSet verifies tokens of several issuers for one audience, such as the projected
// ServiceAccount tokens of several Kubernetes clusters, each its own issuer. It reads a token's iss
// without trusting it and gives the token to that issuer's verifier alone, so a token of one
// issuer never makes another's verifier read its keys. Each issuer is a LazyVerifier, read when
// first needed or in the background, so an issuer that cannot be read stops no other.
type IssuerSet struct {
	entries []issuerEntry
}

// issuerEntry is one issuer of a set and its verifier.
type issuerEntry struct {
	issuer   NamedIssuer
	verifier *LazyVerifier
}

// NewIssuerSet returns a set verifying tokens each of issuers issues to audience, and reads
// nothing (NewLazyVerifier). keySpacing is the FailureSpacing of each issuer's key set
// (WithKeyFailureSpacing); zero spaces nothing. The issuers' URLs are distinct, as the
// configuration that names them refuses a repeated one; a token naming a URL twice listed goes to
// the first.
func NewIssuerSet(issuers []NamedIssuer, audience string, keySpacing time.Duration) *IssuerSet {
	set := &IssuerSet{entries: make([]issuerEntry, 0, len(issuers))}
	for _, issuer := range issuers {
		set.entries = append(set.entries, issuerEntry{
			issuer:   issuer,
			verifier: NewLazyVerifier(issuer.URL, audience, WithKeyFailureSpacing(keySpacing)),
		})
	}
	return set
}

// VerifyClaims checks raw against the issuer its iss names, and answers the claims and that
// issuer, unmarshalling the verified payload into each of into through that issuer's
// LazyVerifier (Verifier.VerifyClaims): the one way a caller of the set reads a claim Claims does
// not carry, such as a pod's private claims. The iss is read from raw with every space removed
// (stripSpaces), as the verifier reads the token, so a token with a trailing line feed or a space
// inside a segment goes to its issuer and verifies as it does through that issuer's Verifier. A
// token that cannot be decoded is ErrMalformed, and one whose iss no issuer of the set names is
// ErrIssuer, as Verifier.Verify classifies each; neither reads anything. Every other answer is
// that issuer's LazyVerifier's, ErrIssuerUnavailable while its discovery document has not been
// read or its key set cannot be, and it carries the issuer even when it is an error, so a caller
// can say which issuer could not be read. The unverified iss only chooses the verifier: every
// claim the answer carries is read from the payload that issuer's signature covers.
func (s *IssuerSet) VerifyClaims(ctx context.Context, raw string, into ...any) (Claims, NamedIssuer, error) {
	iss, err := UnverifiedIssuer(raw)
	if err != nil {
		return Claims{}, NamedIssuer{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	entry, err := s.entry(iss)
	if err != nil {
		return Claims{}, NamedIssuer{}, err
	}
	verified, err := entry.verifier.VerifyClaims(ctx, raw, into...)
	return verified, entry.issuer, err
}

// Key answers the key the issuer of the set whose URL is issuer publishes under kid for alg,
// through that issuer's LazyVerifier (LazyVerifier.Key), so a caller that needs the key itself,
// as the RFC 8693 token exchange does, or a Verifier of that issuer for another audience
// (NewWithKeys), reads the discovery document and the key set VerifyClaims reads, with their
// background read, their spacing and their errors, ErrIssuerUnavailable among them. An issuer the
// set does not name is ErrIssuer, and reads nothing.
func (s *IssuerSet) Key(ctx context.Context, issuer, kid, alg string) (crypto.PublicKey, error) {
	entry, err := s.entry(issuer)
	if err != nil {
		return nil, err
	}
	return entry.verifier.Key(ctx, kid, alg)
}

// entry is the entry of the set whose issuer's URL is url, and ErrIssuer for a URL no issuer of
// the set names.
func (s *IssuerSet) entry(url string) (issuerEntry, error) {
	for _, entry := range s.entries {
		if entry.issuer.URL == url {
			return entry, nil
		}
	}
	return issuerEntry{}, fmt.Errorf("%w: %q, which no issuer of the set names", ErrIssuer, url)
}

// IssuerRead is one issuer of a set and whether its discovery document has been read.
type IssuerRead struct {
	Issuer NamedIssuer
	Read   bool
}

// Boot tries every issuer's discovery at once, each try bounded by discoveryAttemptTimeout, and
// answers, in the set's order, each issuer and whether it was read. A failed try is no error of
// Boot's: Run reads that issuer in the background, and until then its tokens are
// ErrIssuerUnavailable.
func (s *IssuerSet) Boot(ctx context.Context) []IssuerRead {
	reads := make([]IssuerRead, len(s.entries))
	var wg sync.WaitGroup
	for i, entry := range s.entries {
		reads[i].Issuer = entry.issuer
		wg.Go(func() {
			attempt, cancel := context.WithTimeout(ctx, discoveryAttemptTimeout)
			defer cancel()
			reads[i].Read = entry.verifier.Discover(attempt) == nil
		})
	}
	wg.Wait()
	return reads
}

// Run reads in the background each issuer not read when it is called (LazyVerifier.Run), and
// calls read once for each of them when its read succeeds, from that issuer's own goroutine. It
// returns once every one of them is read or ctx ends.
func (s *IssuerSet) Run(ctx context.Context, read func(NamedIssuer)) {
	var wg sync.WaitGroup
	for _, entry := range s.entries {
		if entry.verifier.Discovered() {
			continue
		}
		wg.Go(func() {
			entry.verifier.Run(ctx)
			if entry.verifier.Discovered() {
				read(entry.issuer)
			}
		})
	}
	wg.Wait()
}
