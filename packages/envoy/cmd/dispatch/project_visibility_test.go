package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// envoy-dispatch project-visibility <KEY> public makes a private project public without the
// server, the repair of a private project the deployment's list no longer names: one transaction
// that appends project.access_changed as the command, after which no private project is off an
// empty list. It exits 2, writing nothing, for a key no project holds, for any value but public,
// and for wrong arguments.
func TestProjectVisibilityCommandMakesAProjectPublic(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'olga@example.com', 'private');
	`); err != nil {
		t.Fatalf("seed the private project PRIV: %v", err)
	}
	run := func(args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, append([]string{"project-visibility"}, args...), env, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	eventCount := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from events`).Scan(&count); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return count
	}
	visibility := func() string {
		t.Helper()
		var visibility string
		if err := database.Pool.QueryRow(ctx, `select visibility from projects where key = 'PRIV'`).Scan(&visibility); err != nil {
			t.Fatalf("read PRIV's visibility: %v", err)
		}
		return visibility
	}

	for _, refused := range []struct {
		name   string
		args   []string
		naming string
	}{
		{"a key no project holds", []string{"NOPE", "public"}, "NOPE"},
		{"the value private", []string{"PRIV", "private"}, "public"},
		{"no argument", nil, "envoy-dispatch project-visibility <KEY> public"},
		{"a key alone", []string{"PRIV"}, "envoy-dispatch project-visibility <KEY> public"},
		{"an argument too many", []string{"PRIV", "public", "again"}, "envoy-dispatch project-visibility <KEY> public"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			before := eventCount()
			code, out := run(refused.args...)
			if code != 2 || !strings.Contains(out, refused.naming) {
				t.Errorf("project-visibility %q: exit %d, want 2 naming %q:\n%s", refused.args, code, refused.naming, out)
			}
			if got := visibility(); got != model.ProjectPrivate {
				t.Errorf("project-visibility %q left PRIV %s, want it private as before", refused.args, got)
			}
			if after := eventCount(); after != before {
				t.Errorf("project-visibility %q moved events from %d to %d", refused.args, before, after)
			}
		})
	}

	before := eventCount()
	if code, out := run("PRIV", "public"); code != 0 {
		t.Fatalf("project-visibility PRIV public: exit %d, want 0:\n%s", code, out)
	}
	if got := visibility(); got != model.ProjectPublic {
		t.Fatalf("PRIV after the command is %s, want public", got)
	}
	var eventType string
	var actorJSON, payloadJSON []byte
	if err := database.Pool.QueryRow(ctx, `
		select type, actor, payload from events where project_key = 'PRIV' and id > $1 order by id
	`, before).Scan(&eventType, &actorJSON, &payloadJSON); err != nil {
		t.Fatalf("the command appended no project.access_changed on PRIV: %v", err)
	}
	var actor model.Actor
	var payload model.ProjectAccessEventPayload
	if err := json.Unmarshal(actorJSON, &actor); err != nil {
		t.Fatalf("decode the event's actor %s: %v", actorJSON, err)
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("decode the event's payload %s: %v", payloadJSON, err)
	}
	if after := eventCount(); eventType != "project.access_changed" || actor.Kind != "system" || actor.ID != "envoy-dispatch project-visibility" ||
		payload.Change != model.AccessVisibilityChanged || after != before+1 {
		t.Fatalf("the command's events: %d past %d, the last %s by %s, payload %s; want one project.access_changed by the system actor envoy-dispatch project-visibility, change %s",
			after-before, before, eventType, actorJSON, payloadJSON, model.AccessVisibilityChanged)
	}
	if off, err := access.PrivateOffList(ctx, database.Pool, nil); err != nil || len(off) != 0 {
		t.Fatalf("private projects off an empty list after the command: %v, %v; want none", off, err)
	}
}
