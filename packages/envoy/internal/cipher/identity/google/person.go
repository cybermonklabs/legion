package google

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/oidc"
)

// person answers the claims for a person's own Google identity token, presented with the
// machine's own as the actor token (RFC 8693 Section 2.1): the sign-in `agent-secrets login` made
// on that machine, where the person is the subject and the machine is only acting for them.
//
// The two tokens are read by the two rules that belong to them. The person's is held to the
// Workspace rule the broker's own approval sign-in is held to (oidc.WorkspaceLogin): a subject, an
// email of an address's shape that Google has verified, and the `hd` claim equal to the
// configured domain, which an empty configured domain can never match. The domain comes from `hd`
// alone, never from the address's suffix, because a personal Google account can hold an address
// under a Workspace's domain. Google's signature, the issuer, the expiry and the audience of the
// device-flow client are checked before this is reached. The machine's is held to the entry
// rules a machine trading for itself is held to: its account is one BROKER_VM_SERVICES lists,
// Google holds that address verified, and the machine runs where the entry pins it, so a person
// cannot sign in from a box of an account the deployment does not list.
//
// The principal is the person, as it is for a dev box session's machine credential: every machine
// and every session of one person is one holder, so a person keeps their session names from box
// to box, and no other identity may write under one of them. The token acts as that person, with
// the person scope, and carries the projects the trade asked for: a person's sign-in is their own
// identity on their own box, which reaches whichever projects that person reaches, and the list
// says which of them this session works on. That the person may see each one is the exchange's
// next question, which it asks Dispatch; this provider reads no Dispatch.
//
// What it does not decide is whether that person's sign-in on that box key still stands: the box
// key's thumbprint is the DPoP proof's, which a decision never sees, and the sign-in is a row the
// exchange reads in its own transaction.
func (p *Provider) person(token, actor map[string]any, session string, projects []string) (exchange.Claims, error) {
	if actor == nil {
		return exchange.Claims{}, fmt.Errorf("%w: a person signs in on a machine, so the machine's own Google identity token is presented as the actor token (%s)", exchange.ErrNoActorToken, ActorTokenTypeMachine)
	}
	machine := ReadIdentity(actor)
	if machine.Account == "" {
		return exchange.Claims{}, fmt.Errorf("%w: the actor token carries no email claim, so it names no machine (ask for it with format=full)", exchange.ErrClaimMissing)
	}
	entry, listed := p.machines[machine.Account]
	if !listed {
		return exchange.Claims{}, fmt.Errorf("%w: no entry lists the Google account %s, which the actor token names as the machine", exchange.ErrNoEntry, machine.Account)
	}
	if !machine.EmailVerified {
		return exchange.Claims{}, fmt.Errorf("%w: the machine %s presented an actor token whose email_verified is not true", exchange.ErrEmailNotVerified, machine.Account)
	}
	if err := allowedMachine(entry, machine); err != nil {
		return exchange.Claims{}, err
	}
	login, err := oidc.WorkspaceLogin(personClaims(token), p.config.SignInDomain)
	switch {
	case errors.Is(err, oidc.ErrNoSubject):
		return exchange.Claims{}, fmt.Errorf("%w: the subject token carries no sub claim, so it names no person", exchange.ErrClaimMissing)
	case errors.Is(err, oidc.ErrNoEmail):
		return exchange.Claims{}, fmt.Errorf("%w: the subject token carries no email claim of an address's shape, so it names no person", exchange.ErrClaimMissing)
	case errors.Is(err, oidc.ErrNotInDomain):
		return exchange.Claims{}, fmt.Errorf("%w: %s signed in, and this broker takes the Google Workspace domain BROKER_SIGNIN_DOMAIN names; sign in again with `agent-secrets login` as an account of that domain", exchange.ErrNotInDomain, login)
	case errors.Is(err, oidc.ErrEmailNotVerified):
		return exchange.Claims{}, fmt.Errorf("%w: %s presented a token whose email_verified is not true", exchange.ErrEmailNotVerified, login)
	case err != nil:
		return exchange.Claims{}, fmt.Errorf("%w: the subject token is not a person's Google identity token (%v)", exchange.ErrClaimMissing, err)
	}
	name, err := exchange.ResolveSession("", session, login)
	if err != nil {
		return exchange.Claims{}, err
	}
	return exchange.Claims{
		Session:   name,
		Person:    login,
		Scope:     exchange.ProjectScopePerson,
		Projects:  slices.Clone(projects),
		Principal: exchange.PersonPrincipal(login),
		Actor:     machine.Account,
		// The fleet the box belongs to, which the launcher credential the exchange mints for this
		// sign-in records as its service: a credential keyed by a subject carries one. Nothing of
		// the fleet reaches the token's claims — the token is the person's.
		Fleet: entry.Service,
	}, nil
}

// personClaims is the reading of a person's Google identity token that oidc.WorkspaceLogin
// judges: the four claims its rule names, out of the claim map of a token the exchange's own
// verifier has already checked. It is the one place those claim names are spelled for a person,
// as ReadIdentity is for a machine; `email_verified` is read through the same isVerified, since
// Google writes it as a boolean on some of its tokens and as the string "true" on others.
func personClaims(token map[string]any) oidc.Claims {
	subject, _ := token["sub"].(string)
	email, _ := token["email"].(string)
	domain, _ := token["hd"].(string)
	return oidc.Claims{
		Subject:       subject,
		Email:         email,
		EmailVerified: isVerified(token["email_verified"]),
		HostedDomain:  domain,
	}
}
