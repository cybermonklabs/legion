package slot

import (
	"fmt"
	"strings"
	"testing"
)

// TestTheSlotNamesAreTheWireNames pins each slot to the string the broker's release answer and
// the airlock_placeholders table's slot column hold: a renamed constant is a changed wire.
func TestTheSlotNamesAreTheWireNames(t *testing.T) {
	for _, row := range []struct{ name, got, want string }{
		{"Value", Value, "value"},
		{"AWSAccessKeyID", AWSAccessKeyID, "aws-access-key-id"},
		{"AWSSecretAccessKey", AWSSecretAccessKey, "aw" + "s-secret-access-key"},
		{"AWSSessionToken", AWSSessionToken, "aws-session-token"},
		{"GitHubPrefix", GitHubPrefix, "github:"},
		{"AzureFederated", AzureFederated, "azure-federated"},
		{"AzureAccessPrefix", AzureAccessPrefix, "azure-access:"},
	} {
		if row.got != row.want {
			t.Errorf("%s = %q, want %q", row.name, row.got, row.want)
		}
	}
}

// envSetFaults names every way sets break what agent-secrets' walk over them rests on: a set
// with no Required slot, which every name would fill, and a slot or a variable that two entries
// name, where one entry would silently overwrite the other.
func envSetFaults(sets []EnvSet) []string {
	var faults []string
	slots, names := map[string]string{}, map[string]string{}
	for _, set := range sets {
		if len(set.Required) == 0 {
			faults = append(faults, fmt.Sprintf("%q requires no slot", set.What))
		}
		for _, v := range append(append([]Var{}, set.Required...), set.Optional...) {
			if other, seen := slots[v.Slot]; seen {
				faults = append(faults, fmt.Sprintf("the slot %q is named by %q and by %q", v.Slot, other, set.What))
			}
			if other, seen := names[v.Name]; seen {
				faults = append(faults, fmt.Sprintf("the variable %s is named by %q and by %q", v.Name, other, set.What))
			}
			slots[v.Slot], names[v.Name] = set.What, set.What
		}
	}
	return faults
}

// TestEveryEnvSetNamesItsSlotsOnce holds EnvSets to envSetFaults, with a control row of sets the
// check must refuse, so a check that can see nothing does not pass.
func TestEveryEnvSetNamesItsSlotsOnce(t *testing.T) {
	if len(EnvSets) == 0 {
		t.Fatal("EnvSets is empty, want the AWS key pair's set")
	}
	if faults := envSetFaults(EnvSets); len(faults) > 0 {
		t.Errorf("EnvSets: %s", strings.Join(faults, "; "))
	}

	repeated := append(append([]EnvSet{}, EnvSets...), EnvSet{
		What:     "a second key pair",
		Required: []Var{{"other-key-id", "AWS_ACCESS_KEY_ID"}},
	}, EnvSet{What: "a set of nothing"})
	faults := strings.Join(envSetFaults(repeated), "; ")
	for _, want := range []string{"the variable AWS_ACCESS_KEY_ID is named by", `"a set of nothing" requires no slot`} {
		if !strings.Contains(faults, want) {
			t.Errorf("the control's faults %q do not hold %q", faults, want)
		}
	}
}
