package policy_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// TestTypeTagWinsAndOldTagLogsOnce: type wins over the old kind tag, an empty type wins too, and
// an invalid type does not fall back; a secret read through the old tag alone writes
// LegacyTypeTagMessage once across two loads and two rereads. The ids logged are forgotten
// first and after, so the count is this run's whatever ran before it in the process (-count=2).
func TestTypeTagWinsAndOldTagLogsOnce(t *testing.T) {
	policy.ForgetLegacyTypeTagLines()
	t.Cleanup(policy.ForgetLegacyTypeTagLines)
	logged := policytest.CaptureLog(t)
	for _, row := range []struct {
		name    string
		tags    map[string]string
		want    string
		refused bool
		old     bool
	}{
		{"TYPE_ONLY_NOT_A_SECRET", map[string]string{"type": "google-login"}, "google-login", false, false},
		{"OLD_TYPE_NOT_A_SECRET", map[string]string{"kind": "google-login"}, "google-login", false, true},
		{"BOTH_TYPES_NOT_A_SECRET", map[string]string{"type": "google-login", "kind": "not-a-secret-unknown"}, "google-login", false, false},
		{"EMPTY_TYPE_NOT_A_SECRET", map[string]string{"type": "", "kind": "google-login"}, "", false, false},
		{"UNKNOWN_TYPE_NOT_A_SECRET", map[string]string{"type": "not-a-secret-unknown", "kind": "google-login"}, "", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			secret := policytest.Secret(row.name, "ada@example.com", policy.TierAgent, "not-a-secret")
			for key, value := range row.tags {
				secret.Tags[key] = value
			}
			loader := policytest.Loader(memory.New(secret))
			for range 2 {
				set, err := loader.Load(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				got, served := set.Secrets[row.name]
				if served == row.refused || (!row.refused && got.Type() != row.want) {
					t.Errorf("Load: served=%t type=%q refused=%v; want type=%q refused=%t", served, got.Type(), set.Refused, row.want, row.refused)
				}
				one, err := loader.LoadOne(t.Context(), row.name)
				if err != nil {
					t.Fatal(err)
				}
				if one.Served == row.refused || (!row.refused && one.Secret.Type() != row.want) {
					t.Errorf("LoadOne=%+v; want type=%q refused=%t", one, row.want, row.refused)
				}
			}
			wantLogs := 0
			if row.old {
				wantLogs = 1
			}
			if got := strings.Count(logged.String(), "name="+policytest.ID(row.name)); got != wantLogs {
				t.Errorf("old-name warnings for %s = %d, want %d: %s", row.name, got, wantLogs, logged)
			}
			if row.old && !strings.Contains(logged.String(), policy.LegacyTypeTagMessage) {
				t.Errorf("the old-name warning for %s is not %q: %s", row.name, policy.LegacyTypeTagMessage, logged)
			}
		})
	}
}
