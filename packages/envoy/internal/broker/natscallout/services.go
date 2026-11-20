package natscallout

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/natsgrant"
)

// ServicesVariable is the setting ParseServices reads.
const ServicesVariable = "BROKER_NATS_SERVICES"

// Kind is which of the grants a service connection is answered with.
type Kind string

// The kinds of service the auth service admits, each answered with its grant of
// natsgrant: Dispatch's, a listener's, a Legion daemon's and the measurement job's.
const (
	KindDispatch Kind = "dispatch"
	KindListener Kind = "listener"
	KindDaemon   Kind = "daemon"
	KindMeasure  Kind = "measure"
)

// ServiceEntry is one service the auth service admits by its platform identity.
type ServiceEntry struct {
	Kind Kind
	// MachineID is the machine id the grant is built for, which Dispatch's and a listener's
	// name their durable and queue group by; required for those two kinds and refused for the
	// others.
	MachineID string
	// Account is the platform account whose token the service presents: a ServiceAccount
	// subject, or a Google account address.
	Account string
	// Google is whether Account is a Google service account.
	Google bool
	// AllProjects is the grant whole; otherwise the projects come from the broker's exchange
	// list for Account.
	AllProjects bool
}

// name is the name the user the auth service answers this entry's connections with carries,
// <kind>/<machine id or account>, which a server's connection listing (/connz) reports.
func (e ServiceEntry) name() string {
	if e.MachineID != "" {
		return string(e.Kind) + "/" + e.MachineID
	}
	return string(e.Kind) + "/" + e.Account
}

// noMachineID is how an entry writes a kind that takes no machine id, and allProjectsScope and
// exchangeScope the two scopes an entry takes.
const (
	noMachineID      = "-"
	allProjectsScope = "*"
	exchangeScope    = "-"
)

// ParseServices reads BROKER_NATS_SERVICES: whitespace-separated <kind>/<machine id or ->/<* or
// ->=<account> entries, in order, where * is the grant whole and - means "the projects this
// account's exchange entry lists", and the account is written as BROKER_API_ACCOUNTS writes one
// (config.ParseAccount). It refuses, naming the entry: an unknown kind; a dispatch or listener
// entry with no machine id, and a daemon or measure entry with one; a machine id that is not one
// subject token; a measure entry with -, since its grant holds a KV read no project scope can
// hold and it reads no project's messages; an account config.ParseAccount refuses (a namespace's
// default account, a Google default or service-agent account); and an account given twice, since
// one account connects as one service.
func ParseServices(raw string) ([]ServiceEntry, error) {
	var services []ServiceEntry
	seen := map[string]string{}
	for _, text := range strings.Fields(raw) {
		entry, err := parseService(text)
		if err != nil {
			return nil, err
		}
		if other, twice := seen[entry.Account]; twice {
			return nil, fmt.Errorf("%s names the account %s in both %q and %q; one account connects as one service, so name it once", ServicesVariable, entry.Account, other, text)
		}
		seen[entry.Account] = text
		services = append(services, entry)
	}
	return services, nil
}

func parseService(text string) (ServiceEntry, error) {
	form := func() error {
		return fmt.Errorf("%s must be <kind>/<machine id or ->/<* or ->=<account> entries, the kind one of dispatch, listener, daemon or measure and the account system:serviceaccount:<namespace>:<name> or google:<service account email>, got %q", ServicesVariable, text)
	}
	head, account, ok := config.CutNATSServiceEntry(text)
	if !ok {
		return ServiceEntry{}, form()
	}
	parts := strings.Split(head, "/")
	if len(parts) != 3 {
		return ServiceEntry{}, form()
	}
	entry := ServiceEntry{Kind: Kind(parts[0])}
	machineID, scope := parts[1], parts[2]
	switch entry.Kind {
	case KindDispatch, KindListener:
		if machineID == noMachineID {
			return ServiceEntry{}, fmt.Errorf("%s entry %q names no machine id: a %s's grant names its durable and its queue group by one", ServicesVariable, text, entry.Kind)
		}
		if !natsgrant.SessionToken(machineID) {
			return ServiceEntry{}, fmt.Errorf("%s entry %q names a machine id that is not one subject token", ServicesVariable, text)
		}
		entry.MachineID = machineID
	case KindDaemon, KindMeasure:
		if machineID != noMachineID {
			return ServiceEntry{}, fmt.Errorf("%s entry %q names a machine id, which a %s's grant does not take; write -", ServicesVariable, text, entry.Kind)
		}
	default:
		return ServiceEntry{}, form()
	}
	switch scope {
	case allProjectsScope:
		entry.AllProjects = true
	case exchangeScope:
		if entry.Kind == KindMeasure {
			return ServiceEntry{}, fmt.Errorf("%s entry %q holds the measurement job to its exchange projects, which its grant cannot be held to: it reads a KV bucket and no project's messages, so write *", ServicesVariable, text)
		}
	default:
		return ServiceEntry{}, form()
	}
	parsed, err := config.ParseAccount(ServicesVariable, text, account)
	if errors.Is(err, config.ErrAccountForm) {
		return ServiceEntry{}, form()
	}
	if err != nil {
		return ServiceEntry{}, err
	}
	entry.Account, entry.Google = parsed.Address, parsed.Google
	return entry, nil
}
