package enroll

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// MachineCredentials is oauth.ExchangeConfig.MachineCredentials over the enrollment service: the
// launcher credential a trade mints or moves, written in the exchange's own transaction
// (IssueTokenLoginTx), so the credential and the access token that names it land together. Both
// trades that mint one come through here — a machine's own, which carries its fleet's service and
// no person, and a person's sign-in on such a machine, which carries that person as its operator —
// so there is one way a credential is minted and not two. A key another live credential holds is
// oauth.ErrMachineKeyTaken, and a key a live read token is bound to (ErrKeyBindsReadToken) is
// oauth.ErrMachineKeyBindsReadToken; the exchange answers each invalid_grant with a hint of its
// own. The broker command and the test rig (ciphertest) both install it, so the rig's trades write
// what the broker's write.
func (s *Service) MachineCredentials() oauth.MachineCredentials {
	return func(ctx context.Context, cred oauth.MachineCredential) (string, error) {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return "", errors.New("broker: a machine's launcher credential is minted in the exchange's transaction")
		}
		id, err := s.IssueTokenLoginTx(ctx, tx, TokenLogin{
			Subject:      cred.Subject,
			Service:      cred.Service,
			Host:         cred.Host,
			Operator:     cred.Operator,
			TokenExpires: cred.Expires,
			Thumbprint:   cred.Thumbprint,
			JWK:          cred.JWK,
			Expires:      cred.Expires,
		})
		if KeyHeld(err) {
			return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyTaken, err)
		}
		if errors.Is(err, ErrKeyBindsReadToken) {
			return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyBindsReadToken, err)
		}
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}
}

// PersonSignIns is oauth.ExchangeConfig.PersonSignIns over the broker's store: the sign-in a
// person's trade belongs to, written in the exchange's own transaction (RecordPersonSignIn), so
// the sign-in and the access token minted under it land together. endUnrecorded is
// store.SignInTrade.EndUnrecorded: whether a trade ends a sign-in recording no account and
// everything it held before it writes the trade's own, which the broker command sets where
// BROKER_PROJECT_ACCESS_URL is set. An account that no longer holds the person's address is
// store.ErrSubjectReplaced, a box key that person has revoked store.ErrSignInRevoked, and
// one whose sign-in was made on another machine or by another account
// store.ErrSignInMachineChanged or store.ErrSignInSubjectChanged; the exchange answers each
// invalid_grant saying which.
func PersonSignIns(endUnrecorded bool) oauth.PersonSignIns {
	return func(ctx context.Context, in oauth.PersonSignIn) error {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return errors.New("broker: a person's sign-in is recorded in the exchange's transaction")
		}
		err := RecordPersonSignIn(ctx, tx, store.SignInTrade{
			Person: in.Person, Thumbprint: in.Thumbprint, MachineAccount: in.Machine,
			Account:       store.IssuerSubject{Issuer: in.Issuer, Subject: in.Subject},
			EndUnrecorded: endUnrecorded,
		}, in.CredentialSubject)
		switch {
		case errors.Is(err, store.ErrSubjectReplaced):
			return fmt.Errorf("%w: %v", oauth.ErrSignInSubjectReplaced, err)
		case errors.Is(err, store.ErrSignInRevoked):
			return fmt.Errorf("%w: %v", oauth.ErrSignInRevoked, err)
		case errors.Is(err, store.ErrSignInMachineChanged):
			return fmt.Errorf("%w: %v", oauth.ErrSignInMachineChanged, err)
		case errors.Is(err, store.ErrSignInSubjectChanged):
			return fmt.Errorf("%w: %v", oauth.ErrSignInSubjectChanged, err)
		}
		return err
	}
}
