package config

import (
	"strings"
	"testing"
	"time"
)

// notification_stream_max_age_hours is how long the notification stream keeps a message, which
// the intake's early give-up reads and a daemon held to its projects may not read from the stream
// itself. A file that configures the workflow must say it: leaving it out is refused, and is not
// read as a stream that keeps every message, which only the word unlimited says. A value that is
// no positive number of hours and not that word is refused; a positive number, decimals among them,
// is read in hours. A daemon with no workflow needs none.
func TestTheNotificationStreamsAgeIsSuppliedAndOmittedIsNotUnlimited(t *testing.T) {
	const key = "notification_stream_max_age_hours: 72\n"
	if !strings.Contains(minimalFile, key) {
		t.Fatalf("minimalFile holds no %q, so the rows below would read another file than they say", strings.TrimSpace(key))
	}
	without := strings.Replace(minimalFile, key, "", 1)
	with := func(value string) string { return without + "notification_stream_max_age_hours: " + value + "\n" }

	t.Run("left out of a workflow's file", func(t *testing.T) {
		// The control first: the same file with the key loads, so the refusal below is the key's.
		if _, err := Load(writeConfigFile(t, minimalFile), noEnv); err != nil {
			t.Fatalf("Load of the file with the key: %v, want it to load", err)
		}
		_, err := Load(writeConfigFile(t, without), noEnv)
		if err == nil || err.Error() != missingNotificationStreamMaxAge {
			t.Fatalf("Load of a workflow's file with no notification_stream_max_age_hours = %v, want %q", err, missingNotificationStreamMaxAge)
		}
		if _, err := LoadForValidation(writeConfigFile(t, without), noEnv); err == nil || err.Error() != missingNotificationStreamMaxAge {
			t.Fatalf("LoadForValidation (legion config check) of the same file = %v, want the same refusal", err)
		}
	})

	for _, row := range []struct {
		value string
		want  time.Duration
	}{
		{"72", 72 * time.Hour},
		{"0.5", 30 * time.Minute},
		{"unlimited", 0},
	} {
		t.Run("reads "+row.value, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, with(row.value)), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.NotificationStreamMaxAge != row.want {
				t.Fatalf("NotificationStreamMaxAge = %s, want %s", cfg.NotificationStreamMaxAge, row.want)
			}
		})
	}

	const refusal = "notification_stream_max_age_hours must be a positive number of hours, or unlimited"
	for _, row := range []struct{ value, want string }{
		{"", refusal},
		{"~", refusal},
		{"forever", refusal},
		{"Unlimited", refusal},
		{`"72"`, refusal},
		{"72h", refusal},
		{"[72]", refusal},
		{"0", "notification_stream_max_age_hours must be a positive number"},
		{"-72", "notification_stream_max_age_hours must be a positive number"},
		{"1e300", "notification_stream_max_age_hours must be at most 2562047"},
	} {
		t.Run("refuses "+strings.TrimSpace(row.value+" (empty)"), func(t *testing.T) {
			_, err := Load(writeConfigFile(t, with(row.value)), noEnv)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Load with notification_stream_max_age_hours: %s = %v, want a refusal naming %q", row.value, err, row.want)
			}
		})
	}

	t.Run("not needed without a workflow", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"), noEnv)
		if err != nil {
			t.Fatalf("Load of a file with no workflow: %v", err)
		}
		if cfg.NotificationStreamMaxAge != 0 {
			t.Fatalf("NotificationStreamMaxAge = %s for a daemon with no workflow, want 0, which no intake reads", cfg.NotificationStreamMaxAge)
		}
	})
}
