package natscallout

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/natsgrant"
)

// ServicesVariable is the setting ParseServices reads.
const ServicesVariable = "BROKER_NATS_SERVICES"

// Kind is which of the grants a service connection is answered with.
type Kind string

// The kinds of service the auth service admits, each answered with its grant of
// natsgrant: Dispatch's, a listener's, a Legion daemon's and the measurement job's. The privacy
// measurement (plan C's measurement of a listener's GET /v1/project-privacy) is a kind of entry
// too, so the broker answers that read for its account; it has no grant, so the auth service
// answers it no NATS user (serviceGrant).
const (
	KindDispatch       Kind = "dispatch"
	KindListener       Kind = "listener"
	KindDaemon         Kind = "daemon"
	KindMeasure        Kind = "measure"
	KindPrivacyMeasure Kind = "privacy-measure"
)

// ServiceEntry is one service the auth service admits by its platform identity.
type ServiceEntry struct {
	Kind Kind
	// MachineID is the entry's machine id: the one Dispatch's and a listener's grant name their
	// durable and queue group by, and the one a privacy measurement's reads are named by in the
	// broker's record; required for those three kinds and refused for the others.
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

// name is the entry's name, <kind>/<machine id or account>: the name of the user the auth service
// answers its connections with, which a server's connection listing (/connz) reports, and for a
// privacy measurement, which takes no NATS user, the name the broker's record gives its reads.
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
// (config.ParseAccount). It refuses, naming the entry: an unknown kind; a dispatch, listener or
// privacy-measure entry with no machine id, and a daemon or measure entry with one; a machine id
// that is not one subject token; a measure entry with -, since its grant holds a KV read no
// project scope can hold and it reads no project's messages, and a privacy-measure entry with -,
// since the read it makes answers every private project; an account config.ParseAccount refuses
// (a namespace's default account, a Google default or service-agent account); and an account given
// twice, since one account connects as one service.
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

// ReadsProjectPrivacy reports whether a service of this kind reads, at the broker's GET
// /v1/project-privacy, every private project and the Slack channel ids each one's owner named. It
// is the one statement of which kinds do: the route answers what PrivacyReaders keeps, and nothing
// else. A listener does, for its privacy decisions, and so does the privacy measurement, which
// times that read on the listener's own path; Dispatch's own listener's kind joins them when the
// auth service gains one. Every other kind does not, until someone decides it should here: a kind
// is not a reader for being named like one, and an outside-facing or project-limited listener
// reads no other project's privacy.
func (k Kind) ReadsProjectPrivacy() bool {
	switch k {
	case KindListener, KindPrivacyMeasure:
		return true
	}
	return false
}

// PrivacyReaders is services as GET /v1/project-privacy reads them (api.PrivacyReaders): each
// entry whose kind reads project privacy and whose grant is whole (*), by its account. Each is
// named by its entry, <kind>/<machine id>, which for a listener is also the name of the NATS user
// the auth service gives it (the privacy measurement takes none), and the privacy measurement's
// reads are told apart from a listener's (api.PrivacyReader.Measurement). An entry held to its
// exchange projects (-) is none, since the route answers every private project and such an
// entry's grant reaches only its own.
func PrivacyReaders(services []ServiceEntry) api.PrivacyReaders {
	readers := api.PrivacyReaders{K8s: map[string]api.PrivacyReader{}, Google: map[string]api.PrivacyReader{}}
	for _, entry := range services {
		if !entry.AllProjects || !entry.Kind.ReadsProjectPrivacy() {
			continue
		}
		reader := api.PrivacyReader{Name: entry.name(), Measurement: entry.Kind == KindPrivacyMeasure}
		if entry.Google {
			readers.Google[entry.Account] = reader
		} else {
			readers.K8s[entry.Account] = reader
		}
	}
	return readers
}

func parseService(text string) (ServiceEntry, error) {
	form := func() error {
		return fmt.Errorf("%s must be <kind>/<machine id or ->/<* or ->=<account> entries, the kind one of dispatch, listener, daemon, measure or privacy-measure and the account system:serviceaccount:<namespace>:<name> or google:<service account email>, got %q", ServicesVariable, text)
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
	case KindDispatch, KindListener, KindPrivacyMeasure:
		if machineID == noMachineID {
			if entry.Kind == KindPrivacyMeasure {
				return ServiceEntry{}, fmt.Errorf("%s entry %q names no machine id: the broker's record names a privacy measurement's reads by one", ServicesVariable, text)
			}
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
		switch entry.Kind {
		case KindMeasure:
			return ServiceEntry{}, fmt.Errorf("%s entry %q holds the measurement job to its exchange projects, which its grant cannot be held to: it reads a KV bucket and no project's messages, so write *", ServicesVariable, text)
		case KindPrivacyMeasure:
			return ServiceEntry{}, fmt.Errorf("%s entry %q holds the privacy measurement to its exchange projects, which its read cannot be held to: GET /v1/project-privacy answers every private project, so write *", ServicesVariable, text)
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
