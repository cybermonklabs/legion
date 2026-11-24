package boot_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
)

const (
	refusalDaemonAccount  = "system:serviceaccount:example-ns:example-daemon"
	refusalMeasureAccount = "example-measure@example-project.iam.gserviceaccount.com"
)

// open is Open over cfg with keys read from the stand-in issuers, answering its refusal.
func open(t *testing.T, cfg config.Config) error {
	t.Helper()
	_, err := boot.Open(context.Background(), cfg, (&countingKeys{reads: map[string]int{}}).keys)
	return err
}

// requireRefusal fails unless err is a refusal naming every one of names.
func requireRefusal(t *testing.T, err error, names ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Open = nil error, want a refusal naming %v", names)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Open refused with %q, which does not name %q", err, name)
		}
	}
}

// TestAnAccountOfAKindThisBuildLacksIsRefusedAtBoot: an API caller or a NATS service of a kind no
// provider of this build verifies could never authenticate, so the start refuses it, naming the
// setting and the kind, rather than refusing every one of its calls with nothing naming the cause
// (Review focus 4).
func TestAnAccountOfAKindThisBuildLacksIsRefusedAtBoot(t *testing.T) {
	listener, known := api.AccountFor("listener")
	if !known {
		t.Fatal(`api.AccountFor("listener") knows no such caller`)
	}
	cfg := everyIssuer(newIssuers(t))
	cfg.APIAccounts = map[identity.Account]api.Account{{Kind: "nosuch", Address: "example"}: listener}
	requireRefusal(t, open(t, cfg), "BROKER_API_ACCOUNTS", "nosuch")

	cfg = everyIssuer(newIssuers(t))
	cfg.NATS.Services = []config.NATSService{{Kind: config.NATSEdgeListener, MachineID: "edge-a", Account: identity.Account{Kind: "nosuch", Address: "example"}, Text: "edge-listener/edge-a/PRIV=example"}}
	requireRefusal(t, open(t, cfg), "BROKER_NATS_SERVICES", "nosuch")
}

// TestANATSEntryWithNoAudienceOfItsKindIsRefusedAtBoot: a listed service whose kind of token has
// no NATS audience could never connect, so the start of a broker running the NATS auth service
// refuses it, naming the entry and the audience it needs. An edge listener's entry is held to its
// audience as every service's is, since the auth service answers its connection with the edge's
// grant. A broker that runs no auth service holds no entry to an audience.
func TestANATSEntryWithNoAudienceOfItsKindIsRefusedAtBoot(t *testing.T) {
	for _, c := range []struct {
		name, services string
		k8sNamed       string
		googleNamed    string
	}{
		{name: "a daemon and a measurement job", services: "daemon/-/*=" + refusalDaemonAccount + " measure/-/*=google:" + refusalMeasureAccount,
			k8sNamed: refusalDaemonAccount, googleNamed: refusalMeasureAccount},
		{name: "two edge listeners", services: "edge-listener/edge-a/PRIV=" + refusalDaemonAccount + " edge-listener/edge-g/OTHER=google:" + refusalMeasureAccount,
			k8sNamed: "edge-listener/edge-a", googleNamed: "edge-listener/edge-g"},
	} {
		t.Run(c.name, func(t *testing.T) {
			services, err := config.ParseNATSServices(c.services)
			if err != nil {
				t.Fatalf("ParseNATSServices: %v", err)
			}
			running := func(k8sAudience, googleAudience string) config.Config {
				cfg := everyIssuer(newIssuers(t))
				cfg.NATS = config.NATSCallout{URLs: []string{"nats://hub.example:4222"}, Services: services, K8sAudience: k8sAudience, GoogleAudience: googleAudience}
				return cfg
			}
			if err := open(t, running("envoy-nats", "envoy-nats-google")); err != nil {
				t.Fatalf("Open with both NATS audiences = %v, want no refusal", err)
			}
			requireRefusal(t, open(t, running("", "envoy-nats-google")), "BROKER_NATS_K8S_AUDIENCE", c.k8sNamed, "a Kubernetes account")
			requireRefusal(t, open(t, running("envoy-nats", "")), "BROKER_NATS_GOOGLE_AUDIENCE", c.googleNamed, "a Google account")

			notRunning := everyIssuer(newIssuers(t))
			notRunning.NATS = config.NATSCallout{Services: services}
			if err := open(t, notRunning); err != nil {
				t.Fatalf("Open with no NATS auth service = %v, want no refusal: only the auth service verifies a NATS token", err)
			}
		})
	}
	if err := open(t, config.Config{NATS: config.NATSCallout{URLs: []string{"nats://hub.example:4222"}}}); err != nil {
		t.Fatalf("Open with the auth service and no entry = %v, want no refusal", err)
	}
}
