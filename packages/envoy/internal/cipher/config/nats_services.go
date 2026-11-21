package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/natsgrant"
)

// NATSServicesVariable is the setting ParseNATSServices reads.
const NATSServicesVariable = "BROKER_NATS_SERVICES"

// NATSServiceKind is which kind of service a BROKER_NATS_SERVICES entry names.
type NATSServiceKind string

// The kinds of entry. Dispatch, a listener, a Legion daemon, the measurement job and an edge
// listener are services the NATS auth service admits by their platform identity, each answered
// with its kind's grant of natsgrant, an edge listener's built for the projects its entry lists
// (natsgrant.Edge). An edge listener's entry is also read for those projects
// (NATSCallout.EdgeListeners), whether or not the broker runs the auth service.
const (
	NATSDispatch     NATSServiceKind = "dispatch"
	NATSListener     NATSServiceKind = "listener"
	NATSDaemon       NATSServiceKind = "daemon"
	NATSMeasure      NATSServiceKind = "measure"
	NATSEdgeListener NATSServiceKind = "edge-listener"
)

// NATSService is one BROKER_NATS_SERVICES entry, as ParseNATSServices reads it at load. It is the
// one reading of an entry: the NATS auth service admits services by it, the broker's API reads the
// edge listeners from it, and every check of Load that asks an entry's kind or account asks it.
type NATSService struct {
	Kind NATSServiceKind
	// MachineID is the machine id the grant is built for, which Dispatch's, a listener's and an
	// edge listener's name their durable and queue group, or the edge's inbox prefix, by; required
	// for those three kinds and refused for the others.
	MachineID string
	// Account is the platform account whose token the service presents: a ServiceAccount
	// subject, or a Google account address.
	Account string
	// Google is whether Account is a Google service account.
	Google bool
	// AllProjects is the grant whole; otherwise the projects come from the broker's exchange
	// list for Account, but for an edge listener, whose projects are Projects.
	AllProjects bool
	// Projects is the projects an edge listener's entry lists, sorted, each once; nil for every
	// other kind.
	Projects []string
	// Text is the entry as the setting gives it, which every refusal naming the entry quotes.
	Text string
}

// Name is the name the user the auth service answers this entry's connections with carries,
// <kind>/<machine id or account>, which a server's connection listing (/connz) reports.
func (s NATSService) Name() string {
	if s.MachineID != "" {
		return string(s.Kind) + "/" + s.MachineID
	}
	return string(s.Kind) + "/" + s.Account
}

// noMachineID is how an entry writes a kind that takes no machine id, and allProjectsScope and
// exchangeScope the two scopes an entry of a service takes.
const (
	noMachineID      = "-"
	allProjectsScope = "*"
	exchangeScope    = "-"
)

// ParseNATSServices reads BROKER_NATS_SERVICES: whitespace-separated <kind>/<machine id or ->/<* or
// ->=<account> entries, in order, where * is the grant whole and - means "the projects this
// account's exchange entry lists", and the account is written as BROKER_API_ACCOUNTS writes one
// (ParseAccount). An edge listener's entry writes, in place of * or -, the projects it serves:
// edge-listener/<machine id>/<KEY>[,<KEY>…]=<account>. It refuses, naming the entry: an entry of
// none of these forms or of an unknown kind; a dispatch, listener or edge-listener entry with no
// machine id, and a daemon or measure entry with one; a machine id that is not one subject token;
// a measure entry with -, since its grant holds a KV read no project scope can hold and it reads
// no project's messages; an edge listener's entry with * or -, or whose projects are no list of
// at most oauthwire.MaxProjects project keys named once each; an account ParseAccount refuses (a
// namespace's default account, a Google default or service-agent account); and an account given
// twice, since one account connects as one service.
func ParseNATSServices(raw string) ([]NATSService, error) {
	var services []NATSService
	seen := map[string]string{}
	for _, text := range strings.Fields(raw) {
		entry, err := parseNATSService(text)
		if err != nil {
			return nil, err
		}
		if other, twice := seen[entry.Account]; twice {
			return nil, fmt.Errorf("%s names the account %s in both %q and %q; one account connects as one service, so name it once", NATSServicesVariable, entry.Account, other, text)
		}
		seen[entry.Account] = text
		services = append(services, entry)
	}
	return services, nil
}

func parseNATSService(text string) (NATSService, error) {
	form := func() error {
		return fmt.Errorf("%s must be <kind>/<machine id or ->/<* or ->=<account> entries, an edge-listener entry's projects <KEY>[,<KEY>…] in place of * or -, the kind one of dispatch, listener, edge-listener, daemon or measure and the account system:serviceaccount:<namespace>:<name> or google:<service account email>, got %q", NATSServicesVariable, text)
	}
	head, account, ok := strings.Cut(text, "=")
	if !ok {
		return NATSService{}, form()
	}
	parts := strings.Split(head, "/")
	if len(parts) != 3 {
		return NATSService{}, form()
	}
	entry := NATSService{Kind: NATSServiceKind(parts[0]), Text: text}
	machineID, scope := parts[1], parts[2]
	switch entry.Kind {
	case NATSDispatch, NATSListener, NATSEdgeListener:
		if machineID == noMachineID {
			return NATSService{}, fmt.Errorf("%s entry %q names no machine id, which the %s grant is built for", NATSServicesVariable, text, entry.Kind)
		}
		if !natsgrant.SessionToken(machineID) {
			return NATSService{}, fmt.Errorf("%s entry %q names a machine id that is not one subject token", NATSServicesVariable, text)
		}
		entry.MachineID = machineID
	case NATSDaemon, NATSMeasure:
		if machineID != noMachineID {
			return NATSService{}, fmt.Errorf("%s entry %q names a machine id, which a %s's grant does not take; write -", NATSServicesVariable, text, entry.Kind)
		}
	default:
		return NATSService{}, form()
	}
	switch {
	case entry.Kind == NATSEdgeListener:
		projects, err := edgeProjects(text, scope)
		if err != nil {
			return NATSService{}, err
		}
		entry.Projects = projects
	case scope == allProjectsScope:
		entry.AllProjects = true
	case scope == exchangeScope:
		if entry.Kind == NATSMeasure {
			return NATSService{}, fmt.Errorf("%s entry %q holds the measurement job to its exchange projects, which its grant cannot be held to: it reads a KV bucket and no project's messages, so write *", NATSServicesVariable, text)
		}
	default:
		return NATSService{}, form()
	}
	parsed, err := ParseAccount(NATSServicesVariable, text, account)
	if errors.Is(err, ErrAccountForm) {
		return NATSService{}, form()
	}
	if err != nil {
		return NATSService{}, err
	}
	entry.Account, entry.Google = parsed.Address, parsed.Google
	return entry, nil
}

// edgeProjects reads an edge listener's scope, the projects it serves: project keys separated by
// commas, at most oauthwire.MaxProjects, each named once, answered sorted. * and - are refused:
// an edge listener serves only the projects its entry names.
func edgeProjects(text, scope string) ([]string, error) {
	if scope == allProjectsScope || scope == exchangeScope {
		return nil, fmt.Errorf("%s entry %q gives an edge listener %s, and an edge listener serves only the projects its entry names: write them as <KEY>[,<KEY>…]", NATSServicesVariable, text, scope)
	}
	projects := strings.Split(scope, ",")
	slices.Sort(projects)
	if err := oauthwire.ValidProjects(projects); err != nil {
		return nil, fmt.Errorf("%s entry %q lists projects that are no list of project keys: %w", NATSServicesVariable, text, err)
	}
	return projects, nil
}

// EdgeListeners is each edge listener's entry of Services as the broker's API reads it
// (api.Deps.EdgeListeners): the account its platform token names and the projects the entry
// lists, the projects GET /v1/project-repositories answers it about. An entry of any other kind
// is none, whatever projects its own grant reaches.
func (n NATSCallout) EdgeListeners() []api.EdgeListener {
	var edges []api.EdgeListener
	for _, entry := range n.Services {
		if entry.Kind == NATSEdgeListener {
			edges = append(edges, api.EdgeListener{Account: entry.Account, Google: entry.Google, Projects: slices.Clone(entry.Projects)})
		}
	}
	return edges
}
