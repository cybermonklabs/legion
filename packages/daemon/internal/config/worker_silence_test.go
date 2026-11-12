package config

import (
	"testing"
	"time"
)

// `worker_silence_minutes` is the bound workflow.SilentPhaseWorker judges a quiet phase worker
// by, and half an hour is what a file that names the key nowhere runs on.
func TestWorkerSilenceDefaultsToHalfAnHour(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.WorkerSilence != 30*time.Minute {
		t.Errorf("WorkerSilence = %s, want the half-hour default", cfg.WorkerSilence)
	}
	if cfg.WorkerSilence < minWorkerSilence {
		t.Errorf("the default %s is under the floor %s, so no file could pass the check",
			cfg.WorkerSilence, minWorkerSilence)
	}
}

// A value the floor leaves standing is read as written, the floor itself among them: the floor is
// the shortest bound a deployment may run, not the shortest it may name.
//
// A silence above the four-hour default of `worker_background_hours` carries that key with it,
// since the loader holds the second above the first (checkWorkerBackground). That is what bounds
// the longest silence a file can carry: `worker_background_hours` is read in whole hours and
// bounded at 596 of them, so a silence at readMinutes' own bound of 35791 minutes — 596h31m —
// is above every value the companion key can take and no file can name it. The refusal of a
// value past that bound is TestWorkerSilenceRefusesAValueThatIsNoPositiveBoundedNumber's.
func TestWorkerSilenceTakesEveryValueFromTheFloorUp(t *testing.T) {
	for _, tc := range []struct {
		value string
		// beside is the rest of the file the case needs, empty for a case that names this key
		// alone.
		beside string
		want   time.Duration
	}{
		{value: "16", want: minWorkerSilence},
		{value: "20", want: 20 * time.Minute},
		{value: "90", want: 90 * time.Minute},
		{value: "20.5", want: 20*time.Minute + 30*time.Second},
		{value: "35759", beside: "worker_background_hours: 596\n", want: 35759 * time.Minute},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"worker_silence_minutes: "+tc.value+"\n"+tc.beside), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.WorkerSilence != tc.want {
				t.Errorf("WorkerSilence = %s, want %s", cfg.WorkerSilence, tc.want)
			}
		})
	}
}

// A minute under the floor is refused, and the refusal carries all three numbers an operator
// needs to act on it: the key, the value their file names and the floor it is under. The two
// waits the floor is reckoned from are named too, since neither is in the file they are reading.
func TestWorkerSilenceRefusesAValueUnderTheFloor(t *testing.T) {
	_, err := LoadForValidation(writeConfigFile(t, minimalFile+"worker_silence_minutes: 15\n"), noEnv)

	want := "worker_silence_minutes (15m0s) is below the 16m0s floor under it: a healthy phase worker that answers its reminder with a WAITING line is quiet for the 15m that reply buys before the reminder comes again (DEFAULT_STALL_LIMITS.waitingMs), and under runtime: kubernetes a pod with no turn in progress and none due is released after runtime.kubernetes.idle_release.idle_minutes (10 by default) and woken at the backstop; a shorter bound would send a second task for work the worker already holds, to a worker whose own reminder is about to start a turn or whose pod the release is about to take"
	if err == nil || err.Error() != want {
		t.Errorf("LoadForValidation error = %v, want %q", err, want)
	}
}

// readMinutes' own refusals, which run before the floor is reckoned at all: a value that is no
// number, one that is not positive, and one past the timer bound in whole minutes.
func TestWorkerSilenceRefusesAValueThatIsNoPositiveBoundedNumber(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"soon", "worker_silence_minutes must be a number"},
		{"\"30\"", "worker_silence_minutes must be a number"},
		{"{}", "worker_silence_minutes must be a number"},
		{"0", "worker_silence_minutes must be a positive number"},
		{"-30", "worker_silence_minutes must be a positive number"},
		{"0.0000000000001", "worker_silence_minutes must be a positive number"},
		{"35791.5", "worker_silence_minutes must be at most 35791"},
		{"1000000", "worker_silence_minutes must be at most 35791"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"worker_silence_minutes: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A key the file writes with no value at all leaves the default standing, as `!!null` does for
// every other duration key (readHours, readMinutes).
func TestWorkerSilenceReadsAnEmptyKeyAsTheDefault(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"worker_silence_minutes:\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.WorkerSilence != 30*time.Minute {
		t.Errorf("WorkerSilence = %s, want the half-hour default", cfg.WorkerSilence)
	}
}
