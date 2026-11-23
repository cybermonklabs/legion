package api

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestM10ARoleOfAHiddenProjectReachesNoSenderWhoCannotSeeIt: role:legion-priv-architect is a role
// of PRIV by its name (contracts.RoleProject), held by session-pub, which has acted in PUB alone and
// so passes access.SessionClause for alice (seedHiddenHolders). A delivery to it resolved under
// alice's reader, who does not see PRIV, answers as role:legion-priv-planner, which no session
// holds - the same refusal, at the same cost in statements - and under carol's, who sees PRIV, it
// reaches session-pub. With no private project (R9) alice's resolve reaches session-pub too, at the
// cost of no statement, as the base resolved it.
//
// Control: roleHolderHidden judging the holder alone; alice's resolve then reaches session-pub and
// the row fails naming it.
func TestM10ARoleOfAHiddenProjectReachesNoSenderWhoCannotSeeIt(t *testing.T) {
	t.Parallel()
	counter := &contextStatements{}
	seed := seedHiddenHolders(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	reader := func(login string) model.Actor {
		t.Helper()
		resolved, err := seed.server.readerFor(t.Context(), model.BrowserActor(login))
		if err != nil {
			t.Fatalf("resolve what %s sees: %v", login, err)
		}
		return resolved
	}
	// resolve is what a delivery to role answers under login's reader, with the role's name
	// blanked, and the statements it made.
	resolve := func(login, role string) (string, int64) {
		t.Helper()
		ctx := context.WithValue(withReader(t.Context(), reader(login)), contextStatementsKey{}, true)
		before := counter.sent.Load()
		session, _, refusal, _ := seed.server.resolveDeliveryTarget(ctx, model.Route{Kind: "role", ID: role}, "btw")
		if refusal != "" {
			session = "refused: " + strings.ReplaceAll(refusal, role, "<role>")
		}
		return session, counter.sent.Load() - before
	}
	const unheldPrivateRole = "legion-priv-planner"

	held, heldCost := resolve(seedAlice, privateProjectRole)
	unheld, unheldCost := resolve(seedAlice, unheldPrivateRole)
	if held != unheld || !strings.HasPrefix(held, "refused: ") {
		t.Errorf("under alice's reader role:%s resolves %q and role:%s, which no session holds, %q: want one refusal", privateProjectRole, held, unheldPrivateRole, unheld)
	}
	if heldCost != unheldCost || heldCost == 0 {
		t.Errorf("under alice's reader role:%s costs %d statements and role:%s %d: want the same, and some", privateProjectRole, heldCost, unheldPrivateRole, unheldCost)
	}
	if got, _ := resolve(seedCarol, privateProjectRole); got != sessionOfPublic {
		t.Errorf("under carol's reader role:%s resolves %q, want %s", privateProjectRole, got, sessionOfPublic)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if got, cost := resolve(seedAlice, privateProjectRole); got != sessionOfPublic || cost != 0 {
		t.Errorf("with no private project, role:%s under alice's reader resolves %q with %d statements, want %s with none, as before", privateProjectRole, got, cost, sessionOfPublic)
	}
}
