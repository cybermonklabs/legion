package config

import (
	"strings"
	"testing"
)

// wantLingerHoursWarning is the one line a file that sets linger_hours is warned with: no tree
// lingers any more (R20 of dispatch://INFRA-3320), so the key changes nothing and is read for
// nothing (decision 18 of dispatch://INFRA-4730).
const wantLingerHoursWarning = "setting linger_hours changes nothing: no tree lingers: an issue Dispatch shows out of work stops at once, keeping its phase, sessions and pull request until it returns or closes, so no deadline is armed for anything; remove linger_hours from legion.yaml (R20 of dispatch://INFRA-3320)"

// A file that still sets linger_hours, as every file the release before shipped did, loads with
// the one warning and every other key it sets intact. Whatever value it carries — the shipped
// default, a decimal, one the old bound refused, no number at all, null — passes, since no value is
// read.
func TestLingerHoursLoadsWithItsWarningAndChangesNothing(t *testing.T) {
	for _, value := range []string{"72", "0.3", "597", "soon", "null"} {
		t.Run(value, func(t *testing.T) {
			body := minimalFile + "production_check_hours: 48\nreview_round_cap: 7\nlinger_hours: " + value + "\n"

			cfg, err := Load(writeConfigFile(t, body), noEnv)
			if err != nil {
				t.Fatalf("Load: %v, want linger_hours accepted with a warning", err)
			}
			if len(cfg.Warnings) != 1 || cfg.Warnings[0] != wantLingerHoursWarning {
				t.Fatalf("Load warned %q, want the one line %q", cfg.Warnings, wantLingerHoursWarning)
			}
			if cfg.ProductionCheckBound.Hours() != 48 || cfg.ReviewRoundCap != 7 || cfg.Project != "DEMO" {
				t.Errorf("production_check_hours = %s, review_round_cap = %d, project = %q; want 48h, 7 and DEMO",
					cfg.ProductionCheckBound, cfg.ReviewRoundCap, cfg.Project)
			}
		})
	}
}

// A file that names no linger_hours is warned nothing about it.
func TestAFileWithoutLingerHoursIsWarnedNothing(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, line := range cfg.Warnings {
		if strings.Contains(line, "linger_hours") {
			t.Errorf("Load warned %q for a file that does not set linger_hours", line)
		}
	}
}
