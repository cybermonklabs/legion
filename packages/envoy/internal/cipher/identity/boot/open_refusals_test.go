package boot_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

const (
	refusalDaemonAccount  = "system:serviceaccount:example-ns:example-daemon"
	refusalMeasureAccount = "example-measure@example-project.iam.gserviceaccount.com"
)

// requireRefusal fails unless err is a refusal naming every one of names.
func requireRefusal(t *testing.T, err error, names ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("= nil error, want a refusal naming %v", names)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refused with %q, which does not name %q", err, name)
		}
	}
}

// TestAnAccountOfAKindThisBuildLacksIsRefused: an API caller or a NATS service of a kind no
// provider of this build verifies could never authenticate, so Serves refuses it, naming the
// setting and the kind, rather than letting the start take it and refuse every one of its calls
// with nothing naming the cause (a build that left the provider out with -tags cipher_no_<kind>).
func TestAnAccountOfAKindThisBuildLacksIsRefused(t *testing.T) {
	settings := settingsOf(t, everyIssuer(newIssuers(t)))
	for _, use := range []identity.Use{identity.UseAPI, identity.UseNATS} {
		requireRefusal(t, settings.Serves("nosuch", use, identity.Site{Setting: "BROKER_API_ACCOUNTS"}), "BROKER_API_ACCOUNTS", "nosuch")
	}
}

// TestEachProviderSaysWhatItsCallersNeed: a caller of a kind a provider holds, where the
// variable its tokens need is unset, is refused in that provider's own words, naming the setting
// and the variable; with the variable set, it is served. An edge listener's refusal names its
// entry too, since the broker's API serves one whether or not the NATS auth service runs.
func TestEachProviderSaysWhatItsCallersNeed(t *testing.T) {
	i := newIssuers(t)
	for _, c := range []struct {
		name     string
		kind     identity.Kind
		use      identity.Use
		unset    string
		needs    string
		env      map[string]string
		fullEnv  map[string]string
		site     identity.Site
		accounts string
	}{
		{name: "a Kubernetes API caller with no cluster", kind: kubernetes.Kind, use: identity.UseAPI,
			needs: "BROKER_K8S_OIDC_ISSUER", env: map[string]string{}, fullEnv: everyIssuer(i),
			site: identity.Site{Setting: "BROKER_API_ACCOUNTS"}},
		{name: "a Google API caller with no audience", kind: google.Kind, use: identity.UseAPI,
			needs: "BROKER_GOOGLE_OIDC_AUDIENCE", env: map[string]string{}, fullEnv: everyIssuer(i),
			site: identity.Site{Setting: "BROKER_API_ACCOUNTS"}},
		{name: "a Kubernetes edge listener with no cluster", kind: kubernetes.Kind, use: identity.UseAPI,
			needs: "BROKER_K8S_OIDC_ISSUER", env: map[string]string{}, fullEnv: everyIssuer(i),
			site: identity.Site{Setting: "BROKER_NATS_SERVICES", Entry: "edge-listener/edge-a/PRIV=" + refusalDaemonAccount, Edge: true}},
		{name: "a Kubernetes NATS service with no NATS audience", kind: kubernetes.Kind, use: identity.UseNATS,
			needs: "BROKER_NATS_K8S_AUDIENCE", env: everyIssuer(i),
			fullEnv: withEnv(everyIssuer(i), map[string]string{"BROKER_NATS_K8S_AUDIENCE": "nats-cluster"}),
			site:    identity.Site{Setting: "BROKER_NATS_SERVICES", Entry: "daemon/" + refusalDaemonAccount}},
		{name: "a Google NATS service with no NATS audience", kind: google.Kind, use: identity.UseNATS,
			needs: "BROKER_NATS_GOOGLE_AUDIENCE", env: everyIssuer(i),
			fullEnv: withEnv(everyIssuer(i), map[string]string{"BROKER_NATS_GOOGLE_AUDIENCE": "nats-google"}),
			site:    identity.Site{Setting: "BROKER_NATS_SERVICES", Entry: "measure/" + refusalMeasureAccount}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := settingsOf(t, c.env).Serves(c.kind, c.use, c.site)
			names := []string{c.needs, c.site.Setting}
			if c.site.Entry != "" {
				names = append(names, c.site.Entry)
			}
			requireRefusal(t, err, names...)
			if err := settingsOf(t, c.fullEnv).Serves(c.kind, c.use, c.site); err != nil {
				t.Errorf("Serves with %s set = %v, want it served", c.needs, err)
			}
		})
	}
}

// TestPodsServedNeedsTheClustersIssuer: a setting about pods needs the pod provider to trust a
// cluster, since a pod's token could be verified against nothing; it names the setting and the
// variables.
func TestPodsServedNeedsTheClustersIssuer(t *testing.T) {
	requireRefusal(t, settingsOf(t, nil).PodsServed("BROKER_SERVICE_LOGINS"), "BROKER_SERVICE_LOGINS", "BROKER_K8S_OIDC_ISSUER")
	if err := settingsOf(t, everyIssuer(newIssuers(t))).PodsServed("BROKER_SERVICE_LOGINS"); err != nil {
		t.Errorf("PodsServed with a cluster = %v, want it served", err)
	}
}

// TestParseAccountAsksEachProviderInRankOrder: an account of each provider's form is read by that
// provider, with its kind; one of no provider's form is ErrAccountForm, whose sentence lists
// every form this build reads.
func TestParseAccountAsksEachProviderInRankOrder(t *testing.T) {
	for _, c := range []struct {
		account string
		want    identity.Account
	}{
		{refusalDaemonAccount, identity.Account{Kind: kubernetes.Kind, Address: refusalDaemonAccount}},
		{"google:" + refusalMeasureAccount, identity.Account{Kind: google.Kind, Address: refusalMeasureAccount}},
	} {
		got, err := boot.ParseAccount("BROKER_API_ACCOUNTS", "listener="+c.account, c.account)
		if err != nil || got != c.want {
			t.Errorf("ParseAccount(%s) = %+v, %v; want %+v", c.account, got, err, c.want)
		}
	}
	_, err := boot.ParseAccount("BROKER_API_ACCOUNTS", "listener=example", "example")
	if err == nil || !strings.Contains(err.Error(), kubernetes.AccountForm) || !strings.Contains(err.Error(), google.AccountForm) {
		t.Fatalf("ParseAccount(an account of no form) = %v, want a refusal listing every form this build reads", err)
	}
}

// withEnv is base with each entry of extra set.
func withEnv(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	for name, value := range base {
		merged[name] = value
	}
	for name, value := range extra {
		merged[name] = value
	}
	return merged
}
