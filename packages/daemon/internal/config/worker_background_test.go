package config

import (
	"testing"
	"time"
)

// `worker_background_hours` is the bound the daemon reads a worker's own report of background
// work against before it names that worker to its architect, and four hours is what a file that
// names the key nowhere runs on.
func TestWorkerBackgroundDefaultsToFourHours(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.WorkerBackground != 4*time.Hour {
		t.Errorf("WorkerBackground = %s, want the four-hour default", cfg.WorkerBackground)
	}
	if cfg.WorkerBackground <= cfg.WorkerSilence {
		t.Errorf("the default %s is at or under the silence default %s, so no file that names neither key could pass the check",
			cfg.WorkerBackground, cfg.WorkerSilence)
	}
}

// A value above the silence bound is read as written, a decimal among them: the floor is a
// relation to `worker_silence_minutes` and not a number of this key's own, so a deployment that
// shortens one may shorten the other with it.
func TestWorkerBackgroundTakesEveryValueAboveTheSilenceBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		want  time.Duration
		other time.Duration
	}{
		{name: "a whole number of hours", file: "worker_background_hours: 6\n", want: 6 * time.Hour, other: 30 * time.Minute},
		{name: "a decimal", file: "worker_background_hours: 1.5\n", want: 90 * time.Minute, other: 30 * time.Minute},
		{
			name: "the first value above a shortened silence bound",
			file: "worker_silence_minutes: 20\nworker_background_hours: 0.35\n",
			want: 21 * time.Minute, other: 20 * time.Minute,
		},
		{name: "the timer bound in whole hours", file: "worker_background_hours: 596\n", want: 596 * time.Hour, other: 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.file), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.WorkerBackground != tc.want || cfg.WorkerSilence != tc.other {
				t.Errorf("WorkerBackground = %s with WorkerSilence %s, want %s and %s",
					cfg.WorkerBackground, cfg.WorkerSilence, tc.want, tc.other)
			}
		})
	}
}

// A value at the silence bound and one under it are both refused, and each refusal names both
// keys and both values: the floor is a relation, so which of the two the operator moves is
// theirs to choose and neither number means anything without the other.
func TestWorkerBackgroundRefusesAValueAtOrUnderTheSilenceBound(t *testing.T) {
	for _, tc := range []struct{ name, file, want string }{
		{
			name: "at the silence bound",
			file: "worker_background_hours: 0.5\n",
			want: "worker_background_hours (30m0s) is at or below worker_silence_minutes (30m0s): the bound on background work a worker reports must be above the bound on its silence, since a shorter one would name a job that has barely begun and would race the repair that sends the same worker its phase's task again",
		},
		{
			name: "under the silence bound",
			file: "worker_background_hours: 0.25\n",
			want: "worker_background_hours (15m0s) is at or below worker_silence_minutes (30m0s): the bound on background work a worker reports must be above the bound on its silence, since a shorter one would name a job that has barely begun and would race the repair that sends the same worker its phase's task again",
		},
		{
			name: "under a silence bound the same file raised",
			file: "worker_silence_minutes: 90\nworker_background_hours: 1\n",
			want: "worker_background_hours (1h0m0s) is at or below worker_silence_minutes (1h30m0s): the bound on background work a worker reports must be above the bound on its silence, since a shorter one would name a job that has barely begun and would race the repair that sends the same worker its phase's task again",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.file), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

// readHours' own refusals, which run before the relation above is reckoned at all: a value that
// is no number, one that is not positive, and one past the timer bound in whole hours.
func TestWorkerBackgroundRefusesAValueThatIsNoPositiveBoundedNumber(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"soon", "worker_background_hours must be a number"},
		{"\"4\"", "worker_background_hours must be a number"},
		{"{}", "worker_background_hours must be a number"},
		{"0", "worker_background_hours must be a positive number"},
		{"-4", "worker_background_hours must be a positive number"},
		{"0.0000000000001", "worker_background_hours must be a positive number"},
		{"596.5", "worker_background_hours must be at most 596"},
		{"1000000", "worker_background_hours must be at most 596"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"worker_background_hours: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A key the file writes with no value at all leaves the default standing, as `!!null` does for
// every other duration key (readHours).
func TestWorkerBackgroundReadsAnEmptyKeyAsTheDefault(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"worker_background_hours:\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.WorkerBackground != 4*time.Hour {
		t.Errorf("WorkerBackground = %s, want the four-hour default", cfg.WorkerBackground)
	}
}
