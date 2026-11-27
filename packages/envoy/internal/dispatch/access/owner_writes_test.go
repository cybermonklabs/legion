package access

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ownerChanges counts project key's project.access_changed events and the server's records of them
// (project_access_changes), and reads the project's owner.
func ownerChanges(t *testing.T, database *store.Store, key string) (appended, records int, owner string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from events where project_key = $1 and type = 'project.access_changed'),
			(select count(*) from project_access_changes c join events e on e.id = c.event_id where e.project_key = $1),
			(select coalesce(owner, '') from projects where key = $1)
	`, key).Scan(&appended, &records, &owner); err != nil {
		t.Fatalf("read project %s's owner changes: %v", key, err)
	}
	return appended, records, owner
}

// committed runs write in one transaction of database and fails the test unless it commits.
func committed(t *testing.T, database *store.Store, write func(tx pgx.Tx) error) {
	t.Helper()
	if err := inTx(t, database, write); err != nil {
		t.Fatal(err)
	}
}

// TestSetOwnerToTheCurrentOwnerWritesNothing: giving a project to the person who owns it already
// changes nothing, so it appends no project.access_changed and writes no project_access_changes row,
// and answers the project's access as it is.
func TestSetOwnerToTheCurrentOwnerWritesNothing(t *testing.T) {
	database := seedAccess(t)
	broker := events.NewBroker()
	beforeEvents, beforeRecords, _ := ownerChanges(t, database, "PUB")
	committed(t, database, func(tx pgx.Tx) error {
		access, event, err := SetOwner(context.Background(), tx, broker, "PUB", " Olga@Example.com ", model.Actor{Kind: "user", ID: olga})
		if err != nil {
			return err
		}
		if event.ID != 0 || access.Owner != olga || access.Project != "PUB" {
			t.Errorf("SetOwner to the current owner = event %d, access %s owned by %s; want no event and PUB owned by %s",
				event.ID, access.Project, access.Owner, olga)
		}
		return nil
	})
	if appended, records, owner := ownerChanges(t, database, "PUB"); appended != beforeEvents || records != beforeRecords || owner != olga {
		t.Fatalf("after SetOwner to the current owner: %d event(s), %d record(s), owner %s; want %d, %d and %s as before",
			appended, records, owner, beforeEvents, beforeRecords, olga)
	}
	// Control: a change to another owner appends one event and one record.
	committed(t, database, func(tx pgx.Tx) error {
		_, _, err := SetOwner(context.Background(), tx, broker, "PUB", carol, model.Actor{Kind: "user", ID: olga})
		return err
	})
	if appended, records, owner := ownerChanges(t, database, "PUB"); appended != beforeEvents+1 || records != beforeRecords+1 || owner != carol {
		t.Fatalf("after SetOwner to carol: %d event(s), %d record(s), owner %s; want one more of each and carol",
			appended, records, owner)
	}
}

// TestTheBootsGrantIsRecordedAsTheCommandsIs: GiveOwnerless, the boot's fill of a project the
// access migration left with no owner, writes the owner, one project.access_changed of change owner
// and its project_access_changes record naming no previous owner, as `envoy-dispatch project-owner`
// does; on a project that has an owner by then it writes nothing.
func TestTheBootsGrantIsRecordedAsTheCommandsIs(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		alter table projects disable trigger projects_owner_from_default;
		insert into projects (key, name, default_assignee) values ('STRAY', 'Its default never signed in', 'gone@example.com');
		alter table projects enable trigger projects_owner_from_default;
	`); err != nil {
		t.Fatalf("seed a project with no owner: %v", err)
	}
	broker := events.NewBroker()
	boot := model.Actor{Kind: "system", ID: "DISPATCH_PROJECT_OWNER_FALLBACK"}
	committed(t, database, func(tx pgx.Tx) error { return GiveOwnerless(ctx, tx, broker, "STRAY", carol, boot) })
	if appended, records, owner := ownerChanges(t, database, "STRAY"); appended != 1 || records != 1 || owner != carol {
		t.Fatalf("after the grant: %d event(s), %d record(s), owner %q; want one event, one record and carol", appended, records, owner)
	}
	var change, actor, principal, recordOwner string
	if err := database.Pool.QueryRow(ctx, `
		select e.payload->>'change', e.actor->>'id', c.principal, c.owner
		from events e join project_access_changes c on c.event_id = e.id
		where e.project_key = 'STRAY' and e.type = 'project.access_changed'
	`).Scan(&change, &actor, &principal, &recordOwner); err != nil {
		t.Fatalf("read the grant's event: %v", err)
	}
	if change != model.AccessOwnerChanged || actor != boot.ID || principal != "" || recordOwner != carol {
		t.Errorf("the grant's event = change %q by %q, record principal %q owner %q; want owner by %s, no principal, carol",
			change, actor, principal, recordOwner, boot.ID)
	}
	// A project that has an owner by the time its lock is held is left as it is.
	committed(t, database, func(tx pgx.Tx) error { return GiveOwnerless(ctx, tx, broker, "STRAY", dave, boot) })
	if appended, records, owner := ownerChanges(t, database, "STRAY"); appended != 1 || records != 1 || owner != carol {
		t.Fatalf("after a second grant: %d event(s), %d record(s), owner %q; want one, one and carol as before", appended, records, owner)
	}
}
