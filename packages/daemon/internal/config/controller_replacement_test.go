package config

import (
	"testing"
	"time"
)

// The config half of dispatch://INFRA-2045 R2: `controller_turn_bound_minutes` is the bound a turn
// of the daemon's own controller may stay open for before its keeper ends the launch. A file that
// sets no key, or sets it with no value, reads 90 minutes; 0 turns the rule off; a decimal is read
// as written; anything else is refused, each refusal saying that 0 is the rule off. The keeper's
// half, the end itself, is internal/supervise's and internal/daemon's test of the same name.
func TestAControllerTurnOpenPastItsBoundIsEnded(t *testing.T) {
	t.Run("e: a file that sets no key reads 90", func(t *testing.T) {
		for _, file := range []string{minimalFile, minimalFile + "controller_turn_bound_minutes:\n"} {
			cfg, err := Load(writeConfigFile(t, file), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ControllerTurnBound != 90*time.Minute {
				t.Errorf("ControllerTurnBound = %s, want 1h30m0s", cfg.ControllerTurnBound)
			}
		}
	})
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"0", 0}, {"120", 2 * time.Hour}, {"45.5", 45*time.Minute + 30*time.Second}} {
		t.Run("the file sets "+tc.value, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, minimalFile+"controller_turn_bound_minutes: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ControllerTurnBound != tc.want {
				t.Errorf("ControllerTurnBound = %s, want %s", cfg.ControllerTurnBound, tc.want)
			}
		})
	}
	for _, tc := range []struct{ value, want string }{
		{"-1", "controller_turn_bound_minutes must be a positive number, or 0 to turn the rule off"},
		{"soon", "controller_turn_bound_minutes must be a number, or 0 to turn the rule off"},
		{"35791.5", "controller_turn_bound_minutes must be at most 35791, or 0 to turn the rule off"},
	} {
		t.Run("the file sets "+tc.value, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+"controller_turn_bound_minutes: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The config half of dispatch://INFRA-2045 R3: `controller_session_max_age_hours` is the age past
// which a relaunch the daemon makes of its controller starts a fresh session. A file that sets no
// key, or sets it with no value, reads 72 hours; 0 turns the rule off; a decimal is read as
// written; anything else is refused, each refusal saying that 0 is the rule off. The replacement
// itself is internal/daemon's test of the same name.
func TestAnOldControllerSessionIsReplacedAtADaemonDrivenRelaunch(t *testing.T) {
	t.Run("f: a file that sets no key reads 72", func(t *testing.T) {
		for _, file := range []string{minimalFile, minimalFile + "controller_session_max_age_hours:\n"} {
			cfg, err := Load(writeConfigFile(t, file), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ControllerSessionMaxAge != 72*time.Hour {
				t.Errorf("ControllerSessionMaxAge = %s, want 72h0m0s", cfg.ControllerSessionMaxAge)
			}
		}
	})
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"0", 0}, {"48", 48 * time.Hour}, {"0.5", 30 * time.Minute}} {
		t.Run("the file sets "+tc.value, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, minimalFile+"controller_session_max_age_hours: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ControllerSessionMaxAge != tc.want {
				t.Errorf("ControllerSessionMaxAge = %s, want %s", cfg.ControllerSessionMaxAge, tc.want)
			}
		})
	}
	for _, tc := range []struct{ value, want string }{
		{"-72", "controller_session_max_age_hours must be a positive number, or 0 to turn the rule off"},
		{"\"72\"", "controller_session_max_age_hours must be a number, or 0 to turn the rule off"},
		{"597", "controller_session_max_age_hours must be at most 596, or 0 to turn the rule off"},
	} {
		t.Run("the file sets "+tc.value, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+"controller_session_max_age_hours: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}
