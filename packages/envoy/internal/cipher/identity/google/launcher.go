package google

import (
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// LauncherSubject is the subject and host of the launcher credential a trade mints: a machine's
// own credential keyed by the machine (MachineSubject), and a person's sign-in keyed by the
// person, the machine acting for them and the box key (PersonSubject), each with the machine's
// host. thumbprint is the key the trade's DPoP proof presented.
func (p *Provider) LauncherSubject(trade identity.Trade, claims exchange.Claims, thumbprint string) (subject, host string) {
	switch trade.TokenType {
	case SubjectTokenType:
		machine := ReadIdentity(trade.Subject)
		return MachineSubject(machine, thumbprint), machineHost(machine)
	case PersonTokenType:
		return PersonSubject(claims.Person, claims.Actor, thumbprint), machineHost(ReadIdentity(trade.Actor))
	}
	return "", ""
}

// machineHost is the machine as its credential's row records it, which is what an operator reads
// on their machine-login page: the instance a Compute Engine token was minted on, and the account
// alone for a token minted anywhere else, which names no instance to show.
func machineHost(identity Identity) string {
	if identity.Compute.Whole() {
		return identity.Compute.Project + "/" + identity.Compute.Zone + "/" + identity.Compute.Instance
	}
	return identity.Account
}
