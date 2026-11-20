package api

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// requestStatements counts every statement sent on a context that carries requestStatementsKey,
// one by one and in batches alike, so a test counts one request's own statements; visibility counts
// those of them that are the rule of references' read (seenReferencesSQL).
type requestStatements struct{ sent, visibility atomic.Int64 }

type requestStatementsKey struct{}

func (c *requestStatements) count(ctx context.Context, sql string) {
	if ctx.Value(requestStatementsKey{}) == nil {
		return
	}
	c.sent.Add(1)
	if sql == seenReferencesSQL {
		c.visibility.Add(1)
	}
}

func (c *requestStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.count(ctx, data.SQL)
	return ctx
}

func (c *requestStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *requestStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *requestStatements) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	c.count(ctx, data.SQL)
}

func (c *requestStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// rosterCostIssues are PUB issues that differ only in what the reach rows count: none has an
// assignee, a blocker, a child or a claim but where named. unclaimed names nothing; claimed is
// claimed by session-priv; toPrivateRole is routed to role:legion-priv-architect, a role of PRIV;
// toUnheld to role:idle, which no session holds; toHiddenHolder to role:reviewer, which session-priv
// holds. Each carries an open ask of carol's, by key in asks, and a label of its own, by key in
// labels.
type rosterCostIssues struct {
	unclaimed, claimed, toPrivateRole, toUnheld, toHiddenHolder string
	asks, labels                                                map[string]string
}

func seedRosterCostIssues(t *testing.T, seed hiddenHolderSeed) rosterCostIssues {
	t.Helper()
	issues := rosterCostIssues{asks: map[string]string{}, labels: map[string]string{}}
	for index, made := range []struct {
		key   *string
		route string
	}{{&issues.unclaimed, ""}, {&issues.claimed, ""}, {&issues.toPrivateRole, privateProjectRole}, {&issues.toUnheld, unheldRole}, {&issues.toHiddenHolder, hiddenHolderRole}} {
		label := fmt.Sprintf("cost-%d", index)
		*made.key = decodeBody[struct {
			Key string `json:"key"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": seedPublic, "title": fmt.Sprintf("Costed work %d", index), "labels": []string{label}, "force": true,
		}, http.StatusCreated)).Key
		patch := map[string]any{"assignee": nil}
		if made.route != "" {
			patch["route"] = "role:" + made.route
		}
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+*made.key, patch, http.StatusOK)
		issues.labels[*made.key] = label
		issues.asks[*made.key] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+*made.key+"/asks",
			map[string]any{"question": "Who works this?"}))
	}
	seed.asSession(t, sessionOfPrivate, seedPublic, http.MethodPost, "/api/v1/issues/"+issues.claimed+"/claim", map[string]any{}, http.StatusOK)
	return issues
}

// TestM5M6AHiddenClaimOrRoleRouteCostsWhatNoneCosts: the issue read, the listing and the ask read
// judge a claim and a route as their reader reads them before they read the roster, so to carol's
// token that lists no project (which sees PUB and not session-priv) an issue claimed by session-priv
// costs the statements an unclaimed one costs, and one routed to role:legion-priv-architect the
// statements an unrouted one costs; and a role route's holder is judged at a fixed cost, so
// role:reviewer, held by session-priv, costs what role:idle, held by nobody, costs. Each read is
// counted after one read of the same target that warms it, statements sent one by one and in
// batches alike (requestStatements).
//
// Control within the row: role:idle costs more than no route, so the count sees the roster read.
// Controls by hand, one line each: each read's visibility statement skipping a set that names
// nothing (seenReferences for seenReferencesAtFixedCost), and the role holders' statement likewise.
// The order itself - visibility before the roster - is what the base lacks, so the row is red there.
func TestM5M6AHiddenClaimOrRoleRouteCostsWhatNoneCosts(t *testing.T) {
	counter := &requestStatements{}
	seed := seedHiddenHolders(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	issues := seedRosterCostIssues(t, seed)
	costs, visibility := rosterCosts(t, seed, counter, callerCarolToken, issues)
	for _, read := range []string{"issue", "listing", "ask"} {
		cost := costs[read]
		t.Logf("carol's PUB token: %s read costs: none %d, claimed %d, role:%s %d, role:%s %d, role:%s %d (the rule of references' statement: %v)", read, cost[issues.unclaimed], cost[issues.claimed],
			privateProjectRole, cost[issues.toPrivateRole], unheldRole, cost[issues.toUnheld], hiddenHolderRole, cost[issues.toHiddenHolder], visibility[read])
		for _, pair := range []struct{ name, key, like string }{
			{"claimed by session-priv", issues.claimed, issues.unclaimed},
			{"routed to role:" + privateProjectRole, issues.toPrivateRole, issues.unclaimed},
			{"routed to role:" + hiddenHolderRole + ", held by session-priv", issues.toHiddenHolder, issues.toUnheld},
		} {
			if cost[pair.key] != cost[pair.like] {
				t.Errorf("carol's PUB token: the %s read of the issue %s costs %d statements, and of %s %d: want the same",
					read, pair.name, cost[pair.key], pair.like, cost[pair.like])
			}
		}
		if cost[issues.toUnheld] <= cost[issues.unclaimed] {
			t.Errorf("carol's PUB token: the %s read of role:%s costs %d statements and of no route %d, so the count does not see the roster read",
				read, unheldRole, cost[issues.toUnheld], cost[issues.unclaimed])
		}
	}
}

// TestM5M6WithNoPrivateProjectTheReachReadsCostAsBefore (R9): with PRIV public alice, a full member,
// reads every reach as the base read it: a session's claim and each route read the roster, one
// statement, and nothing reads what she sees, so each costs one statement more than the issue that
// names nothing, role:reviewer what role:idle costs, and no read makes the rule of references'
// statement (seenReferencesSQL). Every count is logged, for the report's comparison of the two
// commits.
//
// Control: the issue read's visibility read made for a reader that sees every object. Two guards
// stand in its way (leaveOutHiddenIssueReferences' first test and seenReferencesAtFixedCost's), so
// the control drops both; alice's issue reads then make the rule of references' statement and the
// row fails naming each. Dropping one alone leaves the row green, the other guard holding.
func TestM5M6WithNoPrivateProjectTheReachReadsCostAsBefore(t *testing.T) {
	counter := &requestStatements{}
	seed := seedHiddenHolders(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	issues := seedRosterCostIssues(t, seed)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	costs, visibility := rosterCosts(t, seed, counter, callerAlice, issues)
	for _, read := range []string{"issue", "listing", "ask"} {
		cost := costs[read]
		t.Logf("R9 %s read costs: none %d, claimed %d, role:%s %d, role:%s %d, role:%s %d", read, cost[issues.unclaimed], cost[issues.claimed],
			privateProjectRole, cost[issues.toPrivateRole], unheldRole, cost[issues.toUnheld], hiddenHolderRole, cost[issues.toHiddenHolder])
		for name, key := range map[string]string{"role:" + privateProjectRole: issues.toPrivateRole, "role:" + unheldRole: issues.toUnheld, "role:" + hiddenHolderRole: issues.toHiddenHolder} {
			if cost[key] != cost[issues.unclaimed]+1 {
				t.Errorf("PRIV public: alice's %s read of %s costs %d statements, want %d, the roster's one more than no route", read, name, cost[key], cost[issues.unclaimed]+1)
			}
		}
		for key, made := range visibility[read] {
			if made != 0 {
				t.Errorf("PRIV public: alice's %s read of %s makes the rule of references' statement %d times, want none: she sees every object", read, key, made)
			}
		}
	}
	if cost := costs["issue"]; cost[issues.claimed] != cost[issues.unclaimed]+1 {
		t.Errorf("PRIV public: alice's issue read of the claimed issue costs %d statements, want %d, the roster's one more than none", cost[issues.claimed], cost[issues.unclaimed]+1)
	}
}

// rosterCosts counts, as caller, the statements of each issue's read, its one-row listing (by its
// label) and its ask's read, after one warming read of each, by read and issue key, and how many of
// them are the rule of references' statement.
func rosterCosts(t *testing.T, seed hiddenHolderSeed, counter *requestStatements, caller privateCaller, issues rosterCostIssues) (costs, visibility map[string]map[string]int64) {
	t.Helper()
	costs = map[string]map[string]int64{"issue": {}, "listing": {}, "ask": {}}
	visibility = map[string]map[string]int64{"issue": {}, "listing": {}, "ask": {}}
	ctx := context.WithValue(t.Context(), requestStatementsKey{}, true)
	for _, key := range []string{issues.unclaimed, issues.claimed, issues.toPrivateRole, issues.toUnheld, issues.toHiddenHolder} {
		for read, target := range map[string]string{
			"issue":   "/api/v1/issues/" + key,
			"listing": "/api/v1/issues?project=" + seedPublic + "&label=" + issues.labels[key],
			"ask":     "/api/v1/asks/" + issues.asks[key],
		} {
			seed.mustRequest(t, caller, http.MethodGet, target, nil, http.StatusOK)
			before, seenBefore := counter.sent.Load(), counter.visibility.Load()
			if response := seed.requestIn(t, ctx, caller, http.MethodGet, target, nil); response.Code != http.StatusOK {
				t.Fatalf("%s: GET %s = %d %s", caller, target, response.Code, response.Body.String())
			}
			costs[read][key] = counter.sent.Load() - before
			visibility[read][key] = counter.visibility.Load() - seenBefore
		}
	}
	return costs, visibility
}
