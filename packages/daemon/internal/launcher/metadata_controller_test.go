package launcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This test reads Config.servedMetadata, which the trees the redaction check's launcher leak rows
// apply their tests to do not declare; it lives apart from metadata_test.go, which those rows copy
// onto those trees, so the rows still compile there.

// The controller's launcher, the single-role form, in a microVM pod whose deployment names the
// endpoint, serves it as an issue pod's launcher does: it listens before the role's first
// connection, and so before its first generation, names the address it bound in its own
// environment, exchanges what the token file holds for a token of the pool it was named, and
// tells each generation that address in both variables, over whatever the daemon's start command
// carried. Where no test names a token file, it reads the one the daemon mounts in the
// controller's pod, under FixedRoot, since that pod has no roles directory; an issue pod's
// launcher reads its own under its roles directory. Both paths are read from the configuration the
// launcher would serve (Config.servedMetadata) without listening: that one listens on
// MetadataAddress, which no test holds.
func TestTheControllersLauncherServesTheMetadataEndpoint(t *testing.T) {
	named := func() *MetadataConfig {
		return &MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider}
	}
	controller := Config{Token: "launcher-token", Role: "controller", PrivateDir: "/private", MicroVM: true, Metadata: named()}
	if got := controller.servedMetadata(); got.tokenFile != "/var/run/legion/workload-identity/token" || got.listenAddress() != MetadataAddress {
		t.Errorf("the controller's endpoint reads the token %q and listens at %q, want /var/run/legion/workload-identity/token and %q",
			got.tokenFile, got.listenAddress(), MetadataAddress)
	}
	issuePod := Config{RolesDir: "/roles", Roles: []string{"tester"}, MicroVM: true, Metadata: named()}
	if got := issuePod.servedMetadata().tokenFile; got != "/roles/workload-identity/token" {
		t.Errorf("an issue pod's endpoint reads the token %q, want /roles/workload-identity/token", got)
	}

	holdMetadataVariables(t, "")
	tokenFile := filepath.Join(t.TempDir(), WorkloadIdentityTokenFile)
	writeSubjectToken(t, tokenFile, "fake-subject-token-1")
	sts := &fakeSTS{t: t, tokenFile: tokenFile, expiresIn: 3600}
	service := httptest.NewServer(sts)
	t.Cleanup(service.Close)
	metadata := named()
	metadata.address, metadata.tokenFile, metadata.tokenURL = "127.0.0.1:0", tokenFile, service.URL
	// The rig has taken the launcher's first connection, and no generation has started.
	g := newSessionRig(t, Config{StopGrace: 2 * time.Second, MicroVM: true, Metadata: metadata})
	address := servedMetadataAddress(t)
	if got := os.Getenv(MetadataIPVariable); got != address {
		t.Errorf("the launcher's own %s=%q once its role connected, want %q as %s", MetadataIPVariable, got, address, MetadataHostVariable)
	}
	if strings.Contains(g.stderr.String(), "launcher: metadata: ") {
		t.Errorf("the launcher logged a metadata line, want none: %s", g.stderr.String())
	}
	response, body := getMetadata(t, "http://"+address, tokenPath, flavor)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a token request at %s answered %d %q, want 200", address, response.StatusCode, redacted(body))
	}
	var token metadataToken
	if err := json.Unmarshal([]byte(body), &token); err != nil {
		t.Fatalf("a token request answered no token: %v", err)
	}
	if exchanges := sts.answered(); len(exchanges) != 1 || exchanges[0].subject != "fake-subject-token-1" || exchanges[0].token != token.AccessToken {
		t.Fatalf("the endpoint made %d exchanges, want one of the token file's contents whose token it answered", len(exchanges))
	}
	const expansion = "${" + MetadataHostVariable + "-unset}|${" + MetadataIPVariable + "-unset}"
	want := address + "|" + address
	carried := []string{MetadataHostVariable + "=169.254.169.254", MetadataIPVariable + "=169.254.169.254"}
	if got := g.told(1, expansion, carried, ""); got != want {
		t.Errorf("a generation whose start command carried the node's address ran with %q, want %q", got, want)
	}
	if got := g.told(2, expansion, nil, ""); got != want {
		t.Errorf("a generation whose start command named no address ran with %q, want %q", got, want)
	}
}
