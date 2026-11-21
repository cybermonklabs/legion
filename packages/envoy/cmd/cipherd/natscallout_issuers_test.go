// natscallout_issuers_test.go drives the REAL compiled broker running the NATS auth service in
// front of a hub with an auth callout, trusting two named Kubernetes issuers
// (BROKER_K8S_OIDC_ISSUER=a=<URL>,b=<URL>), each a local OpenID issuer standing in for one
// cluster: a service's connection is admitted only with a token of an issuer its
// BROKER_NATS_SERVICES entry names, and an entry that names none takes the first issuer alone.
// Every connection and its answer is logged, with no token, as the live run's transcript.
package main

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
)

// natsAudience is BROKER_NATS_K8S_AUDIENCE, the audience a service's projected token for the hub
// carries, another than the one the pods enroll with (enrolmentAudience).
const natsAudience = "envoy-nats"

// natsToken is account's projected token whose iss is cluster's, signed with key, for
// natsAudience, issued a few seconds ago, living an hour and bound to no pod: the token a service
// presents to the hub.
func natsToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account string) string {
	t.Helper()
	claims := cluster.Claims(account, natsAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return cluster.Mint(t, key, claims)
}

// connectService connects to the hub at url as a service presenting token, as natscred's service
// credential does, logs the answer, and answers the connect's error, nil where the hub admitted it.
func connectService(t *testing.T, url, token string) error {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, token), nats.NoReconnect(), nats.Timeout(5*time.Second))
	if err != nil {
		t.Logf("> CONNECT %s as %s\n< %v", url, natscred.ServiceUser, err)
		return err
	}
	t.Logf("> CONNECT %s as %s\n< admitted", url, natscred.ServiceUser)
	nc.Close()
	return nil
}

// TestANatsServiceIsAdmittedFromTheIssuersItsEntryNames: with two named issuers, A and B, a
// BROKER_NATS_SERVICES entry that names no issuer admits its subject's token of A, the first
// issuer, and refuses its subject's token of B; an entry that names A and B admits both; and a
// subject no entry lists is refused with a token of either. Each refusal is the auth service's
// service_not_listed, the refusal of a subject no entry lists, and none is a token it could not
// verify: B's token of the entry naming none verifies, against B's keys, for
// BROKER_NATS_K8S_AUDIENCE, which the broker reads through the issuer set its pod routes read.
func TestANatsServiceIsAdmittedFromTheIssuersItsEntryNames(t *testing.T) {
	const (
		namingNone = "system:serviceaccount:legion-system:legion-daemon-one"
		namingBoth = "system:serviceaccount:legion-system:legion-daemon-both"
		stranger   = "system:serviceaccount:example-ns:example-stranger"
	)
	keys := testnats.NewCalloutKeys(t)
	hub := testnats.StartWithCallout(t, keys)
	a, b := oidctest.New(t), oidctest.New(t)
	aKey, bKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key")
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b),
		"BROKER_NATS_URLS="+hub,
		"BROKER_NATS_SEED_FILE="+writeTempFile(t, "service.seed", []byte(keys.ServiceSeed+"\n")),
		"BROKER_NATS_ISSUER_SEED_FILE="+writeTempFile(t, "issuer.seed", []byte(keys.IssuerSeed+"\n")),
		"BROKER_NATS_XKEY_SEED_FILE="+writeTempFile(t, "xkey.seed", []byte(keys.XKeySeed+"\n")),
		"BROKER_NATS_SERVICES=daemon/-/*="+namingNone+" daemon/-/*="+namingBoth+"=a,b",
		"BROKER_NATS_K8S_AUDIENCE="+natsAudience,
	)})
	broker.Await(t, "broker: the NATS auth service is answering")
	tokenOf := map[string]func(t *testing.T, account string) string{
		"A": func(t *testing.T, account string) string { return natsToken(t, a, aKey, account) },
		"B": func(t *testing.T, account string) string { return natsToken(t, b, bKey, account) },
	}

	refused := 0
	for _, c := range []struct {
		name, account, issuer string
		admitted              bool
	}{
		{"the entry naming no issuer admits its subject's token of A", namingNone, "A", true},
		{"the entry naming no issuer refuses its subject's token of B", namingNone, "B", false},
		{"the entry naming A and B admits its subject's token of A", namingBoth, "A", true},
		{"the entry naming A and B admits its subject's token of B", namingBoth, "B", true},
		{"a subject no entry lists is refused from A", stranger, "A", false},
		{"a subject no entry lists is refused from B", stranger, "B", false},
	} {
		if !c.admitted {
			refused++
		}
		t.Run(c.name, func(t *testing.T) {
			err := connectService(t, hub, tokenOf[c.issuer](t, c.account))
			switch {
			case c.admitted && err != nil:
				t.Fatalf("the connect = %v, want it admitted", err)
			case !c.admitted && err == nil:
				t.Fatal("the hub admitted the connection, want it refused")
			case !c.admitted && !errors.Is(err, nats.ErrAuthorization):
				t.Fatalf("the connect = %v, want the hub's authorization refusal", err)
			}
		})
	}

	// Each refusal is counted as the service counts a subject no entry lists, and none as a token
	// it could not verify.
	const (
		notListed   = `kind="service",outcome="refused",reason="service_not_listed"`
		notVerified = `kind="service",outcome="refused",reason="service_token_not_verified"`
	)
	var decisions map[string]float64
	if !testwait.Holds(awaitTimeout, listeningPoll, func() bool {
		decisions = scrapeMetric(t, broker.MetricsAddr(t), "broker_nats_callout_decisions_total")
		return decisions[notListed] >= float64(refused)
	}) || decisions[notListed] != float64(refused) || decisions[notVerified] != 0 {
		t.Fatalf("broker_nats_callout_decisions_total = %v, want %d refusals %s and none %s", decisions, refused, notListed, notVerified)
	}
}
