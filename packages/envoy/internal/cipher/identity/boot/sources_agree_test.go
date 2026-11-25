package boot_test

//ci:reads packages/cipher-client/scripts/print-identity-sources.ts TestClientSourcesAgree runs `bun ../../../../../cipher-client/scripts/print-identity-sources.ts`
//ci:reads packages/cipher-client/src/** print-identity-sources.ts imports ../src/environment, whose own imports the run evaluates

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
	"github.com/sjawhar/envoy/internal/cipher/identity/providers"
)

// clientSource is one entry of the TypeScript client's identity registry, as
// packages/cipher-client/scripts/print-identity-sources.ts prints it: the name
// AGENT_SECRETS_MACHINE_IDENTITY takes and the broker provider whose tokens it makes, null for
// the device login.
type clientSource struct {
	Name     string  `json:"name"`
	Provider *string `json:"provider"`
}

// TestClientSourcesAgree: the Go client's identity registry (identity/providers), the TypeScript
// client's (packages/cipher-client/src/environment.ts, read by running the script that prints it,
// so there is no second copy to go stale) and the broker's own providers (identity/boot) name the
// same kinds. Every identity the Go client trades the TypeScript client trades too; the one it
// adds is the device login, whose broker access token no identity provider verifies; and every
// provider either names is one this build of the broker holds.
//
// It runs Bun, which the envoy-go job installs. Without Bun on PATH it skips when CI is unset and
// fails when it is set: in CI a missing Bun is a job that stopped checking this, not a pass.
func TestClientSourcesAgree(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("bun is not on PATH, and this check reads the TypeScript registry by running it: %v", err)
		}
		t.Skipf("bun is not on PATH: %v", err)
	}
	script := filepath.Join("..", "..", "..", "..", "..", "cipher-client", "scripts", "print-identity-sources.ts")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the TypeScript registry's printer is missing: %v", err)
	}
	out, err := exec.Command(bun, script).Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("run %s: %v\n%s", script, err, stderr)
	}
	var client []clientSource
	if err := json.Unmarshal(out, &client); err != nil {
		t.Fatalf("read the TypeScript registry %q: %v", out, err)
	}
	if len(client) == 0 {
		t.Fatal("the TypeScript registry is empty, so this check compared nothing")
	}
	goSources := providers.Sources()
	if len(goSources) == 0 {
		t.Fatal("the Go registry is empty, so this check compared nothing")
	}
	kinds := boot.Kinds()
	if len(kinds) == 0 {
		t.Fatal("the broker's registry holds no provider, so this check compared nothing")
	}

	clientByName := map[string]clientSource{}
	var clientNames []string
	for _, source := range client {
		clientByName[source.Name] = source
		clientNames = append(clientNames, source.Name)
	}
	var goNames []string
	for _, source := range goSources {
		goNames = append(goNames, source.Name)
		other, held := clientByName[source.Name]
		if !held {
			t.Errorf("the Go client trades %s and the TypeScript client does not", source.Name)
			continue
		}
		if other.Provider == nil || identity.Kind(*other.Provider) != source.Provider {
			t.Errorf("%s is the %s provider's in Go and %v's in TypeScript", source.Name, source.Provider, other.Provider)
		}
	}
	var only []string
	for _, name := range clientNames {
		if !slices.Contains(goNames, name) {
			only = append(only, name)
		}
	}
	if want := []string{"device-login"}; !slices.Equal(only, want) {
		t.Errorf("the TypeScript client trades %v that the Go client does not, want %v", only, want)
	}
	for _, source := range client {
		switch {
		case source.Provider == nil:
			if source.Name != "device-login" {
				t.Errorf("%s names no provider; only the device login, whose token no provider verifies, may", source.Name)
			}
		case !slices.Contains(kinds, identity.Kind(*source.Provider)):
			t.Errorf("the TypeScript client names the provider %s, which this build of the broker does not hold (%v)", *source.Provider, kinds)
		}
	}
	for _, source := range goSources {
		if !slices.Contains(kinds, source.Provider) {
			t.Errorf("the Go client names the provider %s, which this build of the broker does not hold (%v)", source.Provider, kinds)
		}
	}
}
