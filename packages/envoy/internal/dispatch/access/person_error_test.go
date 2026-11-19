package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var errDatabaseGone = errors.New("the database is not answering")

// failingRow is the row of a read the database did not answer.
type failingRow struct{}

func (failingRow) Scan(...any) error { return errDatabaseGone }

// failAtTx is a transaction whose statement number failAt fails as a database that stopped
// answering fails it, and whose every other statement runs on the real transaction.
type failAtTx struct {
	pgx.Tx
	failAt, seen int
}

func (tx *failAtTx) fails() bool {
	tx.seen++
	return tx.seen == tx.failAt
}

func (tx *failAtTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.fails() {
		return pgconn.CommandTag{}, errDatabaseGone
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *failAtTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx.fails() {
		return nil, errDatabaseGone
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *failAtTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.fails() {
		return failingRow{}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

// The person every read and write below is about, a full member. Each part of the address is
// distinctive, so a part that reached an error is found whichever part it is.
const (
	hiddenLocal  = "zq.hidden.person"
	hiddenDomain = "zq-secret.example"
	hiddenEmail  = hiddenLocal + "@" + hiddenDomain
)

// namesThePerson reports the part of hiddenEmail that text holds, or "" when it holds none.
func namesThePerson(text string) string {
	for _, part := range []string{hiddenEmail, hiddenLocal, hiddenDomain} {
		if strings.Contains(strings.ToLower(text), part) {
			return part
		}
	}
	return ""
}

// TestNoAccessErrorNamesAPerson: every read and write of the access package, about one person,
// is run once for each statement it makes, with that statement failing as a database that
// stopped answering fails it. Each failure answers an error that wraps the database's and names
// no part of the person's email: every caller knows whom it asked, and the callers that log an
// error (the API's failed-handler line, the whoami warning) would otherwise write the address
// (CodeQL go/clear-text-logging, alert #143). The person is named wherever a read or a write takes
// one: the login read, the listed person, the owner given, the group's owner and member, and the
// actor of each write.
func TestNoAccessErrorNamesAPerson(t *testing.T) {
	database := seedAccess(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email, member_kind) values ($1, 'full');
	`, hiddenEmail); err != nil {
		t.Fatalf("seed the person: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update projects set slack_channels = '{C0SHARED001}' where key = 'OTHER'`); err != nil {
		t.Fatalf("name OTHER's channel: %v", err)
	}
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: hiddenEmail}
	steps := []struct {
		name string
		run  func(tx pgx.Tx) error
	}{
		{"MemberKind", func(tx pgx.Tx) error {
			_, err := MemberKind(ctx, tx, " "+strings.ToUpper(hiddenEmail)+" ")
			return err
		}},
		{"PersonAnswer", func(tx pgx.Tx) error { _, err := PersonAnswer(ctx, tx, hiddenEmail); return err }},
		{"Answer", func(tx pgx.Tx) error { _, err := Answer(ctx, tx, "PRIV", hiddenEmail); return err }},
		{"Listed", func(tx pgx.Tx) error { _, err := Listed(ctx, tx, hiddenEmail); return err }},
		{"Resolve", func(tx pgx.Tx) error { _, err := Resolve(ctx, tx, cookie(hiddenEmail)); return err }},
		{"ReadProject", func(tx pgx.Tx) error {
			_, err := ReadProject(ctx, tx, "PRIV", viewerArg(t, *model.ServerViewer))
			return err
		}},
		{"ReadGroup", func(tx pgx.Tx) error { _, err := ReadGroup(ctx, tx, "crew"); return err }},
		{"ListGroups", func(tx pgx.Tx) error { _, err := ListGroups(ctx, tx, "", model.MaxGroupsPerOwner); return err }},
		{"LockForAccessWrite", func(tx pgx.Tx) error { _, err := LockForAccessWrite(ctx, tx, "PRIV", "crew"); return err }},
		{"SetOwner", func(tx pgx.Tx) error { _, _, err := SetOwner(ctx, tx, broker, "PRIV", hiddenEmail, by); return err }},
		{"SetVisibility", func(tx pgx.Tx) error {
			_, _, err := SetVisibility(ctx, tx, broker, "PRIV", model.ProjectPublic, by)
			return err
		}},
		{"SetSlackChannels", func(tx pgx.Tx) error {
			_, _, err := SetSlackChannels(ctx, tx, broker, "PRIV", []string{"C0123456789"}, by, model.ServerViewer)
			return err
		}},
		// OTHER lists C0SHARED001, so this write is refused once it has read who lists the channel:
		// the refusal itself is this step succeeding.
		{"SetSlackChannels of a channel another private project lists", func(tx pgx.Tx) error {
			_, _, err := SetSlackChannels(ctx, tx, broker, "PRIV", []string{"C0SHARED001"}, by, model.ServerViewer)
			if errors.Is(err, ErrSlackChannelShared) {
				return nil
			}
			return err
		}},
		{"SharedSlackChannel", func(tx pgx.Tx) error {
			_, _, err := SharedSlackChannel(ctx, tx, "PRIV", []string{"C0SHARED001"})
			return err
		}},
		{"PrivateProjects", func(tx pgx.Tx) error { _, err := PrivateProjects(ctx, tx); return err }},
		{"AddPrincipal of the person", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PRIV", PrincipalPerson, hiddenEmail, by)
			return err
		}},
		{"RemovePrincipal of the person", func(tx pgx.Tx) error {
			_, _, err := RemovePrincipal(ctx, tx, broker, "PRIV", PrincipalPerson, hiddenEmail, by)
			return err
		}},
		{"AddPrincipal of a group", func(tx pgx.Tx) error {
			_, _, err := AddPrincipal(ctx, tx, broker, "PRIV", PrincipalGroup, "crew", by)
			return err
		}},
		{"RemovePrincipal of a group", func(tx pgx.Tx) error {
			_, _, err := RemovePrincipal(ctx, tx, broker, "OTHER", PrincipalGroup, "crew", by)
			return err
		}},
		{"PutGroup", func(tx pgx.Tx) error {
			_, _, err := PutGroup(ctx, tx, broker, "hidden-crew", hiddenEmail, []string{hiddenEmail, dave}, by)
			return err
		}},
		{"DeleteGroup", func(tx pgx.Tx) error { _, err := DeleteGroup(ctx, tx, broker, "crew", by); return err }},
	}
	for _, step := range steps {
		failed := 0
		for failAt := 1; ; failAt++ {
			if failAt > 64 {
				t.Fatalf("%s still makes a statement at number 64; it has no end this test finds", step.name)
			}
			inner, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			tx := &failAtTx{Tx: inner, failAt: failAt}
			err = step.run(tx)
			if rollback := inner.Rollback(ctx); rollback != nil {
				t.Fatalf("roll back: %v", rollback)
			}
			if tx.seen < failAt {
				// The step ended before it made statement failAt: every statement it makes has failed.
				if err != nil {
					t.Errorf("%s with no statement failing = %v, want it to succeed", step.name, err)
				}
				break
			}
			at := fmt.Sprintf("%s with its statement %d failing", step.name, failAt)
			if err == nil {
				t.Errorf("%s = nil error, want the database's failure", at)
				continue
			}
			failed++
			if !errors.Is(err, errDatabaseGone) {
				t.Errorf("%s = %q, want it to wrap the database's failure", at, err)
			}
			if part := namesThePerson(err.Error()); part != "" {
				t.Errorf("%s = %q, which names %q of the person it was about", at, err, part)
			}
		}
		if failed == 0 {
			t.Errorf("%s made no statement, so this test failed none of them", step.name)
		}
	}
}
