package google

import (
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// MachineSubject names one machine as the broker keys the launcher credential its exchange mints:
// the row's `subject`, under the live-unique index that makes each machine's credential its own
// and lets its next exchange move that one credential rather than add another.
//
// It is <machine>#<thumbprint>: the machine, and the RFC 7638 thumbprint of the key the request's
// DPoP proof presented. The key is part of the name because the credential is bound to that key
// and the subject token is a bearer: a copy of a machine's Google identity token presented with a
// key of the copier's own would otherwise move the machine's one credential onto that key, and
// whoever holds a machine's credential ends that machine's sessions (enroll.Service.Revoke).
// Keyed by the key, such a copy mints a credential of its own and moves nothing of the machine's.
//
// The machine is <account>@<project>/<zone>/<instance id> for a token minted on a Compute Engine
// instance, and the account alone for a token carrying no Compute Engine claim at all — one minted
// through a workload identity pool, and the one a pod's metadata server on Google Kubernetes
// Engine answers. An instance id is unique only within a project and a zone, so neither the id
// nor the account alone names one machine; the whole of it does. Naming the instance still earns
// its place now that the key tells two machines apart: two instances of one account presenting
// one key — a key baked into an image — ask for two subjects with one key, which
// launcher_credentials_live_key refuses by name, where under one subject they would quietly share
// a credential and each could end the other's sessions.
//
// What the key in the name costs is that a machine making a new key leaves its old credential
// behind until that credential's own expiry, which is the subject token's, and the sessions
// enrolled under it stop renewing when it lapses (enroll.Service.Renew). So a machine keeps its
// key rather than making one per process: `agent-secrets-helper` reads it back from
// ~/.config/agent-secrets/identity-key.pem, mode 0600, and makes one only at the machine's first
// trade (helper.LoadOrMakeIdentityKey). Postgres holds this spelling in
// launcher_credentials.subject, so it never changes.
func MachineSubject(identity Identity, thumbprint string) string {
	name := identity.Account
	if identity.Compute.Whole() {
		name += "@" + identity.Compute.Project + "/" + identity.Compute.Zone + "/" + identity.Compute.InstanceID
	}
	return name + "#" + thumbprint
}

// signInSubjectPrefix opens the subject of a person's sign-in on a machine, so that no subject of
// any other kind can spell one: a Google account never begins with it, and a Kubernetes
// ServiceAccount's subject begins with "system:serviceaccount:".
const signInSubjectPrefix = "signin:"

// PersonSubject names one person's sign-in on one box as the broker keys the launcher credential
// the exchange mints for it: the row's `subject`, under the live-unique index that makes each
// sign-in's credential its own and lets the sign-in's next trade move that one credential rather
// than add another.
//
// It is signin:<person>|<machine account>#<box key thumbprint>, and all three parts are needed.
// The person is there because two people signed in on one box each reach their own secrets and
// must therefore hold a credential each, which one subject could not. The box key's thumbprint is
// there for the reason a machine's is, and because one person signed in on two boxes holds a
// sign-in on each, told apart by the key `agent-secrets login` made there and by nothing else —
// the machine account is a fleet's and is the same on every box of it. The machine account is
// there so that the credential can still be held to a listed fleet on every call
// (enroll.Service.AuthenticateLauncher): a person signs in from a box the deployment lists, and an
// account dropped from BROKER_VM_SERVICES ends its boxes' sign-in credentials as it ends their
// machines'.
func PersonSubject(person, account, thumbprint string) string {
	return signInSubjectPrefix + person + "|" + account + "#" + thumbprint
}

// ReadSubject reads a launcher credential's subject back into its parts: the one reading of the
// grammar MachineSubject and PersonSubject write, so no caller spells that grammar a second time.
// A subject it does not recognise, a malformed one included, is not claimed, and the registry
// answers it SubjectOther with nothing filled in.
func (p *Provider) ReadSubject(subject string) (identity.Subject, bool) {
	if rest, signIn := strings.CutPrefix(subject, signInSubjectPrefix); signIn {
		person, box, split := strings.Cut(rest, "|")
		if !split {
			return identity.Subject{}, false
		}
		account, _, keyed := strings.Cut(box, "#")
		if !keyed || person == "" || account == "" {
			return identity.Subject{}, false
		}
		return subjectOf(identity.SubjectSignIn, account), true
	}
	machine, _, keyed := strings.Cut(subject, "#")
	if !keyed {
		return identity.Subject{}, false
	}
	if at := strings.LastIndex(machine, "@"); at > 0 && strings.Contains(machine[at+1:], "/") {
		return subjectOf(identity.SubjectMachine, machine[:at]), true
	}
	if machine == "" {
		return identity.Subject{}, false
	}
	return subjectOf(identity.SubjectMachine, machine), true
}

// subjectOf is a subject of kind carrying account, whose principal is the account's: a machine's
// own and a sign-in's box are both held to the fleet that account is listed for.
func subjectOf(kind identity.SubjectKind, account string) identity.Subject {
	return identity.Subject{Kind: kind, Account: identity.Account{Kind: Kind, Address: account}, Principal: Principal(account)}
}
