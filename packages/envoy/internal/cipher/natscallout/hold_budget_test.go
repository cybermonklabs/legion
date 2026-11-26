package natscallout

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Through the service, under the decision's own budget (decideBudget, which the service reads as
// it starts): while Dispatch hangs, a session whose person the broker's person_standing keeps as
// not held is admitted, and one whose person it keeps as held is refused, each decided within the
// budget. The service's introspection asks a real hold gate over a real broker store, with the
// decision's context, as the broker's own introspection does (oauth.Provider's withHold); the
// gate's Dispatch answers nothing until the caller hangs up.
//
// Control: the gate's ask bounded by the caller's deadline alone, as it was before its read
// reserve; the ask then ends with the decision's budget, the kept row is read with a spent context,
// and the session kept not held is refused as a decision past its budget.
func TestAHangingDispatchAdmitsAKeptNotHeldSessionWithinTheDecisionsBudget(t *testing.T) {
	st := storetest.Open(t)
	served, held := newSession(t, "ses_kept_served", 15*time.Minute), newSession(t, "ses_kept_held", 15*time.Minute)
	served.answer.Person, held.answer.Person = "ada@example.com", "bob@example.com"

	// The broker keeps an answer for each person from a Dispatch that answered them: ada not held,
	// bob held, each read back from person_standing as hold's own budget test reads it.
	asOf := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for person, isHeld := range map[string]bool{served.answer.Person: false, held.answer.Person: true} {
		answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"person":%q,"member_kind":"full","held":%t,"as_of":%q}`, person, isHeld, asOf.Format(time.RFC3339Nano))
		}))
		keeper := hold.New(hold.Config{Access: keptBy(st, answering.URL)})
		v := keeper.Decide(t.Context(), person)
		answering.Close()
		want := hold.Served
		if isHeld {
			want = hold.Held
		}
		require.Equal(t, want, v.Kind, "Decide(%s) with Dispatch answering held %t: %v", person, isHeld, v.Err)
		kept, found, err := st.PersonStanding(t.Context(), person)
		require.NoError(t, err, "read person_standing for %s", person)
		require.True(t, found && kept.Held == isHeld && kept.AsOf.Equal(asOf),
			"person_standing for %s = %+v (found %t), want held %t as of %v kept", person, kept, found, isHeld, asOf)
	}

	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hanging.Close)
	t.Cleanup(func() { close(release) })
	gate := hold.New(hold.Config{Access: keptBy(st, hanging.URL)})
	base := introspector(served, held)
	// introspect is base with the gate's verdict mapped onto it by oauth.HoldAnswer, the mapping
	// oauth.Provider's withHold makes: withHold itself is reached only through Provider.Introspect
	// for a token the broker's own exchange stored, and the sessions here are this package's test
	// sessions (introspector), which no broker store holds.
	introspect := func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		answer, err := base(ctx, token)
		if err != nil {
			return answer, err
		}
		return oauth.HoldAnswer(answer, gate.Decide(ctx, answer.Person))
	}

	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	startService(t, url, keys, Deps{Introspect: introspect}, Decide)

	_, err := connectSession(t, url, served, nil)
	require.NoError(t, err, "the hub refused a session whose person the broker keeps as not held, while Dispatch hangs")
	_, err = connectSession(t, url, held, nil)
	require.ErrorIs(t, err, nats.ErrAuthorization, "the hub admitted a session whose person the broker keeps as held, while Dispatch hangs")
}

// keptBy is a client of the Dispatch at url that keeps its answers in st, as cmd/cipherd's does,
// for the hold gate to answer from while that Dispatch cannot be asked.
func keptBy(st *store.Store, url string) *projectaccess.Client {
	client := projectaccess.New(url, projectaccesstest.StaticAccount("token"), nil)
	client.Keep(st, nil, nil)
	return client
}
