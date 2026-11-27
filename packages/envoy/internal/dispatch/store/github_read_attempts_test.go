package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// The record keeps one row for each read, which every attempt overwrites with its own values: a
// failed attempt's cause, then a succeeded attempt's none, at a later time.
func TestGitHubReadAttemptsKeepOneRowPerReadWithTheNewestValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	attempts := NewGitHubReadAttempts(migratedEmptyStore(t).Pool)
	if rows, err := attempts.All(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("an empty record reads %v, %v; want no row", rows, err)
	}

	cause := "GET https://api.github.com/app/hook/deliveries: status 503: unavailable"
	if err := attempts.Record(ctx, githubreads.WebhookRedelivery, false, cause); err != nil {
		t.Fatalf("record the failed attempt: %v", err)
	}
	if err := attempts.Record(ctx, githubreads.CIReconcile, true, ""); err != nil {
		t.Fatalf("record the CI reconcile's attempt: %v", err)
	}
	failed := readAttempts(t, attempts)
	if got := failed[githubreads.WebhookRedelivery]; got.Succeeded || got.Cause != cause || got.At.IsZero() {
		t.Fatalf("the failed attempt reads %+v, want failed with its cause and a time", got)
	}

	if err := attempts.Record(ctx, githubreads.WebhookRedelivery, true, "a cause a succeeded attempt does not keep"); err != nil {
		t.Fatalf("record the succeeded attempt: %v", err)
	}
	newest := readAttempts(t, attempts)
	if len(newest) != 2 {
		t.Fatalf("the record holds %d rows (%v), want one for each of the two reads", len(newest), newest)
	}
	got := newest[githubreads.WebhookRedelivery]
	if !got.Succeeded || got.Cause != "" || got.At.Before(failed[githubreads.WebhookRedelivery].At) {
		t.Fatalf("the newest attempt reads %+v, want succeeded with no cause, no earlier than %s", got, failed[githubreads.WebhookRedelivery].At)
	}
	if other := newest[githubreads.CIReconcile]; !other.Succeeded || other.Cause != "" {
		t.Fatalf("the other read's row reads %+v, want it as it was recorded", other)
	}
}

// readAttempts reads the record keyed by read, failing on a read that holds two rows.
func readAttempts(t *testing.T, attempts *GitHubReadAttempts) map[githubreads.Read]githubreads.Recorded {
	t.Helper()
	rows, err := attempts.All(context.Background())
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	keyed := map[githubreads.Read]githubreads.Recorded{}
	for _, row := range rows {
		if _, twice := keyed[row.Read]; twice {
			t.Fatalf("the record holds two rows for %s: %v", row.Read, rows)
		}
		keyed[row.Read] = row
	}
	return keyed
}

// The table holds a row to what GET /healthz may answer: one of the five reads, a cause of at most
// 1,024 bytes, a cause exactly when the attempt failed.
func TestTheGitHubReadAttemptsTableRefusesARowHealthCannotAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := migratedEmptyStore(t)
	cases := []struct {
		name       string
		read       string
		succeeded  bool
		cause      *string
		constraint string
	}{{
		name:       "a cause over 1,024 bytes",
		read:       string(githubreads.DeliveryReconcile),
		cause:      new(strings.Repeat("a", githubreads.CauseLimit+1)),
		constraint: "github_read_attempts_cause_fits",
	}, {
		// U+FFFD is three bytes, so 342 of them are 1,026 bytes in 342 characters.
		name:       "a cause over 1,024 bytes in fewer characters",
		read:       string(githubreads.DeliveryReconcile),
		cause:      new(strings.Repeat("\uFFFD", 342)),
		constraint: "github_read_attempts_cause_fits",
	}, {
		name:       "a succeeded attempt with a cause",
		read:       string(githubreads.ArchitectureImport),
		succeeded:  true,
		cause:      new("GET https://api.github.com/repos/acme/web: status 502: bad gateway"),
		constraint: "github_read_attempts_cause_when_failed",
	}, {
		name:       "a failed attempt without a cause",
		read:       string(githubreads.ArchitectureImport),
		constraint: "github_read_attempts_cause_when_failed",
	}, {
		name:       "a read that is none of the five",
		read:       "intake",
		succeeded:  true,
		constraint: "github_read_attempts_read_check",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := database.Pool.Exec(ctx, `insert into github_read_attempts (read, at, succeeded, cause) values ($1, now(), $2, $3)`,
				tc.read, tc.succeeded, tc.cause)
			var refusal *pgconn.PgError
			if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != tc.constraint {
				t.Fatalf("insert error = %v, want SQLSTATE 23514 naming %s", err, tc.constraint)
			}
		})
	}
	// The cause at the limit is kept.
	if err := NewGitHubReadAttempts(database.Pool).Record(ctx, githubreads.DeliveryReconcile, false, strings.Repeat("a", githubreads.CauseLimit)); err != nil {
		t.Fatalf("record a cause of exactly %d bytes: %v", githubreads.CauseLimit, err)
	}
}

// The check on `read` admits every read githubreads names, the code survival run's among them
// (migration 20261126103900), which the record's first migration did not: each read of
// githubreads.Reads records an attempt and comes back as the row it was written as, and the table
// holds one row for each.
func TestTheGitHubReadAttemptsTableAdmitsEveryRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	attempts := NewGitHubReadAttempts(migratedEmptyStore(t).Pool)
	reads := githubreads.Reads()
	for _, read := range reads {
		if err := attempts.Record(ctx, read, false, "GET https://api.github.com/repos/acme/web: status 503"); err != nil {
			t.Errorf("record an attempt of %s: %v", read, err)
		}
	}
	recorded := readAttempts(t, attempts)
	for _, read := range reads {
		if got, found := recorded[read]; !found || got.Succeeded || got.Cause == "" {
			t.Errorf("the record of %s reads %+v (found %t), want the failed attempt it was written as", read, got, found)
		}
	}
	if len(recorded) != len(reads) {
		t.Errorf("the record holds %d rows (%v), want one for each of the %d reads", len(recorded), recorded, len(reads))
	}
}

// Each write of a row notifies githubreads.Channel with the read's name, the insert and the
// update alike, so every process refreshes its copy whichever process recorded the attempt.
func TestRecordingAGitHubReadAttemptNotifiesItsChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	database := migratedEmptyStore(t)
	listener := connectForTest(t, ctx, database)
	if _, err := listener.Exec(ctx, "listen "+githubreads.Channel); err != nil {
		t.Fatalf("listen on %s: %v", githubreads.Channel, err)
	}
	attempts := NewGitHubReadAttempts(database.Pool)
	for _, write := range []struct {
		name      string
		read      githubreads.Read
		succeeded bool
		cause     string
	}{
		{"the first attempt of a read", githubreads.CIReconcile, false, "GraphQL: status 502"},
		{"the next attempt of the same read", githubreads.CIReconcile, true, ""},
		{"the first attempt of another read", githubreads.ArchitectureImport, true, ""},
	} {
		if err := attempts.Record(ctx, write.read, write.succeeded, write.cause); err != nil {
			t.Fatalf("%s: record: %v", write.name, err)
		}
		notification, err := listener.WaitForNotification(ctx)
		if err != nil || notification.Channel != githubreads.Channel || notification.Payload != string(write.read) {
			t.Fatalf("%s: notification = %+v, %v; want %s with payload %s", write.name, notification, err, githubreads.Channel, write.read)
		}
	}
}
