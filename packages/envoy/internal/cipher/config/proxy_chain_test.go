// packages/envoy/internal/cipher/config/proxy_chain_test.go
// The settings of the proxy's chain: the pool a pod asks for its intermediate, the roots every
// release carries beside its own, and the local stack's three settings, each refused in a
// cluster.
package config

import (
	"strings"
	"testing"
	"time"
)

const testCAPool = "projects/example-project/locations/us-west1/caPools/proxy"

// poolEnv is airlockEnv with the pool named and the mounted pair taken out, which is what a
// deployment that takes its intermediates from Certificate Authority Service sets.
func poolEnv() map[string]string {
	e := airlockEnv()
	delete(e, "BROKER_AIRLOCK_CA_CERT_FILE")
	delete(e, "BROKER_AIRLOCK_CA_KEY_FILE")
	e["BROKER_PROXY_CA_POOL"] = testCAPool
	return e
}

// TestThePoolStandsInForTheMountedAuthority: with BROKER_PROXY_CA_POOL set the mounted pair is
// not required, and the pool is read as written; without either, the refusal names both ways.
func TestThePoolStandsInForTheMountedAuthority(t *testing.T) {
	proxied := []string{"DEEL_API_KEY"}
	cfg, err := Load(env(poolEnv()))
	if err != nil {
		t.Fatalf("Load with the pool and no mounted pair: %v", err)
	}
	if cfg.ProxyCAPool != testCAPool {
		t.Errorf("ProxyCAPool = %q; want %q", cfg.ProxyCAPool, testCAPool)
	}
	if err := cfg.RequireAirlock(proxied); err != nil {
		t.Fatalf("RequireAirlock with the pool and no mounted pair: %v; want nil", err)
	}
	e := poolEnv()
	delete(e, "BROKER_PROXY_CA_POOL")
	if cfg, err = Load(env(e)); err != nil {
		t.Fatal(err)
	}
	err = cfg.RequireAirlock(proxied)
	if err == nil || !strings.Contains(err.Error(), "BROKER_PROXY_CA_POOL") || !strings.Contains(err.Error(), "missing BROKER_AIRLOCK_CA_CERT_FILE, BROKER_AIRLOCK_CA_KEY_FILE") {
		t.Errorf("RequireAirlock with neither the pool nor the pair = %v; want a refusal naming the pool and the missing pair", err)
	}
}

// TestThePoolIsHeldToItsShape: a pool's resource name, and nothing else.
func TestThePoolIsHeldToItsShape(t *testing.T) {
	for _, value := range []string{
		"proxy",
		"projects/example-project/caPools/proxy",
		"projects/example-project/locations/us-west1/caPools/proxy/certificates",
		"https://privateca.googleapis.com/v1/" + testCAPool,
		"projects/example project/locations/us-west1/caPools/proxy",
	} {
		e := poolEnv()
		e["BROKER_PROXY_CA_POOL"] = value
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_PROXY_CA_POOL") {
			t.Errorf("Load with BROKER_PROXY_CA_POOL=%q = %v; want a refusal naming the setting", value, err)
		}
	}
}

// TestTheReleaseRootsFileIsReadAsWritten: the setting is a path, held as written.
func TestTheReleaseRootsFileIsReadAsWritten(t *testing.T) {
	e := poolEnv()
	e["BROKER_PROXY_RELEASE_ROOTS_FILE"] = " /etc/broker/release-roots/roots.pem "
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyReleaseRootsFile != "/etc/broker/release-roots/roots.pem" {
		t.Errorf("ProxyReleaseRootsFile = %q; want the path", cfg.ProxyReleaseRootsFile)
	}
}

// TestTheProxyTestSettingsAreRead: the stand-in's URL, the clock file and the renewal margin,
// 0 included, which is the negative control's.
func TestTheProxyTestSettingsAreRead(t *testing.T) {
	e := poolEnv()
	e["BROKER_PROXY_TEST_CA_URL"] = "http://127.0.0.1:4100"
	e["BROKER_PROXY_TEST_CLOCK_OFFSET_FILE"] = "/tmp/stack/clock-offset"
	e["BROKER_PROXY_TEST_RENEWAL_MARGIN"] = "0s"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyTestCAURL != "http://127.0.0.1:4100" || cfg.ProxyTestClockOffsetFile != "/tmp/stack/clock-offset" {
		t.Errorf("read %q and %q; want the URL and the file", cfg.ProxyTestCAURL, cfg.ProxyTestClockOffsetFile)
	}
	if cfg.ProxyTestRenewalMargin == nil || *cfg.ProxyTestRenewalMargin != 0 {
		t.Errorf("ProxyTestRenewalMargin = %v; want 0, set", cfg.ProxyTestRenewalMargin)
	}
	e["BROKER_PROXY_TEST_RENEWAL_MARGIN"] = "36h"
	if cfg, err = Load(env(e)); err != nil || cfg.ProxyTestRenewalMargin == nil || *cfg.ProxyTestRenewalMargin != 36*time.Hour {
		t.Errorf("BROKER_PROXY_TEST_RENEWAL_MARGIN=36h reads %v, %v; want 36h", cfg.ProxyTestRenewalMargin, err)
	}
	if cfg, err = Load(env(poolEnv())); err != nil || cfg.ProxyTestRenewalMargin != nil {
		t.Errorf("an unset margin reads %v, %v; want nil", cfg.ProxyTestRenewalMargin, err)
	}
}

// TestTheProxyTestSettingsAreRefusedWhereTheyCannotMeanAnything: a stand-in URL or a margin
// with no pool, a URL that is no http URL and a margin that is no duration of 0 or more.
func TestTheProxyTestSettingsAreRefusedWhereTheyCannotMeanAnything(t *testing.T) {
	for _, row := range []struct {
		name, setting, value string
		pool                 bool
	}{
		{"a stand-in URL with no pool", "BROKER_PROXY_TEST_CA_URL", "http://127.0.0.1:4100", false},
		{"a margin with no pool", "BROKER_PROXY_TEST_RENEWAL_MARGIN", "0s", false},
		{"a stand-in URL that is no URL", "BROKER_PROXY_TEST_CA_URL", "127.0.0.1:4100", true},
		{"a margin that is no duration", "BROKER_PROXY_TEST_RENEWAL_MARGIN", "two days", true},
		{"a margin below 0", "BROKER_PROXY_TEST_RENEWAL_MARGIN", "-1h", true},
	} {
		e := airlockEnv()
		if row.pool {
			e = poolEnv()
		}
		e[row.setting] = row.value
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), row.setting) {
			t.Errorf("%s: Load = %v; want a refusal naming %s", row.name, err, row.setting)
		}
	}
}

// TestTheProxyTestSettingsAreRefusedInACluster: each of the three test settings is the local
// stack's alone, so a broker that KUBERNETES_SERVICE_HOST says runs in a cluster refuses to start
// with it, naming it, and starts with none of them. Outside a cluster each is taken.
func TestTheProxyTestSettingsAreRefusedInACluster(t *testing.T) {
	for setting, value := range map[string]string{
		"BROKER_PROXY_TEST_CA_URL":            "http://127.0.0.1:4100",
		"BROKER_PROXY_TEST_CLOCK_OFFSET_FILE": "/tmp/stack/clock-offset",
		"BROKER_PROXY_TEST_RENEWAL_MARGIN":    "0s",
	} {
		e := poolEnv()
		e[setting] = value
		if _, err := Load(env(e)); err != nil {
			t.Fatalf("Load with %s outside a cluster: %v", setting, err)
		}
		e["KUBERNETES_SERVICE_HOST"] = "34.118.224.1"
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
			t.Errorf("Load with %s in a cluster = %v; want a refusal naming it and KUBERNETES_SERVICE_HOST", setting, err)
		}
	}
	e := poolEnv()
	e["KUBERNETES_SERVICE_HOST"] = "34.118.224.1"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("Load in a cluster with none of the test settings: %v", err)
	}
}
