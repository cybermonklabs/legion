package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// TestEachStoreTakesTheTagValuesItCan: AWS Secrets Manager takes a tag value of at most 256
// characters, of Unicode letters, digits, spaces and `_ . : / = + - @` alone, and refuses any
// other; Secret Manager's annotations take any value. So on AWS a guidance with a comma or an
// apostrophe, or one longer than 256 characters, is refused before anything is written, naming
// the store's limit and, for a character, the one that fails, and the same values write on GCP.
// A guidance in the set writes on both, and so do the two values with a ':' this client already
// writes on AWS: a group owner, and the requesters rollback guard a project tag brings. moto and
// the stand-in stores enforce none of AWS's rule, so the client's own check is what is tested.
func TestEachStoreTakesTheTagValuesItCan(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv in secret_test.go); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify in secret_gcp.go); it sets a package variable the package's other tests read (main.onEachStore, main.useAWS, awsClients set in secret_test.go).
	long := strings.Repeat("g", 257)
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		aws := strings.HasSuffix(t.Name(), "/aws")
		for _, c := range []struct {
			name, guidance, refusal string
		}{
			{"a comma", "grant for the release job, nothing else", `"," at character 26`},
			{"an apostrophe", "grant for the job's own run", `"'" at character 18`},
			{"257 characters", long, "the guidance tag is 257 characters, and AWS Secrets Manager holds at most 256"},
		} {
			store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman}, "v1")
			before := store.calls()
			_, stderr, code := runSecret("retag", "API_KEY", "--judge", "300", "--guidance", c.guidance)
			tags := store.tags(t, "API_KEY")
			if aws {
				if code != exitUsageError || !strings.Contains(stderr, c.refusal) || !strings.Contains(stderr, "AWS Secrets Manager") {
					t.Errorf("AWS, guidance with %s: exit %d stderr %q; want %d naming %s and the store", c.name, code, stderr, exitUsageError, c.refusal)
				}
				if c.name != "257 characters" && !strings.Contains(stderr, "_ . : / = + - @") {
					t.Errorf("AWS, guidance with %s: stderr %q names no allowed set", c.name, stderr)
				}
				if _, written := tags[policy.TagGuidance]; written || tags[policy.TagJudge] != "" {
					t.Errorf("AWS, guidance with %s: the store holds %v after the refusal; want nothing written", c.name, tags)
				}
				if strings.Contains(stderr, c.guidance) {
					t.Errorf("AWS, guidance with %s: stderr %q prints the value", c.name, stderr)
				}
				continue
			}
			if code != 0 || tags[policy.TagGuidance] != c.guidance || tags[policy.TagJudge] != "300" {
				t.Errorf("GCP, guidance with %s: exit %d stderr %q, tags %v; want it written (%d store calls before)", c.name, code, stderr, tags, before)
			}
		}

		inSet := "grant for the release job when the reason names k14-grant: run 3/4 = ok + done @ 12.00"
		store.put(t, "SET_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman}, "v1")
		if _, stderr, code := runSecret("retag", "SET_KEY", "--judge", "300", "--guidance", inSet); code != 0 || store.tags(t, "SET_KEY")[policy.TagGuidance] != inSet {
			t.Errorf("guidance in AWS's set: exit %d stderr %q, tags %v; want it written", code, stderr, store.tags(t, "SET_KEY"))
		}

		store.put(t, "GROUP_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman}, "v1")
		if _, stderr, code := runSecret("retag", "GROUP_KEY", "--owner", "group:team@example.com"); code != 0 || store.tags(t, "GROUP_KEY")[policy.TagOwner] != "group:team@example.com" {
			t.Errorf("--owner group:team@example.com: exit %d stderr %q, tags %v; want the group owner written", code, stderr, store.tags(t, "GROUP_KEY"))
		}

		store.put(t, "PROJECT_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, "v1")
		runSecret("retag", "PROJECT_KEY", "--project", "PRIV")
		if got := store.tags(t, "PROJECT_KEY")[policy.TagRequesters]; !strings.Contains(got, policy.RequestersRollbackGuard) {
			t.Errorf("--project PRIV: the requesters tag is %q; want the rollback guard %s written", got, policy.RequestersRollbackGuard)
		}
	})
}
