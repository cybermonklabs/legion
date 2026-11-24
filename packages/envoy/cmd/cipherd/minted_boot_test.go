// minted_boot_test.go drives the REAL compiled broker's start check of the dpop blocks
// (config.ProxyMintedRules), each a capability the broker mints and holds: every broker with an
// airlock port serves every minted grant of the database it shares, so every broker holds every
// dpop block to the private destination's one host, whatever it grants itself. The helpers are
// main_test.go's.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// mintedDestination is BROKER_PROXY_PRIVATE_UPSTREAM for the host the dpop blocks below bind:
// an address inside a private range, which the start takes and nothing here dials.
const mintedDestination = "BROKER_PROXY_PRIVATE_UPSTREAM=dispatch.test=http://10.0.0.5:8766"

// dpopKeyLine is the line the broker logs its airlock's DPoP key by, its thumbprint alone.
const dpopKeyLine = "broker: the proxy's DPoP key"

// mintedProfiles is a profiles file of one dpop block per name, each binding hosts (a JSON list's
// inside), carried in Authorization alone with an allow list.
func mintedProfiles(hosts string, names []string) string {
	var profiles []string
	for _, name := range names {
		profiles = append(profiles, `{"name": "`+name+`", "proxy": {"hosts": [`+hosts+`], "headers": ["Authorization"],
	"allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"}}`)
	}
	return `{"profiles": [` + strings.Join(profiles, ", ") + `]}`
}

// TestTheBootHoldsEveryDPoPBlockToItsOwnDestinationsHost: a broker whose profiles hold a dpop block
// binding a "*." name or two hosts, a dpop block with no private destination or with destinations
// of other hosts only, a kind whose hosts hold the dpop block's host, or a destination no dpop
// block binds, refuses to start, naming the profile, the kind or the setting and what is wrong,
// without ever binding. The database it is given is unreachable, so a broker that checked the
// blocks after opening the store would fail on the host instead.
func TestTheBootHoldsEveryDPoPBlockToItsOwnDestinationsHost(t *testing.T) {
	binary := buildBroker(t)
	wildcard := `[{"name": "wildcard", "hosts": ["*.test"], "ways": ["swap"]}]`
	for _, c := range []struct {
		name, profiles, says string
		upstream             []string
	}{
		{"a dpop block of a *. name", mintedProfiles(`"*.test"`, []string{"FIRST_TOKEN"}), "one exact host", []string{mintedDestination}},
		{"a dpop block of two hosts", mintedProfiles(`"dispatch.test", "other.test"`, []string{"FIRST_TOKEN"}), "one exact host", []string{mintedDestination}},
		{"no private destination", mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN"}), "BROKER_PROXY_PRIVATE_UPSTREAM is unset", nil},
		{"a destination of another host", mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN"}), "binds dispatch.test, which no entry of BROKER_PROXY_PRIVATE_UPSTREAM names", []string{"BROKER_PROXY_PRIVATE_UPSTREAM=other.test=http://10.0.0.5:8766"}},
		{"a second destination no dpop block binds", mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN"}), "names the host results.test", []string{mintedDestination + " results.test=http://10.0.0.6:8766"}},
		{"a kind over the host", withKinds(mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN"}), wildcard), "the kind wildcard of BROKER_PROFILES_FILE binds dispatch.test", []string{mintedDestination}},
		{"a destination no dpop block binds", `{"kinds": [{"name": "example", "hosts": ["api.example.com"], "ways": ["swap"]}], "profiles": []}`, "names the host dispatch.test", []string{mintedDestination}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary, append(append(airlockEnv(t),
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_PROFILES_FILE="+profilesFile(t, c.profiles),
			), c.upstream...)...)
			if !strings.Contains(out, c.says) {
				t.Fatalf("the broker with %s was refused, but its output does not name %q: %s", c.name, c.says, out)
			}
		})
	}
}

// TestTheBootTakesDPoPBlocksOfTheDestinationsHost is the control of the rows above: two dpop blocks
// of the private destination's one host start, and the broker logs its airlock's DPoP key by its
// thumbprint alone and goes on to serve.
func TestTheBootTakesDPoPBlocksOfTheDestinationsHost(t *testing.T) {
	memorySecretsFile := filepath.Join(t.TempDir(), "memory-secrets.json")
	if err := os.WriteFile(memorySecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: append(airlockEnv(t),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE="+memorySecretsFile,
		"BROKER_PROFILES_FILE="+profilesFile(t, mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN", "SECOND_TOKEN"})),
		mintedDestination,
	)})
	requireHealthy(t, broker.Addr)
	record := broker.AwaitRecord(t, dpopKeyLine, nil)
	if thumbprint, _ := record["thumbprint"].(string); len(thumbprint) != 43 {
		t.Fatalf("the DPoP key line %v names no RFC 7638 thumbprint (43 base64url characters)", record)
	}
	for field := range record {
		if field == "d" || field == "key" || field == "private_key" {
			t.Fatalf("the DPoP key line %v writes the key's private part", record)
		}
	}
}
