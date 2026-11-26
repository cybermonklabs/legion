package config

import (
	"strings"
	"testing"
)

// admission_p0_bound is how many P0 issues may hold an admission slot at once, outside the cap (C3
// of dispatch://INFRA-5636): a file that leaves it out gives no bound, a positive integer is that
// bound, and anything less than 1, or no integer at all, is refused naming the key. A bound of 0
// is no way to stop every P0 start; a project pause is (decision 5 of the plan). That the bound
// the configuration carries reaches admission at boot is the booted daemon's to show (check
// c-bound-boot).
func TestAdmissionP0BoundReadsAPositiveIntegerAndRefusesLess(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AdmissionP0Bound != nil {
			t.Fatalf("AdmissionP0Bound = %d with the key left out, want no bound", *cfg.AdmissionP0Bound)
		}
	})
	t.Run("2", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, minimalFile+"admission_p0_bound: 2\n"), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AdmissionP0Bound == nil || *cfg.AdmissionP0Bound != 2 {
			t.Fatalf("AdmissionP0Bound = %v, want 2", cfg.AdmissionP0Bound)
		}
	})
	for _, value := range []string{"0", "-1", "two"} {
		t.Run(value, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+"admission_p0_bound: "+value+"\n"), noEnv)
			if err == nil {
				t.Fatalf("Load took admission_p0_bound: %s, want a refusal naming the key", value)
			}
			if !strings.Contains(err.Error(), "admission_p0_bound") {
				t.Fatalf("Load error = %q, want it to name admission_p0_bound", err.Error())
			}
		})
	}
}
