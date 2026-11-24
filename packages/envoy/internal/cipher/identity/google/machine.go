package google

import (
	"fmt"
	"path"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// machine answers the claims for a machine's Google identity token, from the claims Google puts in
// it: `email`, `email_verified` and, for a token minted on a Compute Engine instance with
// format=full, the `google` → `compute_engine` members.
//
// The identity is the account the token was minted for, which an entry lists by its email
// address. Google's signature, the issuer, the audience and the expiry are checked before this is
// reached (the exchange); what is left to decide is that Google holds the address verified, that
// the machine is one the entry allows, and what its token may write.
//
// Where a machine runs is the entry's to pin. An entry that pins the Compute Engine project and
// an instance-name pattern holds the token to an instance of that project whose name the pattern
// matches, and refuses a token carrying no Compute Engine claims, which proves nothing about
// where it was minted. An entry that pins neither takes such a token: that is what a token minted
// through a workload identity pool carries, and what a token from the metadata server of a pod on
// Google Kubernetes Engine carries, neither of which runs on an instance of its own.
//
// The principal is the listed account, not the machine: every machine of one account is one
// holder, so a machine rebuilt under another instance id keeps writing under the name the machine
// before it held, and a machine of any other account is refused that name. Nothing separates two
// machines of one account the way a registration separates two pods, so each names a session of
// its own and the claim on the name keeps them apart.
//
// An entry that names a person and a project mints a token that acts for that person and names
// that project, as a pod's entry does. An entry that names neither mints a token that names its
// service and no person at all: it names no project either, and says so with the person scope,
// the wire's word for a token no entry maps to a project. Nothing it can write follows from that
// alone — a resource server holds a token naming no person to what a service may do — which is
// why the service is a claim of its own rather than an absence to be read, and why a request of
// such a machine naming a project is refused rather than read.
//
// An entry that names a job list mints a token that names the account's address as its service
// and no person, with the entry's scope and whole list whatever the request names
// (exchange.Claims.Grant). Fleet is still the entry's service, so the launcher credential its
// exchange mints records the service it would for any machine of the account.
func (p *Provider) machine(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	identity := ReadIdentity(token)
	if identity.Account == "" {
		return exchange.Claims{}, fmt.Errorf("%w: the subject token carries no email claim, so it names no account (ask for it with format=full)", exchange.ErrClaimMissing)
	}
	entry, listed := p.machines[identity.Account]
	if !listed {
		return exchange.Claims{}, fmt.Errorf("%w: no entry lists the Google account %s", exchange.ErrNoEntry, identity.Account)
	}
	if !identity.EmailVerified {
		return exchange.Claims{}, fmt.Errorf("%w: %s presented a token whose email_verified is not true", exchange.ErrEmailNotVerified, identity.Account)
	}
	if err := allowedMachine(entry, identity); err != nil {
		return exchange.Claims{}, err
	}
	name, err := exchange.ResolveSession(entry.Session, session, identity.Account)
	if err != nil {
		return exchange.Claims{}, err
	}
	claims := exchange.Claims{Session: name, Principal: Principal(identity.Account), Fleet: entry.Service}
	if !entry.Job.Listed() && entry.Person == "" {
		if len(projects) > 0 {
			return exchange.Claims{}, fmt.Errorf("%w: %s is the service %s and names no person, and the request named %s",
				exchange.ErrProjectsNoPerson, identity.Account, entry.Service, oauthwire.JoinProjects(projects))
		}
		claims.Service, claims.Scope = entry.Service, exchange.ProjectScopePerson
		return claims, nil
	}
	if err := claims.Grant(identity.Account, identity.Account, entry.Person, entry.Project, entry.Job, projects); err != nil {
		return exchange.Claims{}, err
	}
	return claims, nil
}

// allowedMachine holds a token to the Compute Engine instance its entry pins, and takes every
// machine of an entry that pins nothing. The two refusals are told apart because an operator acts
// on each differently: a token with no Compute Engine claims was minted somewhere the pins cannot
// describe, and a token with them from another project or another instance name is a machine the
// entry does not list.
func allowedMachine(entry GoogleEntry, identity Identity) error {
	if entry.CloudProject == "" && entry.Instance == "" {
		return nil
	}
	if !identity.Compute.Whole() {
		return fmt.Errorf("%w: %s presented a token carrying no Compute Engine identity, and its entry takes machines of the project %s named %s",
			exchange.ErrNoComputeEngine, identity.Account, entry.CloudProject, entry.Instance)
	}
	if identity.Compute.Project != entry.CloudProject {
		return fmt.Errorf("%w: %s presented a token minted in the project %s, and its entry takes machines of %s",
			exchange.ErrMachineNotAllowed, identity.Account, identity.Compute.Project, entry.CloudProject)
	}
	// The pattern was checked at New, so a match error here is impossible; a pattern that did not
	// match is the refusal either way.
	if matched, _ := path.Match(entry.Instance, identity.Compute.Instance); !matched {
		return fmt.Errorf("%w: %s presented a token minted on the instance %s, and its entry takes the names %s",
			exchange.ErrMachineNotAllowed, identity.Account, identity.Compute.Instance, entry.Instance)
	}
	return nil
}
