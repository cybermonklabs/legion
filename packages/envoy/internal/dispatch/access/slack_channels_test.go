package access

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
		answer, event, err = SetSlackChannels(ctx, tx, broker, "PUB", []string{"GABCDEFGH1", "C012345678", "C012345678"}, by, nil)
		return err
	}); err != nil {
		t.Fatalf("SetSlackChannels: %v", err)
	}
	want := []string{"C012345678", "GABCDEFGH1"}
	if !slices.Equal(answer.SlackChannels, want) || !slices.Equal(slackChannelsOf(t, database.Pool, "PUB"), want) {
		t.Fatalf("channels = %v (stored %v), want %v sorted and deduplicated", answer.SlackChannels, slackChannelsOf(t, database.Pool, "PUB"), want)
	}
	payload, ok := event.Payload.(model.ProjectAccessEventPayload)
	if want := (model.ProjectAccessEventPayload{Project: "PUB", Change: model.AccessSlackChannelsChanged}); event.Type != "project.access_changed" || !ok || payload != want {
		t.Fatalf("appended %s %+v, want project.access_changed carrying %+v alone, no channel id", event.Type, event.Payload, want)
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
			_, _, err := SetSlackChannels(ctx, tx, broker, "PUB", refused.channels, by, nil)
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

// changedAtOf reads project key's slack_channels_changed_at, nil while it is null.
func changedAtOf(t *testing.T, q Querier, key string) *time.Time {
	t.Helper()
	var changedAt *time.Time
	if err := q.QueryRow(context.Background(), `select slack_channels_changed_at from projects where key = $1`, key).Scan(&changedAt); err != nil {
		t.Fatalf("read %s's channels time: %v", key, err)
	}
	return changedAt
}

// TestTheChannelsTimeMovesOnlyWhenTheListChanges: slack_channels_changed_at is when a project's
// Slack channels last changed, null until a write changes the list, so a write of the empty list
// over the empty list a project starts with leaves it null. A write that changes it sets it to the
// write's own statement time (statement_timestamp()), which comes after every lock the write
// waited for, never to the transaction's start: here each transaction sleeps before it writes, as
// one queued behind another's locks waits. A write of the same list, in another order and over a
// stored list in another order, leaves it. The write's answer and event, ReadProject and the
// membership answer carry it. Controls: set the time on every write, and the empty list's row and
// the same list's row fail naming the project; set it on a write of the empty list over the empty
// list alone, and the empty list's row alone fails naming the project; stamp the transaction's
// now(), and the first change's row fails naming the time before the write.
func TestTheChannelsTimeMovesOnlyWhenTheListChanges(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: olga}
	// write writes PUB's list in a transaction that sleeps first, and answers the clock just
	// before the write and just after it, with what the write answered.
	write := func(channels ...string) (before, after time.Time, answer model.ProjectAccess, event model.Event) {
		t.Helper()
		if err := inTx(t, database, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `select clock_timestamp() from pg_sleep(0.05)`).Scan(&before); err != nil {
				return err
			}
			var err error
			if answer, event, err = SetSlackChannels(ctx, tx, broker, "PUB", channels, by, nil); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&after)
		}); err != nil {
			t.Fatalf("write PUB's channels %v: %v", channels, err)
		}
		return before, after, answer, event
	}
	if got := changedAtOf(t, database.Pool, "PUB"); got != nil {
		t.Fatalf("PUB's channels time before any write = %v, want null", got)
	}
	server := viewerArg(t, *model.ServerViewer)
	if read, err := ReadProject(ctx, database.Pool, "PUB", server); err != nil || read.SlackChannelsChangedAt != nil {
		t.Fatalf("ReadProject of PUB before any write = %+v, %v; want its channels time null", read, err)
	}

	// The empty list over the empty list PUB starts with: the same list, so the time stays null.
	write([]string{}...)
	if got := changedAtOf(t, database.Pool, "PUB"); got != nil {
		t.Errorf("a write of the empty list over PUB's empty list set PUB's channels time to %v, want it left null", got)
	}

	before, after, answer, event := write("GABCDEFGH1", "C012345678")
	first := changedAtOf(t, database.Pool, "PUB")
	if first == nil || first.Before(before) || first.After(after) {
		t.Fatalf("PUB's first write left its channels time at %v, want the write's own statement time, from %v to %v", first, before, after)
	}
	// project.access_changed names the project and the change alone, no list and no time (every
	// reader of the project's events reads it), so the time is carried by the write's answer, the
	// access read and the membership answer.
	if payload, ok := event.Payload.(model.ProjectAccessEventPayload); !ok || payload.Change != model.AccessSlackChannelsChanged {
		t.Errorf("the write's project.access_changed = %#v, want the slack_channels change", event.Payload)
	}
	read, readErr := ReadProject(ctx, database.Pool, "PUB", server)
	membership, answerErr := Answer(ctx, database.Pool, "PUB", "")
	for _, carried := range []struct {
		name string
		at   *time.Time
	}{
		{"the write's answer", answer.SlackChannelsChangedAt},
		{"ReadProject", read.SlackChannelsChangedAt},
		{"the membership answer", membership.SlackChannelsChangedAt},
	} {
		if readErr != nil || answerErr != nil || carried.at == nil || !carried.at.Equal(*first) {
			t.Errorf("%s carries PUB's channels time as %v (read %v, answer %v), want %v", carried.name, carried.at, readErr, answerErr, *first)
		}
	}

	// The same list, written in another order over a list stored in another order by a direct
	// write, which leaves the time alone.
	if _, err := database.Pool.Exec(ctx, `update projects set slack_channels = '{GABCDEFGH1,C012345678}' where key = 'PUB'`); err != nil {
		t.Fatalf("store PUB's list in another order: %v", err)
	}
	write("GABCDEFGH1", "C012345678", "GABCDEFGH1")
	if got := changedAtOf(t, database.Pool, "PUB"); got == nil || !got.Equal(*first) {
		t.Fatalf("a write of PUB's own list in another order moved PUB's channels time from %v to %v, want it left as it was", *first, got)
	}

	before, after, _, _ = write("C012345678")
	if got := changedAtOf(t, database.Pool, "PUB"); got == nil || got.Before(before) || got.After(after) || !got.After(*first) {
		t.Fatalf("a write of a different list left PUB's channels time at %v, want that write's own statement time, from %v to %v, after %v", got, before, after, *first)
	}
}

// TestASharedSlackChannelNamesItsProjectOnlyToACallerWhoSeesIt: a Slack channel belongs to one
// private project, so a write of a private project's list naming a channel another private project
// lists is refused with ErrSlackChannelShared and writes nothing. The refusal names the other
// project only to a caller who sees it by the read rule: the viewer Resolve answers for the
// request's own caller, which the write is handed beside the person it records, and nothing the
// write resolves itself. An agent credential with no list sees only public projects, so it reads
// the refusal that names no project however much its person sees, and so does a write handed no
// viewer. A public project's list is never refused for sharing a channel. OTHER is private and
// names C0SHARED001 here; each write is of PRIV's list, or PUB's. Controls: name OTHER whenever
// the recorded person sees it, and olga's token and broker token listing nothing fail naming it;
// refuse a public project's write too, and PUB's row fails.
func TestASharedSlackChannelNamesItsProjectOnlyToACallerWhoSeesIt(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	if _, err := database.Pool.Exec(ctx, `update projects set slack_channels = '{C0SHARED001}' where key = 'OTHER'`); err != nil {
		t.Fatalf("name OTHER's channel: %v", err)
	}
	const (
		named  = "C0SHARED001 is listed by OTHER, a private project; a Slack channel belongs to one private project"
		hidden = "C0SHARED001 is listed by a private project you cannot see"
	)
	// brokerToken is a person's access token the secrets broker issued, with no list, as the API
	// resolves it (api/broker_token.go's brokerTokenActor): the person scope alone, since no
	// broker-issued token holds approve (brokerPersonScopes), and Proven.
	brokerToken := model.Actor{Kind: "session", ID: "ses_olga", Owner: new(olga), Credential: &model.Credential{Scopes: []string{model.ScopePerson}, Session: "ses_olga"}, Proven: true}
	for _, row := range []struct {
		name   string
		caller model.Actor
		want   string
	}{
		{"olga, OTHER's owner, at her browser", cookie(olga), named},
		{"dave, a full member of the group OTHER lists", cookie(dave), named},
		{"carol, whom PRIV lists and OTHER does not", cookie(carol), hidden},
		{"olga's personal token listing no projects", personalToken(olga), hidden},
		{"olga's broker token listing no projects", brokerToken, hidden},
		{"olga's token listing PRIV alone", personalToken(olga, "PRIV"), hidden},
		{"olga's token listing OTHER and PRIV", personalToken(olga, "OTHER", "PRIV"), named},
	} {
		viewer, err := Resolve(ctx, database.Pool, row.caller)
		if err != nil {
			t.Fatalf("%s: resolve: %v", row.name, err)
		}
		// The owner's write runs as the caller's person; the refusal is written for the caller.
		by := model.Actor{Kind: "user", ID: viewer.Person}
		err = inTx(t, database, func(tx pgx.Tx) error {
			_, _, err := SetSlackChannels(ctx, tx, broker, "PRIV", []string{"C0SHARED001"}, by, &viewer)
			return err
		})
		if !errors.Is(err, ErrSlackChannelShared) || err.Error() != row.want {
			t.Errorf("%s: a write of PRIV's list naming OTHER's channel = %v, want ErrSlackChannelShared reading %q", row.name, err, row.want)
		}
		if got := slackChannelsOf(t, database.Pool, "PRIV"); len(got) != 0 || changedAtOf(t, database.Pool, "PRIV") != nil {
			t.Errorf("%s: PRIV's list moved to %v under a refused write", row.name, got)
		}
	}
	if err := inTx(t, database, func(tx pgx.Tx) error {
		_, _, err := SetSlackChannels(ctx, tx, broker, "PRIV", []string{"C0SHARED001"}, model.Actor{Kind: "user", ID: olga}, nil)
		return err
	}); !errors.Is(err, ErrSlackChannelShared) || err.Error() != hidden {
		t.Errorf("a write handed no viewer = %v, want ErrSlackChannelShared reading %q", err, hidden)
	}
	if err := inTx(t, database, func(tx pgx.Tx) error {
		_, _, err := SetSlackChannels(ctx, tx, broker, "PUB", []string{"C0SHARED001"}, cookie(olga), nil)
		return err
	}); err != nil {
		t.Errorf("a write of public PUB's list naming OTHER's channel = %v, want it stored: a public project's list is never refused for sharing", err)
	}
	if got := slackChannelsOf(t, database.Pool, "PUB"); !slices.Equal(got, []string{"C0SHARED001"}) {
		t.Errorf("PUB's list = %v, want OTHER's channel", got)
	}
}
