package google

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// TestAMetadataIdentityNamesOnlyTheSettingsThatChoseIt: Describe names AGENT_SECRETS_MACHINE_AUDIENCE
// only for an identity that setting chose, and GCE_METADATA_HOST only while it is set. One that
// DetectMachineIdentity found with no setting mints for the Google exchange audience the broker
// names, and a Compute Engine VM's metadata server is at its own address. Controls: the setting
// that chose the identity, and a GCE_METADATA_HOST that is set, are named.
func TestAMetadataIdentityNamesOnlyTheSettingsThatChoseIt(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("GCE_METADATA_HOST", "")
	detected := (&MetadataIdentity{Audience: "agent-secrets-vm", FromBroker: true}).Describe()
	if want := "this machine's Google identity token for the Google exchange audience the broker names"; detected != want {
		t.Errorf("Describe of a detected identity = %q, want %q", detected, want)
	}
	if configured := NewMetadataIdentity("agent-secrets-vm").Describe(); !strings.Contains(configured, identity.MachineAudienceVar) || strings.Contains(configured, "GCE_METADATA_HOST") {
		t.Errorf("control: Describe of a configured identity = %q, want it to name %s and no unset GCE_METADATA_HOST", configured, identity.MachineAudienceVar)
	}

	startDetectMetadata(t)
	source, _, err := DetectMachineIdentity(context.Background(), "agent-secrets-vm", nil)
	if err != nil {
		t.Fatal(err)
	}
	described := source.Describe()
	if strings.Contains(described, identity.MachineAudienceVar) || !strings.Contains(described, "the Google exchange audience the broker names (GCE_METADATA_HOST names the metadata server)") {
		t.Errorf("Describe of the identity DetectMachineIdentity found = %q, want the broker's audience and the GCE_METADATA_HOST that is set", described)
	}
}
