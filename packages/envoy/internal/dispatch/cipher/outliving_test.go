package cipher

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// OutlivingAccess reads agent_tokens, so this package's tests may open a database of their own.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// storePersonalToken stores one personal token of owner, revoked when revoked is set.
func storePersonalToken(t *testing.T, database *store.Store, owner, name string, revoked bool) {
	t.Helper()
	hash := sha256.Sum256([]byte(name))
	if _, err := database.Pool.Exec(context.Background(), `
		insert into agent_tokens (owner, name, token_hash, prefix, revoked_at)
		values ($1, $2, $3, 'test', case when $4::boolean then now() end)`, owner, name, hash[:], revoked); err != nil {
		t.Fatalf("store the personal token %q of %q: %v", name, owner, err)
	}
}

// personAccessStandIn is the broker's GET /v1/person-access, as K8 shapes it: it answers each
// person with what answers names for them, and records each ask's path and bearer.
type personAccessStandIn struct {
	mu    sync.Mutex
	asked []string
	auths []string
}

func (s *personAccessStandIn) serve(t *testing.T, answers map[string]func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/person-access" {
			http.NotFound(w, r)
			return
		}
		answer, found := answers[r.URL.Query().Get("person")]
		if !found {
			t.Errorf("the broker was asked about %q, whom the test gave no answer", r.URL.Query().Get("person"))
			http.Error(w, "no answer", http.StatusTeapot)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// record notes r's method, path and bearer as one ask of the broker.
func (s *personAccessStandIn) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, r.Method+" "+r.URL.RequestURI())
	s.auths = append(s.auths, r.Header.Get("Authorization"))
}

// unanswered stands in for a broker slower than the client's window. It takes the client's
// transport's place rather than a server's, so the ask reaches it before the window can end
// however the machine schedules the two: it records the ask, then holds it, answering nothing,
// until the request's context ends, and returns that context's error, as a transport does for a
// request given up. A caller whose context never ends leaves the client's window as the only
// thing that ends it. A client with no window would be held for ever, so a request that carries
// no deadline is answered at once instead, with person's empty answer, and a test that wants the
// client to give up fails rather than hangs.
func (s *personAccessStandIn) unanswered(person string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		s.record(r)
		if _, windowed := r.Context().Deadline(); !windowed {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"person":"` + person + `","machine_logins":0,"signins":0,"exchange_entries":0,"pod_operator_mappings":0}`)),
				Request:    r,
			}, nil
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
}

// roundTripFunc is a function serving as a client's transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *personAccessStandIn) take() (asked, auths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	asked, auths = s.asked, s.auths
	s.asked, s.auths = nil, nil
	return asked, auths
}

// answerJSON answers status and body as the broker does.
func answerJSON(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// A person holds access that outlives the browser when agent_tokens holds an unrevoked token whose
// owner is them, however its owner is spelled, and the broker is then not asked. Otherwise the
// broker is asked (GET /v1/person-access, under Dispatch's own token), and any of its four counts
// above 0 holds: an account BROKER_POD_OPERATORS maps to the person holds alone. Every failure to
// ask the broker is an error: a 500, a 404 (a broker without the route), a 200 that is not the
// broker's answer for that person, and a broker slower than the client's window. With no broker
// client, agent_tokens alone answers. With no database to read agent_tokens from, the answer is an
// error, and the broker is not asked.
func TestOutlivingAccessCountsTokensThenTheBroker(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	storePersonalToken(t, database, "  Ada@Example.com ", "ada's agent", false)
	storePersonalToken(t, database, "bob@example.com", "bob's revoked agent", true)

	if client := New("http://broker.test", tokenFile(t, "account-token")); client.HTTP.Timeout != 5*time.Second || defaultTimeout != 5*time.Second {
		t.Fatalf("a broker client's window is %s (defaultTimeout %s), want 5s: the sign-out's ask is bounded by it", client.HTTP.Timeout, defaultTimeout)
	}

	standIn := &personAccessStandIn{}
	broker := standIn.serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"bob@example.com":   answerJSON(http.StatusOK, `{"person":"bob@example.com","machine_logins":0,"signins":0,"exchange_entries":0,"pod_operator_mappings":0}`),
		"carol@example.com": answerJSON(http.StatusOK, `{"person":"carol@example.com","machine_logins":1,"signins":0,"exchange_entries":0}`),
		"dave@example.com":  answerJSON(http.StatusOK, `{"person":"dave@example.com","machine_logins":0,"signins":2,"exchange_entries":0}`),
		"erin@example.com":  answerJSON(http.StatusOK, `{"person":"erin@example.com","machine_logins":0,"signins":0,"exchange_entries":1}`),
		"frank@example.com": answerJSON(http.StatusInternalServerError, `{"code":"INTERNAL","error":"boom"}`),
		"gina@example.com":  answerJSON(http.StatusNotFound, `{"code":"NOT_FOUND","error":"not found"}`),
		"hank@example.com":  answerJSON(http.StatusOK, `{"person":"someone@example.com","machine_logins":0,"signins":0,"exchange_entries":0}`),
		"ivan@example.com":  answerJSON(http.StatusOK, `<html>default backend</html>`),
		"kate@example.com":  answerJSON(http.StatusOK, `{"person":"kate@example.com","machine_logins":0,"signins":0,"exchange_entries":0,"pod_operator_mappings":1}`),
	})
	access := OutlivingAccess{Pool: database.Pool, Broker: New(broker.URL, tokenFile(t, "account-token"))}
	slow := New("http://broker.test", tokenFile(t, "account-token"), WithTimeout(time.Millisecond))
	slow.HTTP.Transport = standIn.unanswered("jill@example.com")
	short := OutlivingAccess{Pool: database.Pool, Broker: slow}

	for _, test := range []struct {
		name   string
		access OutlivingAccess
		person string
		holds  bool
		status int // the broker's status an error carries; 0 for an error that is not the broker's
		failed bool
		gaveUp bool   // the error is the client giving up at the end of its window
		asked  string // the ask the broker records; "" for none
	}{
		{name: "an unrevoked personal token, its owner spelled otherwise", access: access, person: "ADA@example.com", holds: true},
		{name: "a revoked personal token and nothing at the broker", access: access, person: "bob@example.com", asked: "bob%40example.com"},
		{name: "a machine login", access: access, person: "carol@example.com", holds: true, asked: "carol%40example.com"},
		{name: "a sign-in on a machine", access: access, person: "dave@example.com", holds: true, asked: "dave%40example.com"},
		{name: "an exchange entry", access: access, person: "erin@example.com", holds: true, asked: "erin%40example.com"},
		{name: "an account BROKER_POD_OPERATORS maps to the person, no login of it live", access: access, person: "kate@example.com", holds: true, asked: "kate%40example.com"},
		{name: "a broker answering 500", access: access, person: "frank@example.com", failed: true, status: http.StatusInternalServerError, asked: "frank%40example.com"},
		{name: "a broker without the route (404)", access: access, person: "gina@example.com", failed: true, status: http.StatusNotFound, asked: "gina%40example.com"},
		{name: "a 200 naming another person", access: access, person: "hank@example.com", failed: true, asked: "hank%40example.com"},
		{name: "a 200 that is no JSON", access: access, person: "ivan@example.com", failed: true, asked: "ivan%40example.com"},
		{name: "a broker slower than the client's window", access: short, person: "jill@example.com", failed: true, gaveUp: true, asked: "jill%40example.com"},
		{name: "no broker client, an unrevoked personal token", access: OutlivingAccess{Pool: database.Pool}, person: "ada@example.com", holds: true},
		{name: "no broker client, only a revoked personal token", access: OutlivingAccess{Pool: database.Pool}, person: "bob@example.com"},
		{name: "no database", access: OutlivingAccess{Broker: access.Broker}, person: "carol@example.com", failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			holds, err := test.access.Holds(ctx, test.person)
			asked, auths := standIn.take()
			if test.failed {
				if err == nil {
					t.Fatalf("Holds(%q) = %v with no error, want an error", test.person, holds)
				}
				if holds {
					t.Errorf("Holds(%q) answered true beside its error %v, want false", test.person, err)
				}
				if !strings.Contains(err.Error(), strings.ToLower(test.person)) {
					t.Errorf("Holds(%q)'s error %q does not name the person", test.person, err)
				}
				var brokerErr *Error
				if test.status != 0 && (!errors.As(err, &brokerErr) || brokerErr.Status != test.status) {
					t.Errorf("Holds(%q)'s error %v is not the broker's *Error with status %d", test.person, err, test.status)
				}
				if test.gaveUp && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Holds(%q)'s error %v is not the client giving up at the end of its window (context.DeadlineExceeded)", test.person, err)
				}
			} else {
				if err != nil {
					t.Fatalf("Holds(%q): %v", test.person, err)
				}
				if holds != test.holds {
					t.Errorf("Holds(%q) = %v, want %v", test.person, holds, test.holds)
				}
			}
			var want []string
			if test.asked != "" {
				want = []string{"GET /v1/person-access?person=" + test.asked}
			}
			if !slices.Equal(asked, want) {
				t.Errorf("Holds(%q) asked the broker %q, want %q", test.person, asked, want)
			}
			for _, auth := range auths {
				if auth != "Bearer account-token" {
					t.Errorf("the broker was asked under Authorization %q, want Dispatch's own token, Bearer account-token", auth)
				}
			}
		})
	}
}
