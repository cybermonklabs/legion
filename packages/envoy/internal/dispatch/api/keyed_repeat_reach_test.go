package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// carolsSession is the session a token of carol's names in a body, as a bearer caller must
// (requireActor); a body carol sends at her browser carries it too, which the browser ignores, so
// both send the same bytes under one key.
var carolsSession = map[string]string{"kind": "session", "id": "session-of-carols-token"}

// TestAKeyedCreateIsPlacedByTheProjectItsHandlerReads: a repeat of a create is placed by the
// project its body names, read as the create's own handler reads it - decoded into the handler's
// input, where encoding/json matches a member's name in any case and the last of two wins, and
// trimmed (createIssueInput.bodyProject). carol creates an issue of PRIV at her browser under a key
// with each of three bodies that name PRIV only that way; her token that lists no project, which
// does not see PRIV, retries each under its key and is refused 409 KEY_COMMITTED, holding none of
// the first answer, and each create made one issue.
//
// Control: the project read from the body by its exact member name, untrimmed, as it was, under
// which each retry reads a project that is not PRIV and is replayed the first 201.
func TestAKeyedCreateIsPlacedByTheProjectItsHandlerReads(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	for _, body := range []struct{ name, json string }{
		{"a padded project", `{"project":" PRIV ","title":"Padded project under a key","force":true,"actor":{"kind":"session","id":"session-of-carols-token"}}`},
		{"a project member in another case", `{"Project":"PRIV","title":"Capitalised project under a key","force":true,"actor":{"kind":"session","id":"session-of-carols-token"}}`},
		{"two project members", `{"project":"PUB","Project":"PRIV","title":"Two project members under a key","force":true,"actor":{"kind":"session","id":"session-of-carols-token"}}`},
	} {
		t.Run(body.name, func(t *testing.T) {
			key := "create-" + strings.ReplaceAll(body.name, " ", "-")
			first := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, "/api/v1/issues", json.RawMessage(body.json), key)
			created := decodeBody[struct {
				Key     string `json:"key"`
				Project string `json:"project"`
				Title   string `json:"title"`
			}](t, first)
			if first.Code != http.StatusCreated || created.Project != seedPrivate {
				t.Fatalf("carol's keyed create = %d %s, want 201 with an issue of %s: the case proves nothing", first.Code, first.Body.String(), seedPrivate)
			}
			wantKeyedRefusal(t, "the retry by carol's token, which does not see "+seedPrivate,
				keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/issues", json.RawMessage(body.json), key), created.Key)
			var made int
			if err := seed.database.Pool.QueryRow(t.Context(), `select count(*) from issues where title = $1`, created.Title).Scan(&made); err != nil || made != 1 {
				t.Errorf("issues titled %q = %d (%v), want the one the first request made", created.Title, made, err)
			}
		})
	}
}

// TestAKeyedRefusalNamesNothingItsCallerDoesNotSee: a key belongs to a person whatever credential
// they write through (requestKeyLogin), so the refusals the keyed layer answers from what the key
// holds name the first request's object only to a caller that sees it.
//
//   - 422 KEY_REUSED names the first request's method and path to carol at her browser, who sees
//     PRIV's issue, and names neither to her token that lists no project, which does not.
//   - 409 KEY_IN_FLIGHT on a status change hands the running request's recovery point, which names
//     every issue its steps changed, to carol at her browser, and an empty one to her token.
//
// Control: the refusals answered as they were, naming the path and handing the point to every
// credential of the person, under which the token's two refusals name PRIV's issue.
func TestAKeyedRefusalNamesNothingItsCallerDoesNotSee(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	issue := seed.issue[seedPrivate]
	path := "/api/v1/issues/" + issue

	t.Run("a reused key", func(t *testing.T) {
		first := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, path, map[string]any{"title": "Renamed under a key", "actor": carolsSession}, "reused")
		if first.Code != http.StatusOK {
			t.Fatalf("carol's keyed rename of %s = %d %s, want 200", issue, first.Code, first.Body.String())
		}
		reused := func(caller privateCaller) (string, string) {
			response := keyedSeedRequest(t, seed, caller, http.MethodPatch, path, map[string]any{"title": "Another name", "actor": carolsSession}, "reused")
			code, message := refusalOf(t, response)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s's other request under the key = %d %s, want 422", caller, response.Code, response.Body.String())
			}
			return code, message
		}
		if code, message := reused(callerCarol); code != contracts.IdempotencyKeyReused || !strings.Contains(message, http.MethodPatch+" "+path) {
			t.Fatalf("carol's reuse answered %s %q, want %s naming %s %s: the comparison proves nothing", code, message, contracts.IdempotencyKeyReused, http.MethodPatch, path)
		}
		if code, message := reused(callerCarolToken); code != contracts.IdempotencyKeyReused || strings.Contains(message, issue) || strings.Contains(message, http.MethodPatch) {
			t.Errorf("the token's reuse answered %s %q, want %s naming neither the method nor the path", code, message, contracts.IdempotencyKeyReused)
		}
	})

	t.Run("a status change in flight", func(t *testing.T) {
		change := map[string]any{"status": "backlog", "actor": carolsSession}
		first := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, path+"/status-change", change, "in-flight")
		if first.Code != http.StatusOK {
			t.Fatalf("carol's keyed status change of %s = %d %s, want 200", issue, first.Code, first.Body.String())
		}
		// The first request still runs: its answer not kept, its lease held, its point left.
		if tag, err := seed.database.Pool.Exec(t.Context(), `
			update request_keys set status = null, headers = null, response = null, leased_at = now()
			where idempotency_key = 'in-flight'`); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("hold the status change in flight: %v (%d rows)", err, tag.RowsAffected())
		}
		changed := func(caller privateCaller) []string {
			response := keyedSeedRequest(t, seed, caller, http.MethodPost, path+"/status-change", change, "in-flight")
			refusal := decodeBody[struct {
				Code    string `json:"code"`
				Changed []struct {
					Key string `json:"key"`
				} `json:"changed"`
			}](t, response)
			if response.Code != http.StatusConflict || refusal.Code != contracts.IdempotencyKeyInFlight {
				t.Fatalf("%s's repeat while the change runs = %d %s, want 409 %s", caller, response.Code, response.Body.String(), contracts.IdempotencyKeyInFlight)
			}
			keys := make([]string, len(refusal.Changed))
			for index, entry := range refusal.Changed {
				keys[index] = entry.Key
			}
			return keys
		}
		if keys := changed(callerCarol); !slices.Contains(keys, issue) {
			t.Fatalf("carol's repeat was handed the point %v, want it naming %s: the comparison proves nothing", keys, issue)
		}
		if keys := changed(callerCarolToken); len(keys) != 0 {
			t.Errorf("the token's repeat was handed the point %v, want none", keys)
		}
	})
}

// TestAHiddenKeyedRepeatIsRefusedAndWritesOnce: at this slice the issue's loaders bind no reader
// (loadIssue), so a request run again for a credential that does not see its issue would find the
// issue and write it a second time. A keyed repeat whose caller does not see the issue its address
// names is refused 409 KEY_COMMITTED and never run: carol renames PRIV's issue at her browser under
// a key, her token that lists no project sends the same rename under the same key, and is refused,
// holding none of the first answer; the issue carries the first rename's one event.
//
// Control: the hidden repeat run again under no key, as it was, under which the token is answered
// a fresh 200 and the issue carries two renames.
func TestAHiddenKeyedRepeatIsRefusedAndWritesOnce(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	issue := seed.issue[seedPrivate]
	rename := map[string]any{"title": "Renamed once under a key", "actor": carolsSession}
	renames := func() int {
		t.Helper()
		var count int
		if err := seed.database.Pool.QueryRow(t.Context(),
			`select count(*) from events where issue_key = $1 and type = 'issue.updated'`, issue).Scan(&count); err != nil {
			t.Fatalf("count the renames of %s: %v", issue, err)
		}
		return count
	}
	first := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, "/api/v1/issues/"+issue, rename, "rename-once")
	if first.Code != http.StatusOK {
		t.Fatalf("carol's keyed rename = %d %s, want 200", first.Code, first.Body.String())
	}
	before := renames()
	wantKeyedRefusal(t, "the token's repeat of the rename",
		keyedSeedRequest(t, seed, callerCarolToken, http.MethodPatch, "/api/v1/issues/"+issue, rename, "rename-once"), `"title":"Renamed once under a key"`)
	if after := renames(); after != before {
		t.Errorf("the issue carries %d renames after the token's repeat, want the %d it carried: the repeat ran again", after, before)
	}
}
