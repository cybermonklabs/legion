// packages/envoy/internal/cipherclient/machine_identities_test.go
//
// The Go half of the one list of machine identities: testdata/machine-identities.json names each
// kind AGENT_SECRETS_MACHINE_IDENTITY takes and the variables it needs, in the order a client reads
// them, and packages/cipher-client's suite holds the TypeScript client to the same file.

package cipherclient

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// machineIdentities is testdata/machine-identities.json.
type machineIdentities struct {
	Identities []struct {
		Kind      string   `json:"kind"`
		Clients   []string `json:"clients"`
		Variables []struct {
			Name    string `json:"name"`
			Example string `json:"example"`
		} `json:"variables"`
	} `json:"identities"`
}

// TestEveryMachineIdentityReadsTheCorpusVariablesInOrder: for each kind the corpus says this
// client takes, MachineIdentity given none of its variables refuses the first, given that one
// refuses the next, and given all of them builds the identity; and the refusal of a kind it does
// not know lists exactly the corpus's kinds.
func TestEveryMachineIdentityReadsTheCorpusVariablesInOrder(t *testing.T) {
	raw, err := os.ReadFile("testdata/machine-identities.json")
	if err != nil {
		t.Fatalf("read the machine identity corpus: %v", err)
	}
	var corpus machineIdentities
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("read the machine identity corpus: %v", err)
	}
	var kinds []string
	for _, identity := range corpus.Identities {
		if !slices.Contains(identity.Clients, "go") {
			continue
		}
		kinds = append(kinds, identity.Kind)
		t.Run(identity.Kind, func(t *testing.T) {
			env := map[string]string{IdentityVar: identity.Kind}
			getenv := func(name string) string { return env[name] }
			for _, variable := range identity.Variables {
				want := variable.Name + " is required by " + IdentityVar + "=" + identity.Kind
				if source, err := MachineIdentity(getenv); err == nil || err.Error() != want {
					t.Fatalf("MachineIdentity with %v = %v, %v; want %q", env, source, err, want)
				}
				env[variable.Name] = variable.Example
			}
			if source, err := MachineIdentity(getenv); err != nil || source == nil {
				t.Fatalf("MachineIdentity with %v = %v, %v; want the identity built", env, source, err)
			}
		})
	}
	if len(kinds) == 0 {
		t.Fatal("the machine identity corpus names no kind this client takes")
	}
	listed := strings.Join(kinds[:len(kinds)-1], ", ") + " or " + kinds[len(kinds)-1]
	want := IdentityVar + " names no identity this machine can prove; it takes " + listed
	if _, err := MachineIdentity(func(name string) string { return map[string]string{IdentityVar: "no-such-kind"}[name] }); err == nil || err.Error() != want {
		t.Errorf("MachineIdentity of a kind it does not know = %v; want %q", err, want)
	}
}
