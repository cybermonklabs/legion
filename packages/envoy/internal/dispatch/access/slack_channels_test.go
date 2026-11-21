package access

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheSlackChannelsColumnRefusesABadList: the column holds Slack channel ids and at most 16 of
// them, whatever writes it, so a direct write cannot leave a project naming a channel nobody can
// post to. Control: the same test against the migration with its check removed stores both bad
// updates and fails naming each.
func TestTheSlackChannelsColumnRefusesABadList(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	seventeen := make([]string, 17)
	for index := range seventeen {
		seventeen[index] = "C0000000" + string(rune('A'+index))
	}
	for _, refused := range []struct {
		name     string
		channels []string
	}{
		{"seventeen ids", seventeen},
		{"a lowercase id", []string{"c012345678"}},
		{"an id of another shape", []string{"#general"}},
		{"an id longer than Slack's", []string{"C" + strings.Repeat("A", 21)}},
	} {
		_, err := database.Pool.Exec(ctx, `update projects set slack_channels = $2 where key = $1`, "PUB", refused.channels)
		if !isCheckViolation(err) {
			t.Errorf("%s: update = %v, want check_violation (23514)", refused.name, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `update projects set slack_channels = $2 where key = $1`,
		"PUB", []string{"C012345678", "GABCDEFGH1"}); err != nil {
		t.Fatalf("two good ids: %v", err)
	}
	if got := slackChannelsOf(t, database.Pool, "PUB"); !slices.Equal(got, []string{"C012345678", "GABCDEFGH1"}) {
		t.Fatalf("stored channels = %v, want the two written", got)
	}
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func slackChannelsOf(t *testing.T, q Querier, key string) []string {
	t.Helper()
	var channels []string
	if err := q.QueryRow(context.Background(), `select slack_channels from projects where key = $1`, key).Scan(&channels); err != nil {
		t.Fatalf("read %s's channels: %v", key, err)
	}
	return channels
}

// SetSlackChannels stores the list sorted and deduplicated, appends one project.access_changed
// naming the change, and refuses an id of another shape and a list over the bound, writing nothing.
func TestSetSlackChannelsStoresASortedList(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: olga}

	var answer model.ProjectAccess
	var event model.Event
	if err := inTx(t, database, func(tx pgx.Tx) error {
		var err error
		answer, event, err = SetSlackChannels(ctx, tx, broker, "PUB", []string{"GABCDEFGH1", "C012345678", "C012345678"}, by)
		return err
	}); err != nil {
		t.Fatalf("SetSlackChannels: %v", err)
	}
	want := []string{"C012345678", "GABCDEFGH1"}
	if !slices.Equal(answer.SlackChannels, want) || !slices.Equal(slackChannelsOf(t, database.Pool, "PUB"), want) {
		t.Fatalf("channels = %v (stored %v), want %v sorted and deduplicated", answer.SlackChannels, slackChannelsOf(t, database.Pool, "PUB"), want)
	}
	payload, ok := event.Payload.(model.ProjectAccessEventPayload)
	if event.Type != "project.access_changed" || !ok || payload.Change != model.AccessSlackChannelsChanged || !slices.Equal(payload.SlackChannels, want) {
		t.Fatalf("appended %s %+v, want project.access_changed with change %s carrying the list", event.Type, event.Payload, model.AccessSlackChannelsChanged)
	}

	seventeen := make([]string, 17)
	for index := range seventeen {
		seventeen[index] = "C0000000" + string(rune('A'+index))
	}
	for _, refused := range []struct {
		name     string
		channels []string
		names    string
	}{
		{"an id of another shape", []string{"C012345678", "#general"}, "#general"},
		{"an id longer than Slack's", []string{"C" + strings.Repeat("A", 21)}, "C" + strings.Repeat("A", 21)},
		{"seventeen ids", seventeen, "17"},
	} {
		err := inTx(t, database, func(tx pgx.Tx) error {
			_, _, err := SetSlackChannels(ctx, tx, broker, "PUB", refused.channels, by)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), refused.names) {
			t.Errorf("%s: SetSlackChannels = %v, want a refusal naming %q", refused.name, err, refused.names)
		}
		if got := slackChannelsOf(t, database.Pool, "PUB"); !slices.Equal(got, want) {
			t.Errorf("%s: the stored list moved to %v under a refused write", refused.name, got)
		}
	}
}
