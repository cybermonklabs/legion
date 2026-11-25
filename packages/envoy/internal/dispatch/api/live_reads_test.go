package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// The sessions of TestEveryLiveViewKeepsTheRuleOfItsHTTPRead, by the uuids an agent view names:
// one has acted in PRIV alone, one in PUB alone, and one is on no roster and has done nothing.
const (
	liveReadsPrivSession   = "0b8a3c1e-5f2d-4a7b-9c3e-1d2f3a4b5c61"
	liveReadsPubSession    = "0b8a3c1e-5f2d-4a7b-9c3e-1d2f3a4b5c62"
	liveReadsAbsentSession = "0b8a3c1e-5f2d-4a7b-9c3e-1d2f3a4b5c63"
)

// liveReadsQuiet is how long a view that answers as an absent session must then stay silent.
const liveReadsQuiet = 300 * time.Millisecond

// TestEveryLiveViewKeepsTheRuleOfItsHTTPRead: every view kind /ws/live serves (deliveryLiveKinds
// and the server's liveKinds) names the GET row whose rule it keeps (liveViewReads), and answers
// each caller of verification v16 as that row answers the caller: refused with the row's own code
// where the row refuses, and served where it serves. A Legion board view is of PRIV's board, and a
// Legion issue view of PRIV's issue, so a caller that may not see PRIV is refused each view as its
// GET answers it, as a project or an issue nobody holds. An agent view keeps its row's session
// rule (access.SessionClause, which GET /api/v1/agents lists sessions by) and is narrower still,
// since it relays the session's whole transcript (access.SessionContentClause): of a session the
// caller's roster does not list, it answers exactly what a session nobody answers for answers and
// never tells the session a viewer is attached; of a listed one it relays the session only where
// the content rule passes, and otherwise answers the same as for one nobody answers for. Both
// sessions here are carol's (she proved them) and no listener lists their projects, so only carol
// and carol's token that lists PRIV, open viewers that see every private project carol sees, are
// relayed them.
//
// Control: today's views (no read rule on the live socket). bob's timeline is served where its
// GET row refuses him, and alice's agent view relays PRIV's session, and the test fails naming each.
func TestEveryLiveViewKeepsTheRuleOfItsHTTPRead(t *testing.T) {
	t.Parallel()
	roster := `[
		{"session_id":"` + liveReadsPrivSession + `","title":"Private","capabilities":["btw"],"last_seen":200},
		{"session_id":"` + liveReadsPubSession + `","title":"Public","capabilities":["btw"],"last_seen":100}
	]`
	listener, _ := newBroadcastListener(t, roster)
	source := agentstream.NewMemory()
	for _, session := range []string{liveReadsPrivSession, liveReadsPubSession} {
		source.SetReplay(session, agentstream.Frame(`{"v":1,"kind":"replay","session":"`+session+`"}`))
	}
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, agentStream: source})
	seedHubSettings(t, seed.database)
	// PRIV names a Legion daemon, so its board's GET and view answer a caller that sees PRIV.
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/legion/daemon",
		map[string]any{"state_url": "http://legion-priv.internal:8080"}, http.StatusOK)
	for session, project := range map[string]string{liveReadsPrivSession: seedPrivate, liveReadsPubSession: seedPublic} {
		seed.asSession(t, session, project, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/comments",
			map[string]any{"body": "Done on " + project + "."}, http.StatusCreated)
	}
	url := liveURLOf(t, seed.handler)

	kinds := slices.Concat(slices.Sorted(maps.Keys(deliveryLiveKinds)), slices.Sorted(maps.Keys(seed.server.liveKinds)))
	for _, kind := range kinds {
		route, named := liveViewReads[kind]
		if !named {
			t.Fatalf("the live view kind %q names no GET row whose rule it keeps (liveViewReads)", kind)
		}
		if _, classed := readClasses[route]; !classed || !strings.HasPrefix(route, http.MethodGet+" ") {
			t.Fatalf("the live view kind %q keeps the rule of %q, which is no GET row of the table", kind, route)
		}
	}
	now := time.Now().UTC()
	from, to := now.Add(-7*24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
	// view and target are each kind's view, and the target of the GET row whose rule it keeps,
	// over the same window or session.
	view := func(kind, session string) LiveView {
		switch kind {
		case deliveryLiveTimeline:
			return LiveView{DeliveryLiveView: timelineRange(now.Add(-7*24*time.Hour), now, "")}
		case deliveryLiveMeasures:
			return LiveView{DeliveryLiveView: measuresRange(now.Add(-7*24*time.Hour), now, "")}
		case deliveryLivePipeline:
			return LiveView{DeliveryLiveView: livePipelineView(nil)}
		case liveAgentView:
			return agentView(session)
		case legionBoardView:
			return LiveView{DeliveryLiveView: legionView(seedPrivate)}
		case legionIssueView:
			return LiveView{DeliveryLiveView: legionIssueSubscription(seed.issue[seedPrivate])}
		}
		return eventsView()
	}
	target := func(kind, session string) string {
		route := strings.TrimPrefix(liveViewReads[kind], http.MethodGet+" ")
		route = strings.ReplaceAll(route, "{session_id}", session)
		key := seedPrivate
		if kind == legionIssueView {
			key = seed.issue[seedPrivate]
		}
		route = strings.ReplaceAll(route, "{key}", key)
		if kind == deliveryLiveTimeline || kind == deliveryLiveMeasures {
			route += "?from=" + from + "&to=" + to
		}
		return route
	}
	// first opens a socket as caller, subscribes the view, and answers the first frame it is sent.
	// The events view's first frame, its events_start, stands behind the event horizon, which any
	// transaction on the Postgres instance holds back, so its wait is the horizon's
	// (eventFeedFrame); every other view answers from the server's own reads, bounded by the test
	// binary's deadline.
	first := func(caller privateCaller, kind, session string) (*socketClient, any) {
		t.Helper()
		client := seed.dialLive(t, url, seed.liveHeader(t, caller))
		client.send(t, liveSubscribe("v", view(kind, session), ""))
		what := fmt.Sprintf("%s's %s view", caller, kind)
		if kind == liveEventsView {
			return client, eventFeedFrame(t, client, what)
		}
		return client, client.next(t, what)
	}

	for _, caller := range privateCallers {
		// The agent view of the absent session, and its GET row's status, which the session rule's
		// checks below read again.
		var absentStatus int
		var absent *socketClient
		var absentFrame any
		for _, kind := range kinds {
			answer := seed.request(t, caller, http.MethodGet, target(kind, liveReadsAbsentSession), nil)
			client, frame := first(caller, kind, liveReadsAbsentSession)
			if kind == liveAgentView {
				absentStatus, absent, absentFrame = answer.Code, client, frame
			} else {
				_ = client.conn.Close()
			}
			refusal, refused := frame.(LiveError)
			if answer.Code >= http.StatusBadRequest {
				var body struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(answer.Body.Bytes(), &body)
				if !refused || refusal.Code != body.Code {
					t.Errorf("%s: GET %s answers %d %s, and the %s view answers %+v, want the same refusal", caller, target(kind, liveReadsAbsentSession), answer.Code, body.Code, kind, frame)
				}
				continue
			}
			if refused {
				t.Errorf("%s: GET %s answers %d, and the %s view refuses it: %+v", caller, target(kind, liveReadsAbsentSession), answer.Code, kind, refusal)
			}
		}

		// The agent view's session rule, for a caller its row serves.
		if absent == nil {
			t.Fatalf("%s: no agent view was asked for among the kinds %v", caller, kinds)
		}
		if absentStatus != http.StatusOK {
			continue
		}
		listed := map[string]bool{}
		for _, agent := range decodeBody[[]struct {
			SessionID string `json:"session_id"`
		}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)) {
			listed[agent.SessionID] = true
		}
		absent.quiet(t, liveReadsQuiet, fmt.Sprintf("%s's agent view of a session nobody answers for", caller))
		relayedTo := caller == callerCarol || caller == callerCarolPrivToken
		for _, session := range []string{liveReadsPrivSession, liveReadsPubSession} {
			watched := source.Watches(session)
			client, frame := first(caller, liveAgentView, session)
			got, _ := json.Marshal(frame)
			if listed[session] && relayedTo {
				if agent, ok := frame.(LiveAgentFrame); !ok || string(agent.Data) != `{"v":1,"responding":true}` {
					t.Errorf("%s: GET /api/v1/agents lists %s, carol's, and its agent view answers %s, want the session's responder", caller, session, got)
				}
				continue
			}
			if !listed[session] && relayedTo {
				t.Errorf("%s: GET /api/v1/agents does not list %s, which carol proved", caller, session)
			}
			want, _ := json.Marshal(absentFrame)
			if string(got) != string(want) {
				t.Errorf("%s: its agent view of %s (listed %v) answers %s, want what a session nobody answers for answers, %s", caller, session, listed[session], got, want)
				continue
			}
			client.quiet(t, liveReadsQuiet, fmt.Sprintf("%s's agent view of %s, which it may not watch", caller, session))
			if now := source.Watches(session); now != watched {
				t.Errorf("%s: its agent view of %s, which it may not watch, told the session a viewer is attached (%d times)", caller, session, now-watched)
			}
		}
	}
}
