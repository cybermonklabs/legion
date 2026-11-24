package cipherclient

import (
	"strings"
	"testing"
)

// TestMetadataDescribeNeverPrintsTheAudiencesCredential: AGENT_SECRETS_MACHINE_AUDIENCE is a
// configured value, usually the broker's URL. Describe is in every trade's log line
// (Client.logf, "identity") and every trade error ("trade %s at %s"), so an audience carrying a
// credential, or one that is no URL, prints by its setting alone, as ProjectedToken.Describe
// names AGENT_SECRETS_MACHINE_TOKEN_FILE. Control: a plain audience prints whole.
func TestMetadataDescribeNeverPrintsTheAudiencesCredential(t *testing.T) {
	const secret = "hunter2audpw7c1"
	for _, audience := range []string{
		"https://aud:" + secret + "@broker.example",
		"https://" + secret + "@broker.example",
		"https://broker.example/?k=" + secret,
		secret,
	} {
		described := NewMetadataIdentity(audience).Describe()
		if strings.Contains(described, secret) {
			t.Errorf("Describe for audience %q = %q: it prints the credential", audience, described)
		}
		if !strings.Contains(described, MachineAudienceVar) {
			t.Errorf("Describe for audience %q = %q: it does not name %s", audience, described, MachineAudienceVar)
		}
	}
	if described := NewMetadataIdentity("https://broker.example").Describe(); !strings.Contains(described, "https://broker.example") {
		t.Errorf("control: Describe for a plain audience = %q, want it to print the audience whole", described)
	}
}

// TestARefusedPersonIsNamedAndNotEchoed: an AGENT_SECRETS_PERSON that names nobody signed in is a
// refused value from the environment, so the refusal names the variable and the people signed in,
// not the value. Control: a person who is signed in is chosen.
func TestARefusedPersonIsNamedAndNotEchoed(t *testing.T) {
	store := storeFor(t)
	if err := store.Write(signedIn(t, "ada@example.com")); err != nil {
		t.Fatal(err)
	}
	_, err := store.Choose("hunter2pwPERSON")
	if err == nil {
		t.Fatal("Choose took a person who is not signed in")
	}
	if text := strings.ToLower(err.Error()); strings.Contains(text, "hunter2pwperson") || !strings.Contains(err.Error(), PersonVar) {
		t.Errorf("Choose's error = %q, want one naming %s and not the value", err, PersonVar)
	}
	if record, err := store.Choose("ADA@example.com"); err != nil || record.Person != "ada@example.com" {
		t.Errorf("Choose(ada) = %+v, %v; want ada's sign-in", record, err)
	}
}
