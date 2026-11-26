package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAReadWhoseContextLostItsReaderSeesNothing: a read that takes its reader from the request's
// context (readerFromContext) and finds none reads as the closed viewer, which sees nothing. Every
// request's gate puts its reader there (withinProjectScope, withinReadScope), so a context with
// none is a handler that lost it, and reading it as the server's own reader (everyReader) would
// hand that handler everything and say nothing. Work the server does for no request - the
// delivery sweep, a test's direct call - names everyReader itself, and sees what the server sees.
//
// Control: each read answered for a context with no reader as it answers for everyReader, the
// fallback this replaced, under which each "lost" row below sees what the "named" row sees and the
// test fails.
func TestAReadWhoseContextLostItsReaderSeesNothing(t *testing.T) {
	t.Parallel()

	t.Run("an assignee check", func(t *testing.T) {
		database := storetest.Open(t)
		seedPeople(t, database, "alice")
		served := directServer(Deps{Store: database})
		// The check takes its reader from its caller (allowedLogin), and the zero reader is the
		// closed viewer, which sees no person.
		if _, err := served.allowedLogin(context.Background(), model.Actor{}, "alice"); err == nil {
			t.Fatal("an assignee check with no reader allowed alice: want the closed viewer's refusal")
		} else if refusal := (*apiError)(nil); !errors.As(err, &refusal) || refusal.code != "ASSIGNEE_NOT_ALLOWED" {
			t.Fatalf("an assignee check with no reader = %v, want ASSIGNEE_NOT_ALLOWED", err)
		}
		if login, err := served.allowedLogin(context.Background(), everyReader, "alice"); err != nil || login != "alice" {
			t.Fatalf("an assignee check that names everyReader = %q, %v; want alice", login, err)
		}
	})

	t.Run("a session route's resolve", func(t *testing.T) {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/registry/s1" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no live session"}`))
				return
			}
			_, _ = w.Write([]byte(`{"session_id":"s1","title":"worker","state":"` + envoy.StateLive + `","last_seen":42,"capabilities":["steer"]}`))
		}))
		t.Cleanup(listener.Close)
		served := &server{deps: Deps{Envoy: envoy.New(listener.URL)}, lifetime: context.Background()}
		route := model.Route{Kind: "session", ID: "s1"}

		id, _, refusal, hidden := served.resolveDeliveryTarget(context.Background(), route, "steer")
		if refusal != noLiveSessionPrefix+" s1" || !hidden {
			t.Fatalf("a session route resolved with no reader in its context = %q refused %q (hidden %t), want it answered as a session the reader does not see", id, refusal, hidden)
		}
		id, _, refusal, hidden = served.resolveDeliveryTarget(withReader(context.Background(), everyReader), route, "steer")
		if id != "s1" || refusal != "" || hidden {
			t.Fatalf("a session route resolved as everyReader = %q refused %q (hidden %t), want the live session s1", id, refusal, hidden)
		}
	})

	t.Run("a related check's ask states", func(t *testing.T) {
		fixture := newAskStateFixture(t, "LOSTREAD")
		fixture.answer(t, "twice", "alice", map[string]any{"selected": []string{"Brass"}, "text": "The brass hangar."})
		var issueKey string
		if err := fixture.database.Pool.QueryRow(context.Background(),
			`select issue_key from asks where id = $1`, fixture.asks["twice"]).Scan(&issueKey); err != nil {
			t.Fatalf("read the ask's issue: %v", err)
		}
		results := []model.SearchResult{{
			Kind: "ask", ID: fixture.asks["twice"], Owner: model.SearchOwner{Kind: "issue", Key: issueKey, Status: "todo"},
		}}
		fill := func(input relatedInput) *model.SearchAsk {
			suggestions := model.Suggestions{}
			fixture.srv.fillRelatedItems(context.Background(), input, slices.Clone(results), &suggestions)
			if len(suggestions.Related) != 1 {
				t.Fatalf("the related items = %+v, want the one ask", suggestions.Related)
			}
			return suggestions.Related[0].Ask
		}
		if ask := fill(relatedInput{project: "LOSTREAD"}); ask != nil {
			t.Fatalf("a check with no reader read the ask's state %+v, want none: the closed viewer reads no answer", ask)
		}
		if ask := fill(relatedInput{who: everyReader, project: "LOSTREAD"}); ask == nil || ask.State != "answered" || ask.Answer == nil {
			t.Fatalf("a check that names everyReader read the ask's state %+v, want answered with its answer", ask)
		}
	})

	t.Run("a related check's search", func(t *testing.T) {
		fixture := newAskStateFixture(t, "BOUNDREAD")
		check := func(who model.Actor) []model.WriteSuggestion {
			return fixture.srv.relatedCheck(context.Background(), relatedInput{
				who: who, project: "BOUNDREAD", text: "Which lacquer for the dirigible gondolas?",
			}).Related
		}
		closed := model.Actor{Kind: "user", ID: "alice", Viewer: closedViewer()}
		if related := check(closed); len(related) != 0 {
			t.Fatalf("a check whose reader is the closed viewer listed %+v, want nothing: its search binds that reader", related)
		}
		if related := check(everyReader); !slices.ContainsFunc(related, func(item model.WriteSuggestion) bool {
			return item.Kind == "ask" && item.ID == fixture.asks["no-option"]
		}) {
			t.Fatalf("a check that names everyReader listed %+v, want the ask %s", related, fixture.asks["no-option"])
		}
	})
}
