package config

import (
	"testing"
	"time"
)

// pod_update_grace_minutes is read as worker_silence_minutes is: a positive number of minutes, a
// decimal among them, so a proof can drive an update round's bound inside one run; thirty minutes
// when the file names none. A key with no value leaves that default standing.
func TestPodUpdateGraceMinutesTakesAPositiveDecimalAndDefaultsToHalfAnHour(t *testing.T) {
	for _, file := range []string{minimalFile, minimalFile + "pod_update_grace_minutes:\n"} {
		cfg, err := LoadForValidation(writeConfigFile(t, file), noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation: %v", err)
		}
		if cfg.PodUpdateGrace != 30*time.Minute {
			t.Errorf("PodUpdateGrace = %s, want the thirty-minute default", cfg.PodUpdateGrace)
		}
	}
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0.5", 30 * time.Second},
		{"1.5", 90 * time.Second},
		{"45", 45 * time.Minute},
		{"35791", 35791 * time.Minute},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"pod_update_grace_minutes: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.PodUpdateGrace != tc.want {
				t.Errorf("PodUpdateGrace = %s, want %s", cfg.PodUpdateGrace, tc.want)
			}
			if cfg.WorkerSilence != 30*time.Minute {
				t.Errorf("WorkerSilence = %s, want its own default untouched by pod_update_grace_minutes", cfg.WorkerSilence)
			}
		})
	}
}

// A bound of zero or less would replace a pod at the tick that opens its round, before any role
// read the ask, so zero and a negative are refused, as are a value past the timer bound in whole
// minutes and one that is no number.
func TestPodUpdateGraceMinutesRefuses(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"0", "pod_update_grace_minutes must be a positive number"},
		{"-0.5", "pod_update_grace_minutes must be a positive number"},
		{"0.0000000000001", "pod_update_grace_minutes must be a positive number"},
		{"35791.5", "pod_update_grace_minutes must be at most 35791"},
		{"soon", "pod_update_grace_minutes must be a number"},
		{"\"30\"", "pod_update_grace_minutes must be a number"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"pod_update_grace_minutes: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
