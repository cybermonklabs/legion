package config

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	listenerAccount  = "system:serviceaccount:example-ns:example-listener"
	daemonAccount    = "system:serviceaccount:example-ns:example-daemon"
	measureAccount   = "envoy-measure@example-project.iam.gserviceaccount.com"
	edgeAccount      = "system:serviceaccount:example-ns:example-edge-listener"
	otherEdgeAccount = "system:serviceaccount:example-ns:example-other-edge-listener"
)

// oneIssuer is BROKER_K8S_OIDC_ISSUER as one bare URL, the one issuer every deployment had before
// it named several, which has no name; twoIssuers names two, first and second.
var (
	oneIssuer  = []oidc.NamedIssuer{{URL: "https://cluster.example"}}
	twoIssuers = []oidc.NamedIssuer{firstIssuer, secondIssuer}
)

// Each entry is read as <kind>/<machine id or ->/<* or ->=<account>, the account written as
// BROKER_API_ACCOUNTS writes one, and keeps its own text for the refusals that name it; a
// Kubernetes account's token is taken from the one issuer of a bare BROKER_K8S_OIDC_ISSUER URL.
func TestParseNATSServicesReadsEachEntry(t *testing.T) {
	listener, daemon := "listener/dev-stack/*="+listenerAccount, "daemon/-/-="+daemonAccount
	measure, dispatch := "measure/-/*=google:"+measureAccount, "dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch"
	services, err := ParseNATSServices(listener+"\n "+daemon+" "+measure+" "+dispatch, oneIssuer)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSListener, MachineID: "dev-stack", Account: listenerAccount, Issuers: []string{""}, AllProjects: true, Text: listener},
		{Kind: NATSDaemon, Account: daemonAccount, Issuers: []string{""}, Text: daemon},
		{Kind: NATSMeasure, Account: measureAccount, Google: true, AllProjects: true, Text: measure},
		{Kind: NATSDispatch, MachineID: "example-dispatch", Account: "system:serviceaccount:dispatch:dispatch", Issuers: []string{""}, AllProjects: true, Text: dispatch},
	}, services)
	none, err := ParseNATSServices("  ", oneIssuer)
	require.NoError(t, err)
	require.Empty(t, none)
}

// A Kubernetes entry may end with =<issuer name>[,<issuer name>…], the issuers of
// BROKER_K8S_OIDC_ISSUER its account's token is taken from, as a BROKER_API_ACCOUNTS entry may: an
// entry that names none takes the first issuer alone, one that names some takes those in its
// order, and a Google entry takes none.
func TestParseNATSServicesReadsTheIssuersAnEntryNames(t *testing.T) {
	daemon, listener := "daemon/-/*="+daemonAccount, "listener/dev-stack/*="+listenerAccount+"=second,first"
	measure := "measure/-/*=google:" + measureAccount
	services, err := ParseNATSServices(daemon+" "+listener+" "+measure, twoIssuers)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSDaemon, Account: daemonAccount, Issuers: []string{"first"}, AllProjects: true, Text: daemon},
		{Kind: NATSListener, MachineID: "dev-stack", Account: listenerAccount, Issuers: []string{"second", "first"}, AllProjects: true, Text: listener},
		{Kind: NATSMeasure, Account: measureAccount, Google: true, AllProjects: true, Text: measure},
	}, services)
}

// An edge listener's entry names its machine id and, in place of a scope, the projects it serves,
// comma separated: edge-listener/<machine id>/<KEY>[,<KEY>…]=<account>[=<issuer name>[,…]]. The
// projects are read sorted, each once, and no other kind's entry carries any.
func TestParseNATSServicesReadsAnEdgeListenersProjects(t *testing.T) {
	edge, listener := "edge-listener/edge-a/PRIV,OTHER="+edgeAccount, "listener/host-a/*="+listenerAccount
	services, err := ParseNATSServices(edge+" "+listener, oneIssuer)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSEdgeListener, MachineID: "edge-a", Account: edgeAccount, Issuers: []string{""}, Projects: []string{"OTHER", "PRIV"}, Text: edge},
		{Kind: NATSListener, MachineID: "host-a", Account: listenerAccount, Issuers: []string{""}, AllProjects: true, Text: listener},
	}, services)
}

// An edge listener's entry names the issuers its account's token is taken from as every other
// entry does, after its account: edge-listener/<machine id>/<KEY>[,<KEY>…]=<account>=<issuer
// name>[,<issuer name>…]. One that names both issuers takes both, in its order, and one that names
// none takes the first alone; the broker's API reads each edge listener with those issuers.
func TestParseNATSServicesReadsTheIssuersAnEdgeListenersEntryNames(t *testing.T) {
	named, unnamed := "edge-listener/edge-a/PRIV="+edgeAccount+"=second,first", "edge-listener/edge-b/OTHER="+otherEdgeAccount
	services, err := ParseNATSServices(named+" "+unnamed, twoIssuers)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSEdgeListener, MachineID: "edge-a", Account: edgeAccount, Issuers: []string{"second", "first"}, Projects: []string{"PRIV"}, Text: named},
		{Kind: NATSEdgeListener, MachineID: "edge-b", Account: otherEdgeAccount, Issuers: []string{"first"}, Projects: []string{"OTHER"}, Text: unnamed},
	}, services)
	require.Equal(t, []api.EdgeListener{
		{Account: edgeAccount, Issuers: []string{"second", "first"}, Projects: []string{"PRIV"}},
		{Account: otherEdgeAccount, Issuers: []string{"first"}, Projects: []string{"OTHER"}},
	}, NATSCallout{Services: services}.EdgeListeners())
}

// EdgeListeners is each edge listener's entry as the broker's API reads it, its account, its
// issuers and its projects, and no entry of another kind: a listener's account is no edge
// listener, whatever projects its own grant reaches.
func TestEdgeListenersAreTheEdgeListenerEntries(t *testing.T) {
	services, err := ParseNATSServices("listener/host-a/*="+listenerAccount+" edge-listener/edge-a/PRIV="+edgeAccount+
		" edge-listener/edge-g/OTHER=google:example-edge@example-project.iam.gserviceaccount.com daemon/-/-="+daemonAccount, oneIssuer)
	require.NoError(t, err)
	require.Equal(t, []api.EdgeListener{
		{Account: edgeAccount, Issuers: []string{""}, Projects: []string{"PRIV"}},
		{Account: "example-edge@example-project.iam.gserviceaccount.com", Google: true, Projects: []string{"OTHER"}},
	}, NATSCallout{Services: services}.EdgeListeners())
	require.Empty(t, NATSCallout{}.EdgeListeners())
}

// The measurement job's grant holds a KV read, which no project scope can hold, and it reads no
// project's messages, so its entry takes the whole grant and is refused at the broker's start
// naming the entry when it asks for its exchange projects.
func TestParseNATSServicesRefusesProjectsForMeasure(t *testing.T) {
	_, err := ParseNATSServices("measure/-/-=google:"+measureAccount, oneIssuer)
	require.ErrorContains(t, err, `"measure/-/-=google:`+measureAccount+`"`)
	require.ErrorContains(t, err, "measure")
	services, err := ParseNATSServices("measure/-/*=google:"+measureAccount, oneIssuer)
	require.NoError(t, err)
	require.True(t, services[0].AllProjects)
}

// Every entry the broker could not serve is refused at start, naming the entry, and so is every
// issuer name the broker could not take an account's token from, an edge listener's as any
// other's: one BROKER_K8S_OIDC_ISSUER does not hold, one named twice, one on a google: entry, and
// any while that list is one bare URL.
func TestParseNATSServicesRefusals(t *testing.T) {
	const (
		unheld     = "daemon/-/*=" + daemonAccount + "=third"
		twice      = "daemon/-/*=" + daemonAccount + "=first,second,first"
		onGoogle   = "measure/-/*=google:" + measureAccount + "=first"
		besideBare = "daemon/-/*=" + daemonAccount + "=first"
		edgeUnheld = "edge-listener/edge-a/PRIV=" + edgeAccount + "=first,third"
		edgeGoogle = "edge-listener/edge-g/PRIV=google:example-edge@example-project.iam.gserviceaccount.com=first"
	)
	for name, row := range map[string]struct {
		raw     string
		issuers []oidc.NamedIssuer
		want    []string
	}{
		"an unknown kind":                            {"relay/-/*=" + daemonAccount, oneIssuer, nil},
		"a dispatch entry with no machine id":        {"dispatch/-/*=" + daemonAccount, oneIssuer, nil},
		"a listener entry with no machine id":        {"listener/-/*=" + listenerAccount, oneIssuer, nil},
		"a daemon entry with a machine id":           {"daemon/host-a/-=" + daemonAccount, oneIssuer, nil},
		"a machine id that is no subject token":      {"listener/a.b/*=" + listenerAccount, oneIssuer, nil},
		"a scope that is neither":                    {"daemon/-/PROJ=" + daemonAccount, oneIssuer, nil},
		"no account":                                 {"daemon/-/-", oneIssuer, nil},
		"too few parts":                              {"daemon/-=" + daemonAccount, oneIssuer, nil},
		"an account of neither form":                 {"daemon/-/-=example-daemon", oneIssuer, nil},
		"a namespace's default account":              {"daemon/-/-=system:serviceaccount:example-ns:default", oneIssuer, nil},
		"a Google default account":                   {"measure/-/*=google:123-compute@developer.gserviceaccount.com", oneIssuer, nil},
		"a Google service agent":                     {"measure/-/*=google:service-123@gcp-sa-pubsub.iam.gserviceaccount.com", oneIssuer, nil},
		"an account given twice":                     {"daemon/-/-=" + daemonAccount + " listener/host-a/*=" + daemonAccount, oneIssuer, nil},
		"an edge listener with no machine id":        {"edge-listener/-/PRIV=" + edgeAccount, oneIssuer, nil},
		"an edge listener naming no project key":     {"edge-listener/edge-a/priv=" + edgeAccount, oneIssuer, nil},
		"an edge listener naming a project twice":    {"edge-listener/edge-a/PRIV,PRIV=" + edgeAccount, oneIssuer, nil},
		"an edge listener naming an empty project":   {"edge-listener/edge-a/PRIV,=" + edgeAccount, oneIssuer, nil},
		"an edge listener naming seventeen projects": {"edge-listener/edge-a/AA,AB,AC,AD,AE,AF,AG,AH,AI,AJ,AK,AL,AM,AN,AO,AP,AQ=" + edgeAccount, oneIssuer, nil},
		"projects for a listener":                    {"listener/host-a/PRIV=" + listenerAccount, oneIssuer, nil},
		"an issuer the list does not hold":           {unheld, twoIssuers, []string{`"` + unheld + `"`, `"third"`, "first, second"}},
		"one issuer named twice":                     {twice, twoIssuers, []string{`"` + twice + `"`, `"first"`, "twice"}},
		"an issuer name on a google: entry":          {onGoogle, twoIssuers, []string{`"` + onGoogle + `"`, "Google"}},
		"an issuer name beside one bare URL":         {besideBare, oneIssuer, []string{`"` + besideBare + `"`, "BROKER_K8S_OIDC_ISSUER", "bare"}},
		"an edge listener naming an unheld issuer":   {edgeUnheld, twoIssuers, []string{`"` + edgeUnheld + `"`, `"third"`, "first, second"}},
		"an issuer name on a google: edge listener":  {edgeGoogle, twoIssuers, []string{`"` + edgeGoogle + `"`, "Google"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNATSServices(row.raw, row.issuers)
			require.Error(t, err)
			require.Contains(t, err.Error(), "BROKER_NATS_SERVICES")
			for _, want := range row.want {
				require.Contains(t, err.Error(), want)
			}
		})
	}
}

// The deployment's one operator identity and the writer hold's one probe are entries of their own
// kinds, each listed once, with no machine id and the whole grant: the hold and the buckets
// command reach no project's messages, and no project scope can hold a stream create. Each names
// its issuers as every other entry does, and one that names none takes the first issuer alone. A
// second entry of either kind, a machine id, the scope -, or an issuer the list does not hold is
// refused, naming the entry.
func TestParseNATSServicesReadsTheOperatorKind(t *testing.T) {
	const operator = "system:serviceaccount:envoy-operator:envoy-operator"
	const probe = "system:serviceaccount:envoy-operator:envoy-hold-probe"
	operatorText, probeText := "operator/-/*="+operator+"=second", "probe/-/*="+probe
	services, err := ParseNATSServices(operatorText+" "+probeText, twoIssuers)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSOperator, Account: operator, Issuers: []string{"second"}, AllProjects: true, Text: operatorText},
		{Kind: NATSProbe, Account: probe, Issuers: []string{"first"}, AllProjects: true, Text: probeText},
	}, services)
	require.Equal(t, "operator/"+operator, services[0].Name())
	for raw, names := range map[string]string{
		"operator/m1/*=" + operator:           "operator/m1/*=" + operator,
		"operator/-/-=" + operator:            "operator/-/-=" + operator,
		"operator/-/*=" + operator + "=third": "operator/-/*=" + operator + "=third",
		"operator/-/*=" + operator + " operator/-/*=system:serviceaccount:envoy-operator:second": "operator/-/*=system:serviceaccount:envoy-operator:second",
		"probe/m1/*=" + probe: "probe/m1/*=" + probe,
		"probe/-/-=" + probe:  "probe/-/-=" + probe,
		"probe/-/*=" + probe + " probe/-/*=system:serviceaccount:envoy-operator:second": "probe/-/*=system:serviceaccount:envoy-operator:second",
	} {
		_, err := ParseNATSServices(raw, twoIssuers)
		require.ErrorContains(t, err, strconv.Quote(names), "ParseNATSServices(%q)", raw)
	}
}

// An edge listener's entry that gives it every project (*) or its exchange's projects (-) is
// refused by its own rule, naming the form an edge entry takes, before its scope is read as a
// list of project keys: an edge listener serves only the projects its entry names.
func TestAnEdgeEntryOfEveryOrTheExchangeProjectsIsRefusedNamingWhy(t *testing.T) {
	for _, scope := range []string{allProjectsScope, exchangeScope} {
		_, err := ParseNATSServices("edge-listener/edge-a/"+scope+"="+edgeAccount, oneIssuer)
		require.ErrorContains(t, err, "an edge listener serves only the projects its entry names", "scope %q", scope)
	}
}
