package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// envoy-dispatch project-owner <KEY> <email> gives a project to another full member at any time:
// here its owner is still a full member, and the project moves all the same, in one transaction
// that appends project.access_changed as the command. It exits 2, writing nothing, for a key no
// project holds, for a restricted member, for a login no people row holds, and for wrong
// arguments.
func TestProjectOwnerCommandGivesTheProject(t *testing.T) {
	// Not parallel: it installs pmdoc's process-wide block schema (the command's openMigrated).
	ctx := context.Background()
	database := storetest.Open(t)
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email, member_kind) values
			('olga@example.com', 'full'), ('carol@example.com', 'full'), ('bob@other.example', 'restricted');
		insert into projects (key, name, default_assignee) values ('WID', 'Widgets', 'olga@example.com');
	`); err != nil {
		t.Fatalf("seed people and a project: %v", err)
	}
	run := func(args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, append([]string{"project-owner"}, args...), env, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	owner := func() string {
		t.Helper()
		var owner string
		if err := database.Pool.QueryRow(ctx, `select owner from projects where key = 'WID'`).Scan(&owner); err != nil {
			t.Fatalf("read WID's owner: %v", err)
		}
		return owner
	}
	eventCount := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from events`).Scan(&count); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return count
	}

	if got := owner(); got != "olga@example.com" {
		t.Fatalf("WID's owner before the command = %q, want olga@example.com, its default assignee", got)
	}
	before := eventCount()
	if code, out := run("WID", " Carol@Example.com "); code != 0 {
		t.Fatalf("project-owner WID carol: exit %d, want 0:\n%s", code, out)
	}
	if got := owner(); got != "carol@example.com" {
		t.Fatalf("WID's owner after the command = %q, want carol@example.com", got)
	}
	if after := eventCount(); after != before+1 {
		t.Fatalf("events moved from %d to %d, want one project.access_changed", before, after)
	}
	var eventType string
	var actorJSON, payloadJSON []byte
	if err := database.Pool.QueryRow(ctx, `
		select type, actor, payload from events where project_key = 'WID' order by id desc limit 1
	`).Scan(&eventType, &actorJSON, &payloadJSON); err != nil {
		t.Fatalf("read WID's last event: %v", err)
	}
	var actor model.Actor
	var payload model.ProjectAccessEventPayload
	if err := json.Unmarshal(actorJSON, &actor); err != nil {
		t.Fatalf("decode the event's actor %s: %v", actorJSON, err)
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("decode the event's payload %s: %v", payloadJSON, err)
	}
	// The owner itself is projects.owner's, read above; the event names the project and the change
	// alone, since every reader of the project's events reads it.
	if want := (model.ProjectAccessEventPayload{Project: "WID", Change: model.AccessOwnerChanged}); eventType != "project.access_changed" ||
		actor.Kind != "system" || actor.ID != "envoy-dispatch project-owner" || payload != want {
		t.Fatalf("WID's last event = %s by %s, payload %s; want project.access_changed by the system actor envoy-dispatch project-owner, carrying %+v",
			eventType, actorJSON, payloadJSON, want)
	}

	for _, refused := range []struct {
		name   string
		args   []string
		naming string
	}{
		{"a key no project holds", []string{"NOPE", "olga@example.com"}, "NOPE"},
		{"a restricted member", []string{"WID", "bob@other.example"}, "bob@other.example"},
		{"a login no people row holds", []string{"WID", "newbie@example.com"}, "newbie@example.com"},
		{"no argument", nil, "envoy-dispatch project-owner <KEY> <email>"},
		{"a key alone", []string{"WID"}, "envoy-dispatch project-owner <KEY> <email>"},
		{"an argument too many", []string{"WID", "olga@example.com", "again"}, "envoy-dispatch project-owner <KEY> <email>"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			before := eventCount()
			code, out := run(refused.args...)
			if code != 2 || !strings.Contains(out, refused.naming) {
				t.Errorf("project-owner %q: exit %d, want 2 naming %q:\n%s", refused.args, code, refused.naming, out)
			}
			if got := owner(); got != "carol@example.com" {
				t.Errorf("project-owner %q moved WID's owner to %q", refused.args, got)
			}
			if after := eventCount(); after != before {
				t.Errorf("project-owner %q moved events from %d to %d", refused.args, before, after)
			}
		})
	}
}
