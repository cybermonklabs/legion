package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/natsgrant"
)

// NATSServicesVariable is the setting ParseNATSServices reads.
const NATSServicesVariable = "BROKER_NATS_SERVICES"

// NATSServiceKind is which kind of service a BROKER_NATS_SERVICES entry names.
type NATSServiceKind string

// The kinds of entry. Dispatch, a listener, the webhook listener, a Legion daemon, the measurement
// job, an edge listener, the deployment's one operator identity (natsgrant.Operator, of the writer
// hold's phase) and the writer hold's probe (natsgrant.HoldProbe) are services the NATS auth
// service admits by their platform identity, each answered with its kind's grant of natsgrant, an
// edge listener's built for the projects its entry lists (natsgrant.Edge). The webhook listener is
// the listener that receives GitHub's webhook (its ENVOY_WEBHOOKS names github): its grant is a
// listener's with GitHub's lane off its publish deny (natsgrant.WebhookListener), so it publishes
// every GitHub event, which a listener's grant refuses. An edge listener's grant keeps a
// listener's deny of Dispatch's lane and GitHub's lane. An edge listener's entry is also read for
// its projects (NATSCallout.EdgeListeners), whether or not the broker runs the auth service.
const (
	NATSDispatch        NATSServiceKind = "dispatch"
	NATSListener        NATSServiceKind = "listener"
	NATSWebhookListener NATSServiceKind = "webhook-listener"
	NATSDaemon          NATSServiceKind = "daemon"
	NATSMeasure         NATSServiceKind = "measure"
	NATSOperator        NATSServiceKind = "operator"
	NATSProbe           NATSServiceKind = "probe"
	NATSEdgeListener    NATSServiceKind = "edge-listener"
)

// onlyOne reports whether a list may name kind once at most: the deployment has one operator
// identity and one hold probe.
func onlyOne(kind NATSServiceKind) bool { return kind == NATSOperator || kind == NATSProbe }

// NATSService is one BROKER_NATS_SERVICES entry, as ParseNATSServices reads it at load. It is the
// one reading of an entry: the NATS auth service admits services by it, the broker's API reads the
// edge listeners from it, and every check of Load that asks an entry's kind or account asks it.
type NATSService struct {
	Kind NATSServiceKind
	// MachineID is the machine id the grant is built for, which Dispatch's, a listener's and the
	// webhook listener's name their durable and queue group by, and an edge listener's its durable
	// and its inbox prefix (natsgrant.EdgeInboxPrefix); required for those four kinds and refused
	// for the others.
	MachineID string
	// Account is the platform account whose token the service presents: a ServiceAccount
	// subject, or a Google account address.
	Account string
	// Google is whether Account is a Google service account.
	Google bool
	// Issuers names the issuers of BROKER_K8S_OIDC_ISSUER a Kubernetes account's token is taken
	// from (AccountIssuers): those the entry names, or the first issuer of the list alone where it
	// names none. A Google account has none.
	Issuers []string
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
// ->=<account>[=<issuer name>[,<issuer name>…]] entries, in order, where * is the grant whole and
// - means "the projects this account's exchange entry lists", and the account is written as
// BROKER_API_ACCOUNTS writes one (ParseAccount), and so are the issuer names a Kubernetes
// account's token is taken from, of issuers, the list BROKER_K8S_OIDC_ISSUER names
// (CutIssuerNames, AccountIssuers). An edge listener's entry writes, in place of * or -, the
// projects it serves, and names its issuers as every other entry does:
// edge-listener/<machine id>/<KEY>[,<KEY>…]=<account>[=<issuer name>[,<issuer name>…]]. It
// refuses, naming the entry: an entry of none of these forms or of an unknown kind; a dispatch,
// listener, webhook-listener or edge-listener entry with no machine id, and a daemon, measure,
// operator or probe entry with one; an empty machine id, one holding a wildcard, a space or a
// control character (natsgrant.MachineIDToken), and one the listener itself refuses
// (bus.CheckListenerMachineID: one holding the escape character '%', or whose durable name is too
// long), so the auth service takes every machine id a listener starts with, a dotted one among
// them, whose names the grant escapes as the listener does (bus.ListenerSpelling), an edge
// listener's inbox prefix among them; a measure entry with -, since its grant holds a KV read no
// project scope can hold and it reads no project's messages; an operator or probe entry with -,
// since the writer hold and the buckets command reach no project's messages and no project scope
// can hold a stream create; a second operator or probe entry, since a deployment has one operator
// identity and one probe; an edge listener's entry with * or -, or whose projects are no list of
// at most oauthwire.MaxProjects project keys named once each; an account ParseAccount refuses (a
// namespace's default account, a Google default or service-agent account); an entry ending in "="
// with no issuer name after it, and the issuer names AccountIssuers refuses (one the list does not
// hold, one named twice, one on a google: entry, and any while the list is one bare URL or empty);
// and an account given twice, since one account connects as one service.
func ParseNATSServices(raw string, issuers []oidc.NamedIssuer) ([]NATSService, error) {
	var services []NATSService
	seen := map[string]string{}
	kinds := map[NATSServiceKind]string{}
	for _, text := range strings.Fields(raw) {
		entry, err := parseNATSService(text, issuers)
		if err != nil {
			return nil, err
		}
		if other, twice := seen[entry.Account]; twice {
			return nil, fmt.Errorf("%s names the account %s in both %q and %q; one account connects as one service, so name it once", NATSServicesVariable, entry.Account, other, text)
		}
		if other, twice := kinds[entry.Kind]; twice && onlyOne(entry.Kind) {
			return nil, fmt.Errorf("%s lists a second %s entry, %q, beside %q; a deployment has one %s identity, so list one", NATSServicesVariable, entry.Kind, text, other, entry.Kind)
		}
		seen[entry.Account] = text
		kinds[entry.Kind] = text
		services = append(services, entry)
	}
	return services, nil
}

func parseNATSService(text string, issuers []oidc.NamedIssuer) (NATSService, error) {
	form := func() error {
		return fmt.Errorf("%s must be <kind>/<machine id or ->/<* or ->=<account>[=<issuer name>[,<issuer name>…]] entries, an edge-listener entry's projects <KEY>[,<KEY>…] in place of * or -, the kind one of dispatch, listener, webhook-listener, edge-listener, daemon, measure, operator or probe and the account system:serviceaccount:<namespace>:<name> or google:<service account email>, got %q", NATSServicesVariable, text)
	}
	withAccount, names, named := CutIssuerNames(text)
	head, account, ok := strings.Cut(withAccount, "=")
	if !ok || !named {
		return NATSService{}, form()
	}
	parts := strings.Split(head, "/")
	if len(parts) != 3 {
		return NATSService{}, form()
	}
	entry := NATSService{Kind: NATSServiceKind(parts[0]), Text: text}
	machineID, scope := parts[1], parts[2]
	switch entry.Kind {
	case NATSDispatch, NATSListener, NATSWebhookListener, NATSEdgeListener:
		if machineID == noMachineID {
			return NATSService{}, fmt.Errorf("%s entry %q names no machine id, which the %s grant is built for", NATSServicesVariable, text, entry.Kind)
		}
		if !natsgrant.MachineIDToken(machineID) {
			return NATSService{}, fmt.Errorf("%s entry %q names a machine id that is empty or holds a wildcard, a space or a control character", NATSServicesVariable, text)
		}
		if err := bus.CheckListenerMachineID(machineID); err != nil {
			return NATSService{}, fmt.Errorf("%s entry %q names a machine id that %w", NATSServicesVariable, text, err)
		}
		entry.MachineID = machineID
	case NATSDaemon, NATSMeasure, NATSOperator, NATSProbe:
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
		switch entry.Kind {
		case NATSMeasure:
			return NATSService{}, fmt.Errorf("%s entry %q holds the measurement job to its exchange projects, which its grant cannot be held to: it reads a KV bucket and no project's messages, so write *", NATSServicesVariable, text)
		case NATSOperator, NATSProbe:
			return NATSService{}, fmt.Errorf("%s entry %q holds the %s to its exchange projects, which its grant cannot be held to: it reaches no project's messages and no project scope can hold a stream create, so write *", NATSServicesVariable, text, entry.Kind)
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
	if entry.Issuers, err = AccountIssuers(NATSServicesVariable, text, names, entry.Google, issuers); err != nil {
		return NATSService{}, err
	}
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
// (api.Deps.EdgeListeners): the account its platform token names, the issuers a Kubernetes
// account's token is taken from, and the projects the entry lists, the projects
// GET /v1/project-repositories answers it about. An entry of any other kind is none, whatever
// projects its own grant reaches.
func (n NATSCallout) EdgeListeners() []api.EdgeListener {
	var edges []api.EdgeListener
	for _, entry := range n.Services {
		if entry.Kind == NATSEdgeListener {
			edges = append(edges, api.EdgeListener{
				Account: entry.Account, Google: entry.Google,
				Issuers: slices.Clone(entry.Issuers), Projects: slices.Clone(entry.Projects),
			})
		}
	}
	return edges
}
