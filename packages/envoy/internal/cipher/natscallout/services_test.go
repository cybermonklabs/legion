package natscallout

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	daemonAccount   = "system:serviceaccount:example-ns:example-daemon"
	measureAccount  = "envoy-measure@example-project.iam.gserviceaccount.com"
)

// oneIssuer is BROKER_K8S_OIDC_ISSUER as one bare URL, the one issuer every deployment had before
// it named several, which has no name; twoIssuers names two, first and second.
var (
	oneIssuer  = []oidc.NamedIssuer{{URL: "https://cluster.example"}}
	twoIssuers = []oidc.NamedIssuer{{Name: "first", URL: "https://first.example"}, {Name: "second", URL: "https://second.example"}}
)

// Each entry is read as <kind>/<machine id or ->/<* or ->=<account>, the account written as
// BROKER_API_ACCOUNTS writes one; a Kubernetes account's token is taken from the one issuer of a
// bare BROKER_K8S_OIDC_ISSUER URL.
func TestParseServicesReadsEachEntry(t *testing.T) {
	services, err := ParseServices("listener/dev-stack/*="+listenerAccount+"\n daemon/-/-="+daemonAccount+
		" measure/-/*=google:"+measureAccount+" dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch", oneIssuer)
	require.NoError(t, err)
	require.Equal(t, []ServiceEntry{
		{Kind: KindListener, MachineID: "dev-stack", Account: listenerAccount, Issuers: []string{""}, AllProjects: true},
		{Kind: KindDaemon, Account: daemonAccount, Issuers: []string{""}},
		{Kind: KindMeasure, Account: measureAccount, Google: true, AllProjects: true},
		{Kind: KindDispatch, MachineID: "example-dispatch", Account: "system:serviceaccount:dispatch:dispatch", Issuers: []string{""}, AllProjects: true},
	}, services)
	none, err := ParseServices("  ", oneIssuer)
	require.NoError(t, err)
	require.Empty(t, none)
}

// A Kubernetes entry may end with =<issuer name>[,<issuer name>…], the issuers of
// BROKER_K8S_OIDC_ISSUER its account's token is taken from, as a BROKER_API_ACCOUNTS entry may: an
// entry that names none takes the first issuer alone, one that names some takes those in its
// order, and a Google entry takes none.
func TestParseServicesReadsTheIssuersAnEntryNames(t *testing.T) {
	services, err := ParseServices("daemon/-/*="+daemonAccount+" listener/dev-stack/*="+listenerAccount+"=second,first"+
		" measure/-/*=google:"+measureAccount, twoIssuers)
	require.NoError(t, err)
	require.Equal(t, []ServiceEntry{
		{Kind: KindDaemon, Account: daemonAccount, Issuers: []string{"first"}, AllProjects: true},
		{Kind: KindListener, MachineID: "dev-stack", Account: listenerAccount, Issuers: []string{"second", "first"}, AllProjects: true},
		{Kind: KindMeasure, Account: measureAccount, Google: true, AllProjects: true},
	}, services)
}

// The measurement job's grant holds a KV read, which no project scope can hold, and it reads no
// project's messages, so its entry takes the whole grant and is refused at the broker's start
// naming the entry when it asks for its exchange projects.
func TestParseServicesRefusesProjectsForMeasure(t *testing.T) {
	_, err := ParseServices("measure/-/-=google:"+measureAccount, oneIssuer)
	require.ErrorContains(t, err, `"measure/-/-=google:`+measureAccount+`"`)
	require.ErrorContains(t, err, "measure")
	services, err := ParseServices("measure/-/*=google:"+measureAccount, oneIssuer)
	require.NoError(t, err)
	require.True(t, services[0].AllProjects)
}

// Every entry the broker could not serve is refused at start, naming the entry, and so is every
// issuer name the broker could not take an account's token from: one BROKER_K8S_OIDC_ISSUER does
// not hold, one named twice, one on a google: entry, and any while that list is one bare URL.
func TestParseServicesRefusals(t *testing.T) {
	const (
		unheld     = "daemon/-/*=" + daemonAccount + "=third"
		twice      = "daemon/-/*=" + daemonAccount + "=first,second,first"
		onGoogle   = "measure/-/*=google:" + measureAccount + "=first"
		besideBare = "daemon/-/*=" + daemonAccount + "=first"
	)
	for name, row := range map[string]struct {
		raw     string
		issuers []oidc.NamedIssuer
		want    []string
	}{
		"an unknown kind":                       {"relay/-/*=" + daemonAccount, oneIssuer, nil},
		"a dispatch entry with no machine id":   {"dispatch/-/*=" + daemonAccount, oneIssuer, nil},
		"a listener entry with no machine id":   {"listener/-/*=" + listenerAccount, oneIssuer, nil},
		"a daemon entry with a machine id":      {"daemon/host-a/-=" + daemonAccount, oneIssuer, nil},
		"a machine id that is no subject token": {"listener/a.b/*=" + listenerAccount, oneIssuer, nil},
		"a scope that is neither":               {"daemon/-/PROJ=" + daemonAccount, oneIssuer, nil},
		"no account":                            {"daemon/-/-", oneIssuer, nil},
		"too few parts":                         {"daemon/-=" + daemonAccount, oneIssuer, nil},
		"an account of neither form":            {"daemon/-/-=example-daemon", oneIssuer, nil},
		"a namespace's default account":         {"daemon/-/-=system:serviceaccount:example-ns:default", oneIssuer, nil},
		"a Google default account":              {"measure/-/*=google:123-compute@developer.gserviceaccount.com", oneIssuer, nil},
		"a Google service agent":                {"measure/-/*=google:service-123@gcp-sa-pubsub.iam.gserviceaccount.com", oneIssuer, nil},
		"an account given twice":                {"daemon/-/-=" + daemonAccount + " listener/host-a/*=" + daemonAccount, oneIssuer, nil},
		"an issuer the list does not hold":      {unheld, twoIssuers, []string{`"` + unheld + `"`, `"third"`, "first, second"}},
		"one issuer named twice":                {twice, twoIssuers, []string{`"` + twice + `"`, `"first"`, "twice"}},
		"an issuer name on a google: entry":     {onGoogle, twoIssuers, []string{`"` + onGoogle + `"`, "Google"}},
		"an issuer name beside one bare URL":    {besideBare, oneIssuer, []string{`"` + besideBare + `"`, "BROKER_K8S_OIDC_ISSUER", "bare"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServices(row.raw, row.issuers)
			require.Error(t, err)
			require.Contains(t, err.Error(), "BROKER_NATS_SERVICES")
			for _, want := range row.want {
				require.Contains(t, err.Error(), want)
			}
		})
	}
}
