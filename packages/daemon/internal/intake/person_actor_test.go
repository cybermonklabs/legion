package intake

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A route's own status write says who asked for it (Requester), and the person who asked through
// the operator's grant (PersonActor), on the event OwnStatusWrite makes. Both reach the handlers
// when the fact api's recordOwnStatusWrite builds applies to a recorded issue, where the event the
// view was prepared in place of is what applies.
func TestAnOwnWriteSaysWhoAsked(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: "ACME-1", Tree: "ACME-1", Project: "ACME", Title: "ACME-1", Phase: phase.Done,
			Generation: 1, Status: "done", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	}); err != nil {
		t.Fatalf("record ACME-1: %v", err)
	}
	var seen []DispatchIssue
	recorder := handlerFunc(func(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
		seen = append(seen, fact.(DispatchIssue))
		return Result{}, nil
	})
	for i, tc := range []struct {
		name      string
		requester claim.Role
		person    string
	}{
		{"the controller's set_status", claim.RoleController, ""},
		{"an architect's rerun_child", claim.RoleArchitect, ""},
		{"the operator's legion issue status", claim.Role("person"), "operator"},
	} {
		written := dispatch.Issue{Key: "ACME-1", Title: "ACME-1", Status: "todo", Rank: "A", HandedOver: true, LastSeq: int64(2 + i), ResolvedRepo: "acme/widgets"}
		event := OwnStatusWrite(written, tc.requester, tc.person)
		if event.Requester != tc.requester || event.PersonActor != tc.person || !event.DaemonActor || !event.StatusMove {
			t.Fatalf("%s's event = %+v, want Requester %q and PersonActor %q on the daemon's own status move", tc.name, event, tc.requester, tc.person)
		}
		view := ObservedIssue(written)
		view.Delivered = &event
		seen = nil
		if _, err := ApplyFact(ctx, pool, "api", "own-write-"+string(rune('a'+i)), view, recorder); err != nil {
			t.Fatalf("apply %s: %v", tc.name, err)
		}
		if len(seen) != 1 || seen[0].Requester != tc.requester || seen[0].PersonActor != tc.person {
			t.Fatalf("the handlers saw %+v for %s, want one fact with Requester %q and PersonActor %q", seen, tc.name, tc.requester, tc.person)
		}
	}
}

// A decoded Dispatch issue event names the person who wrote it (PersonActor) only where its actor
// is a user with an id: a session, Dispatch's own system actor, a user with no id and an event
// with no actor name none. Each shape is a captured event (testdata/dispatch) with its actor
// replaced, and each kind of issue event is read: an update, a creation and a close.
func TestADecodedIssuePreservesItsPersonActor(t *testing.T) {
	t.Parallel()
	const captured = `"actor":{"kind":"user","id":"smoke"}`
	for _, file := range []string{"issue-updated.json", "issue-created.json", "issue-closed.json"} {
		data, err := os.ReadFile("testdata/dispatch/" + file)
		if err != nil {
			t.Fatalf("read the captured %s: %v", file, err)
		}
		var envelope map[string]any
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatalf("decode the captured %s: %v", file, err)
		}
		payload, subject := envelope["payload"].(string), envelope["topic"].(string)
		if !strings.Contains(payload, captured) {
			t.Fatalf("the captured %s has no %s", file, captured)
		}
		for _, tc := range []struct {
			name, actor, want string
		}{
			{"a user", captured, "smoke"},
			{"another user", `"actor":{"kind":"user","id":"alice"}`, "alice"},
			{"a session", `"actor":{"kind":"session","id":"ses-impl"}`, ""},
			{"the daemon's own session", `"actor":{"kind":"session","id":"legion-daemon:CAPTURE"}`, ""},
			{"Dispatch's system actor", `"actor":{"kind":"system","id":"dispatch"}`, ""},
			{"a user with no id", `"actor":{"kind":"user","id":""}`, ""},
			{"no actor", `"actor":null`, ""},
		} {
			t.Run(file+"/"+tc.name, func(t *testing.T) {
				shaped := map[string]any{}
				for key, value := range envelope {
					shaped[key] = value
				}
				shaped["payload"] = strings.Replace(payload, captured, tc.actor, 1)
				body, err := json.Marshal(shaped)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, body)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				issue, ok := decoded.Fact.(DispatchIssue)
				if !ok {
					t.Fatalf("fact = %#v, want an issue fact", decoded.Fact)
				}
				if issue.PersonActor != tc.want {
					t.Fatalf("PersonActor = %q for %s, want %q", issue.PersonActor, tc.actor, tc.want)
				}
			})
		}
	}
}
