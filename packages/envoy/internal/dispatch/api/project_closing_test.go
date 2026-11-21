package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// closingRig is a test server whose deployment provisions listed as private and whose hubs admit
// Dispatch, holding PRIV and PUB, both alice's, with the store's backfills done, and, with a
// broker, the stand-in (brokerStandIn) as its secrets broker. Its membership answer is read as the
// listed service account.
type closingRig struct {
	handler  http.Handler
	database *store.Store
	broker   *brokerStandIn
	// answerBody is the membership answer of a project as its bytes.
	answerBody func(t *testing.T, key string) string
}

// answer is the membership answer of key, decoded.
func (rig closingRig) answer(t *testing.T, key string) model.AccessAnswer {
	t.Helper()
	var answer model.AccessAnswer
	if err := json.Unmarshal([]byte(rig.answerBody(t, key)), &answer); err != nil {
		t.Fatalf("decode the membership answer of %s: %v", key, err)
	}
	return answer
}

func newClosingRig(t *testing.T, listed []string, withBroker bool) closingRig {
	t.Helper()
	issuer := newServiceTokenIssuer(t)
	options := testServerOptions{
		store: storetest.Open(t), privateProjects: listed, hubAdmission: acceptingAdmission{},
		oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}, noAgentSecrets: !withBroker,
	}
	if withBroker {
		options.agentSecrets = newBrokerStandIn()
	}
	handler, database, _ := newTestServer(t, options)
	rig := closingRig{handler: handler, database: database, broker: options.agentSecrets}
	for _, key := range []string{"PRIV", "PUB"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", key, response.Code, response.Body.String())
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into session_projects_filled default values;
		insert into access_events_scrubbed default values;
	`); err != nil {
		t.Fatalf("fill the store's markers: %v", err)
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	rig.answerBody = func(t *testing.T, key string) string {
		t.Helper()
		response := agentRequest(t, handler, http.MethodGet, "/api/v1/access/projects/"+key, nil, bearer)
		if response.Code != http.StatusOK {
			t.Fatalf("the membership answer of %s = %d %s", key, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	return rig
}

// switchPrivate is the owner alice's switch of key to private.
func (rig closingRig) switchPrivate(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/projects/"+key+"/access", map[string]string{"visibility": "private"}, "alice")
}

// closingSince is key's closing_since, nil while it is in no closing state.
func (rig closingRig) closingSince(t *testing.T, key string) *time.Time {
	t.Helper()
	var since *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select closing_since from projects where key = $1`, key).Scan(&since); err != nil {
		t.Fatalf("read %s's closing_since: %v", key, err)
	}
	return since
}

// mint is alice's personal token listing projects, minted through the route; it answers the
// route's response.
func (rig closingRig) mint(t *testing.T, name string, projects []string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody(name, projects), "alice")
}

// whoami is the status of GET /api/v1/whoami presenting token.
func (rig closingRig) whoami(t *testing.T, token string) int {
	t.Helper()
	return agentRequest(t, rig.handler, http.MethodGet, "/api/v1/whoami", nil, token).Code
}

// lowerSettle sets developerBoxSettle to settle until the test ends.
func lowerSettle(t *testing.T, settle time.Duration) {
	t.Helper()
	previous := developerBoxSettle
	developerBoxSettle = settle
	t.Cleanup(func() { developerBoxSettle = previous })
}

// refusedWith fails the test unless response is status with code and a message naming each of
// naming.
func refusedWith(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string, naming ...string) {
	t.Helper()
	body := response.Body.String()
	if response.Code != status || !strings.Contains(body, `"code":"`+code+`"`) {
		t.Fatalf("%s = %d %s, want %d %s", what, response.Code, body, status, code)
	}
	for _, name := range naming {
		if !strings.Contains(body, name) {
			t.Fatalf("%s = %s, want a message naming %q", what, body, name)
		}
	}
}

// fullSwitchAt is key's full_switch_at, nil while no full switch is recorded.
func (rig closingRig) fullSwitchAt(t *testing.T, key string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select full_switch_at from projects where key = $1`, key).Scan(&at); err != nil {
		t.Fatalf("read %s's full_switch_at: %v", key, err)
	}
	return at
}

// makeDayOnePrivate makes key private as a project is on its first day, before its full switch:
// private, with no full switch recorded and no closing state.
func (rig closingRig) makeDayOnePrivate(t *testing.T, key string) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, key); err != nil {
		t.Fatalf("make %s private on its first day: %v", key, err)
	}
}

// TestAProjectBecomesPrivateOnlyWithNoDeveloperBoxTokenLive: the owner's switch of PRIV to private
// starts its closing state and answers 409 DEVELOPER_BOX_TOKENS_LIVE naming the broker's count,
// the closing state committed and the membership answer closed to developer boxes, PRIV still
// public. It answers so while the broker counts any token live, and while the closing state is
// younger than developerBoxSettle with none live; with none live and the state that old, the
// switch completes, records the full switch and ends the closing state, and the answer stays
// closed. A broker that does not answer leaves the switch 503 DEVELOPER_BOX_TOKENS_UNAVAILABLE and
// the closing state in place; the owner's switch to public ends it. A project private on its first
// day, closed to developer boxes once its key is on DISPATCH_PRIVATE_PROJECTS, goes through the same
// closing state at its owner's switch. A deployment with no secrets broker refuses the switch 409
// PRIVATE_PROJECTS_NEED_BROKER with nothing changed: no closing state, no token revoked.
func TestAProjectBecomesPrivateOnlyWithNoDeveloperBoxTokenLive(t *testing.T) {
	t.Run("with a broker", func(t *testing.T) {
		rig := newClosingRig(t, []string{"PRIV", "PUB"}, true)
		lowerSettle(t, time.Hour)
		rig.broker.set("PRIV", 2, false)
		refusedWith(t, "the first switch of PRIV", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE",
			"PRIV is closing since", "holds 2 live developer-box tokens")
		if rig.closingSince(t, "PRIV") == nil || visibilityOf(t, rig.database, "PRIV") != model.ProjectPublic || !rig.answer(t, "PRIV").ClosedToDeveloperBoxes {
			t.Fatalf("after the refused switch PRIV is %s with closing_since %v and answer %+v, want public, closing and closed to developer boxes",
				visibilityOf(t, rig.database, "PRIV"), rig.closingSince(t, "PRIV"), rig.answer(t, "PRIV"))
		}
		lowerSettle(t, 0)
		refusedWith(t, "a switch while 2 tokens are live", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE", "holds 2 live")
		rig.broker.set("PRIV", 0, false)
		lowerSettle(t, time.Hour)
		refusedWith(t, "a switch with none live and the closing state younger than the settle time", rig.switchPrivate(t, "PRIV"),
			http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE", "holds 0 live", "no sooner than")
		rig.broker.set("PRIV", 0, true)
		refusedWith(t, "a switch the broker does not count", rig.switchPrivate(t, "PRIV"), http.StatusServiceUnavailable, "DEVELOPER_BOX_TOKENS_UNAVAILABLE", "PRIV stays closing")
		if rig.fullSwitchAt(t, "PRIV") != nil {
			t.Fatalf("a refused switch recorded PRIV's full switch")
		}
		rig.broker.set("PRIV", 0, false)
		lowerSettle(t, 0)
		if made := rig.switchPrivate(t, "PRIV"); made.Code != http.StatusOK || visibilityOf(t, rig.database, "PRIV") != model.ProjectPrivate ||
			rig.closingSince(t, "PRIV") != nil || rig.fullSwitchAt(t, "PRIV") == nil || !rig.answer(t, "PRIV").ClosedToDeveloperBoxes {
			t.Fatalf("the switch with none live and the state settled = %d %s, PRIV %s closing %v full switch %v; want 200, private, the full switch recorded, the closing state ended and the answer closed",
				made.Code, made.Body.String(), visibilityOf(t, rig.database, "PRIV"), rig.closingSince(t, "PRIV"), rig.fullSwitchAt(t, "PRIV"))
		}

		refusedWith(t, "the first switch of PUB", rig.switchPrivate(t, "PUB"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
		if response := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/projects/PUB/access", map[string]string{"visibility": "public"}, "alice"); response.Code != http.StatusOK ||
			rig.closingSince(t, "PUB") != nil || rig.answer(t, "PUB").ClosedToDeveloperBoxes {
			t.Fatalf("the owner's switch of the closing PUB to public = %d %s, closing %v; want 200 and its closing state ended", response.Code, response.Body.String(), rig.closingSince(t, "PUB"))
		}
	})

	t.Run("a project private on its first day", func(t *testing.T) {
		rig := newClosingRig(t, []string{"PRIV"}, true)
		lowerSettle(t, 0)
		rig.makeDayOnePrivate(t, "PRIV")
		if !rig.answer(t, "PRIV").ClosedToDeveloperBoxes {
			t.Fatalf("PRIV, private on its first day with its key on DISPATCH_PRIVATE_PROJECTS, answered open before its owner's switch")
		}
		refusedWith(t, "the owner's switch of the day-one PRIV", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE", "PRIV is closing since")
		if !rig.answer(t, "PRIV").ClosedToDeveloperBoxes {
			t.Fatalf("PRIV answered open to developer boxes in its closing state")
		}
		if made := rig.switchPrivate(t, "PRIV"); made.Code != http.StatusOK || rig.closingSince(t, "PRIV") != nil || rig.fullSwitchAt(t, "PRIV") == nil {
			t.Fatalf("the owner's next switch = %d %s, closing %v; want 200, the closing state ended and the full switch recorded", made.Code, made.Body.String(), rig.closingSince(t, "PRIV"))
		}
	})
}

// TestASwitchToPrivateNeedsTheSecretsBroker: a deployment with no secrets broker
// (DISPATCH_AGENT_SECRETS_URL unset) refuses the owner's full switch of PRIV 409
// PRIVATE_PROJECTS_NEED_BROKER, naming the setting, with nothing changed: PRIV public, no closing
// state, no event, and PRIV's personal token still reads.
func TestASwitchToPrivateNeedsTheSecretsBroker(t *testing.T) {
	rig := newClosingRig(t, []string{"PRIV"}, false)
	token := decodeBody[agentTokenResponse](t, rig.mint(t, "of PRIV", []string{"PRIV"}))
	before := allEvents(t, rig.database)
	refusedWith(t, "the switch with no broker", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "PRIVATE_PROJECTS_NEED_BROKER", "DISPATCH_AGENT_SECRETS_URL")
	if visibilityOf(t, rig.database, "PRIV") != model.ProjectPublic || rig.closingSince(t, "PRIV") != nil || allEvents(t, rig.database) != before || rig.whoami(t, token.Token) != http.StatusOK {
		t.Fatalf("the refused switch of PRIV with no broker changed something: PRIV %s, closing %v, events %d → %d, PRIV's token reads whoami %d",
			visibilityOf(t, rig.database, "PRIV"), rig.closingSince(t, "PRIV"), before, allEvents(t, rig.database), rig.whoami(t, token.Token))
	}
}

// brokerForgetsOf is the forgets owed the broker for key, as (record or 0) in the order written.
func brokerForgetsOf(t *testing.T, database *store.Store, key string) []int64 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select coalesce(record, 0) from broker_forgets where project = $1 order by id`, key)
	if err != nil {
		t.Fatalf("read the forgets owed for %s: %v", key, err)
	}
	records, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("read the forgets owed for %s: %v", key, err)
	}
	return records
}

// TestTheSwitchSendsItsChannelsAndDeadlineToTheBrokersCount: every count a switch to private asks
// sends deadline_seconds 90 and the Slack channels the project will hold once the switch commits:
// the stored ones, none as an empty list, and the body's, checked and sorted as their write keeps
// them, when the PATCH names them. A body naming a channel that is no channel id is refused 400
// INVALID_SLACK_CHANNEL before anything is written or counted.
func TestTheSwitchSendsItsChannelsAndDeadlineToTheBrokersCount(t *testing.T) {
	rig := newClosingRig(t, []string{"PRIV", "PUB"}, true)
	lowerSettle(t, 0)
	if _, err := rig.database.Pool.Exec(context.Background(), `update projects set slack_channels = '{C0STORED001}' where key = 'PRIV'`); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, "the first switch of PRIV", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
	refusedWith(t, "the first switch of PUB", rig.switchPrivate(t, "PUB"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
	refusedWith(t, "a switch of PRIV naming no channel id", dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
		map[string]any{"visibility": "private", "slack_channels": []string{"general"}}, "alice"), http.StatusBadRequest, "INVALID_SLACK_CHANNEL")
	if made := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
		map[string]any{"visibility": "private", "slack_channels": []string{" C0BODY00002", "C0BODY00001", "C0BODY00002"}}, "alice"); made.Code != http.StatusOK {
		t.Fatalf("the switch of PRIV naming its channels = %d %s, want 200", made.Code, made.Body.String())
	}
	counts, _ := rig.broker.answered()
	want := []standInCount{
		{Project: "PRIV", Channels: []string{"C0STORED001"}, Deadline: "90", Record: 1},
		{Project: "PUB", Channels: []string{}, Deadline: "90", Record: 2},
		{Project: "PRIV", Channels: []string{"C0BODY00001", "C0BODY00002"}, Deadline: "90", Record: 3},
	}
	if len(counts) != len(want) {
		t.Fatalf("the broker answered %d counts %+v, want %+v: the refused body asks none", len(counts), counts, want)
	}
	for i := range want {
		if counts[i].Project != want[i].Project || counts[i].Deadline != want[i].Deadline || counts[i].Record != want[i].Record ||
			strings.Join(counts[i].Channels, ",") != strings.Join(want[i].Channels, ",") || counts[i].Channels == nil {
			t.Errorf("count %d = %+v, want %+v", i, counts[i], want[i])
		}
	}
}

// switchableAdmission is a private-hub admission proof (HubAdmission) a test turns: it answers err,
// or, while block is set, waits for the switch's context to end and answers its error.
type switchableAdmission struct {
	mu    sync.Mutex
	err   error
	block bool
}

func (a *switchableAdmission) Prove(ctx context.Context) error {
	a.mu.Lock()
	err, block := a.err, a.block
	a.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (a *switchableAdmission) setProveErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

func (a *switchableAdmission) setBlock(block bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.block = block
}

// forgetRig is a test server whose deployment provisions PRIV, alice's, proves the hubs' admission
// through admission and counts through broker, with the store's backfills done and the closing
// state's settle time lowered to 0; switchPRIV is alice's switch of PRIV to private.
type forgetRig struct {
	handler    http.Handler
	database   *store.Store
	admission  *switchableAdmission
	broker     *brokerStandIn
	switchPRIV func() *httptest.ResponseRecorder
}

func newForgetRig(t *testing.T) forgetRig {
	t.Helper()
	rig := forgetRig{admission: &switchableAdmission{}, broker: newBrokerStandIn()}
	issuer := newServiceTokenIssuer(t)
	rig.handler, rig.database, _ = newTestServer(t, testServerOptions{
		store: storetest.Open(t), privateProjects: []string{"PRIV"}, hubAdmission: rig.admission, agentSecrets: rig.broker,
		oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount},
	})
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "PRIV", "name": "PRIV"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create PRIV: %d %s", response.Code, response.Body.String())
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `insert into session_projects_filled default values; insert into access_events_scrubbed default values`); err != nil {
		t.Fatal(err)
	}
	lowerSettle(t, 0)
	rig.switchPRIV = func() *httptest.ResponseRecorder {
		return dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/projects/PRIV/access", map[string]string{"visibility": "private"}, "alice")
	}
	return rig
}

// TestASwitchThatDoesNotMakeAProjectPrivateForgetsItAtTheBroker: a switch the closing step refuses
// after the broker's count answered record N owes the forget of N, written with the closing state
// it keeps; one that fails later in its transaction, here at the hubs' admission proof, owes the
// forget of N too, written once the transaction rolled back. A switch whose count the broker did
// not answer owes nothing, as no record is known, and a switch that commits owes nothing for its
// own record. The owner's switch of the private project back to public owes the forget of the
// project's record whatever its number. The sender then delivers each forget owed to the broker
// stand-in, in order, and keeps none. A forget the broker answers 503, then 409 RECORD_IN_FLIGHT,
// then 200 is sent three times and kept until the third.
func TestASwitchThatDoesNotMakeAProjectPrivateForgetsItAtTheBroker(t *testing.T) {
	rig := newForgetRig(t)
	ctx := context.Background()
	refusedWith(t, "the first switch, which starts the closing state", rig.switchPRIV(), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
	if got := brokerForgetsOf(t, rig.database, "PRIV"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("forgets owed after the refused first switch = %v, want [1]", got)
	}
	rig.broker.set("PRIV", 0, true)
	refusedWith(t, "a switch the broker does not count", rig.switchPRIV(), http.StatusServiceUnavailable, "DEVELOPER_BOX_TOKENS_UNAVAILABLE")
	if got := brokerForgetsOf(t, rig.database, "PRIV"); len(got) != 1 {
		t.Fatalf("forgets owed after a count the broker did not answer = %v, want [1] still: no record is known", got)
	}
	rig.broker.set("PRIV", 0, false)
	rig.admission.setProveErr(errors.New("hub nats://hub-b.example:4222 refused the admission proof"))
	refusedWith(t, "a switch whose hubs refuse the proof", rig.switchPRIV(), http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE")
	if got := brokerForgetsOf(t, rig.database, "PRIV"); len(got) != 2 || got[1] != 2 {
		t.Fatalf("forgets owed after the switch the hubs refused = %v, want [1 2]", got)
	}
	rig.admission.setProveErr(nil)
	if made := rig.switchPRIV(); made.Code != http.StatusOK || visibilityOf(t, rig.database, "PRIV") != model.ProjectPrivate {
		t.Fatalf("the switch that commits = %d %s", made.Code, made.Body.String())
	}
	if got := brokerForgetsOf(t, rig.database, "PRIV"); len(got) != 2 {
		t.Fatalf("forgets owed after the switch that committed = %v, want [1 2]: it owes none for its own record 3", got)
	}
	if back := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/projects/PRIV/access", map[string]string{"visibility": "public"}, "alice"); back.Code != http.StatusOK {
		t.Fatalf("the switch back to public = %d %s", back.Code, back.Body.String())
	}
	if got := brokerForgetsOf(t, rig.database, "PRIV"); len(got) != 3 || got[2] != 0 {
		t.Fatalf("forgets owed after the switch back to public = %v, want [1 2 0], the last for the record whatever its number", got)
	}

	client := rig.broker.serve(t)
	if err := sendBrokerForgets(ctx, rig.database.Pool, client); err != nil {
		t.Fatalf("send the forgets: %v", err)
	}
	_, forgets := rig.broker.answered()
	if len(forgets) != 3 || forgets[0].Record == nil || *forgets[0].Record != 1 || forgets[1].Record == nil || *forgets[1].Record != 2 || forgets[2].Record != nil {
		t.Fatalf("the broker stand-in answered the forgets %+v, want PRIV's 1, 2 and then its record whatever its number", forgets)
	}
	if got := brokerForgetsOf(t, rig.database, "PRIV"); len(got) != 0 {
		t.Fatalf("forgets owed once the broker took them = %v, want none", got)
	}

	rig.broker.mu.Lock()
	rig.broker.forgetSequence = []int{http.StatusServiceUnavailable, http.StatusConflict, 0}
	rig.broker.mu.Unlock()
	if _, err := rig.database.Pool.Exec(ctx, `insert into broker_forgets (project, record) values ('PRIV', 9)`); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := rig.database.Pool.Exec(ctx, `update broker_forgets set next_attempt_at = now()`); err != nil {
			t.Fatal(err)
		}
		if err := sendBrokerForgets(ctx, rig.database.Pool, client); err != nil {
			t.Fatalf("send the forget, attempt %d: %v", attempt, err)
		}
		_, forgets := rig.broker.answered()
		owed := brokerForgetsOf(t, rig.database, "PRIV")
		if wantKept := attempt < 3; len(forgets) != 3+attempt || (len(owed) == 1) != wantKept {
			t.Fatalf("after attempt %d of the forget the broker answers 503, 409, 200: %d forgets sent in all, owed %v; want %d sent and the forget kept %t",
				attempt, len(forgets), owed, 3+attempt, wantKept)
		}
	}
}

// TestAPrivateSwitchPastItsDeadlineCommitsNothing: a switch whose commit a test hook holds past the
// switch's deadline, lowered here to 2 s, answers 503 PRIVATE_SWITCH_TIMED_OUT and leaves PRIV
// public once the hook lets go: with both deadlines, the Go context's and the database's
// transaction_timeout, and with the database's alone. With the broker's hold of twice the
// deadline, a commit later than the deadline could come after a read had dropped PRIV's record,
// leaving PRIV private and unrecorded.
func TestAPrivateSwitchPastItsDeadlineCommitsNothing(t *testing.T) {
	for _, row := range []struct {
		name            string
		contextDeadline bool
	}{
		{"both deadlines", true},
		{"the database's deadline alone", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newForgetRig(t)
			ctx := context.Background()
			refusedWith(t, "the first switch, which starts the closing state", rig.switchPRIV(), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
			if _, err := rig.database.Pool.Exec(ctx, `
				create function hold_priv_private_commit() returns trigger language plpgsql as $$
				begin
					perform pg_advisory_xact_lock(hashtext('the deadline row holds PRIV''s commit'));
					return null;
				end $$;
				create constraint trigger hold_priv_private_commit after update on projects
					deferrable initially deferred for each row when (new.key = 'PRIV' and new.visibility = 'private')
					execute function hold_priv_private_commit();
			`); err != nil {
				t.Fatalf("hold the commit that makes PRIV private: %v", err)
			}
			holder, err := rig.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(hashtext('the deadline row holds PRIV''s commit'))`); err != nil {
				t.Fatalf("hold PRIV's commit: %v", err)
			}
			previousDeadline, previousContext := privateSwitchDeadline, privateSwitchContextDeadline
			privateSwitchDeadline, privateSwitchContextDeadline = 2*time.Second, row.contextDeadline
			t.Cleanup(func() { privateSwitchDeadline, privateSwitchContextDeadline = previousDeadline, previousContext })
			// The hook lets go after 15 s whatever happened, so a switch with no deadline commits
			// late and the test names it, rather than waiting forever.
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- rig.switchPRIV() }()
			var late *httptest.ResponseRecorder
			select {
			case late = <-answered:
			case <-time.After(15 * time.Second):
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("let PRIV's commit go: %v", err)
			}
			if late == nil {
				late = <-answered
			}
			privateSwitchDeadline, privateSwitchContextDeadline = previousDeadline, previousContext
			if late.Code != http.StatusServiceUnavailable || !strings.Contains(late.Body.String(), `"code":"PRIVATE_SWITCH_TIMED_OUT"`) || visibilityOf(t, rig.database, "PRIV") != model.ProjectPublic {
				t.Fatalf("%s: the switch whose commit is held past its 2 s deadline = %d %s, PRIV %s; want 503 PRIVATE_SWITCH_TIMED_OUT and PRIV public: a late commit",
					row.name, late.Code, late.Body.String(), visibilityOf(t, rig.database, "PRIV"))
			}
		})
	}
}

// TestTheForgetSenderDeliversEachOwedForget: the sender sends each forget that has come due, with
// its record or none, and deletes each the broker takes; it deletes unsent the forget of a project
// that is private when it reads it; a forget the broker answers 409 RECORD_IN_FLIGHT waits the
// retry_after_seconds it names, and one it answers 404, a broker without the route, waits 5 s
// and is counted, each kept with the broker's answer as its error.
func TestTheForgetSenderDeliversEachOwedForget(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	broker := newBrokerStandIn()
	client := broker.serve(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, owner, visibility) values ('PUBA', 'PUBA', 'alice', 'alice', 'public'), ('PRIVB', 'PRIVB', 'alice', 'alice', 'private');
		insert into broker_forgets (project, record) values ('PUBA', 7), ('PRIVB', 8), ('PUBA', null);
	`); err != nil {
		t.Fatal(err)
	}
	if err := sendBrokerForgets(ctx, database.Pool, client); err != nil {
		t.Fatalf("send the forgets: %v", err)
	}
	_, forgets := broker.answered()
	if len(forgets) != 2 || forgets[0].Project != "PUBA" || forgets[0].Record == nil || *forgets[0].Record != 7 || forgets[1].Project != "PUBA" || forgets[1].Record != nil {
		t.Fatalf("the broker answered forgets %+v, want PUBA's 7 then PUBA's with no record, and none of private PRIVB", forgets)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `select count(*) from broker_forgets`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("forgets left after the broker took them = %d (%v), want 0", left, err)
	}

	for _, row := range []struct {
		status, retryAfter int
		wait               time.Duration
		lastError          string
	}{
		{http.StatusConflict, 42, 42 * time.Second, "in flight"},
		{http.StatusNotFound, 0, brokerForgetFirstRetry, "404"},
	} {
		broker.mu.Lock()
		broker.forgetStatus, broker.retryAfter = row.status, row.retryAfter
		broker.mu.Unlock()
		if _, err := database.Pool.Exec(ctx, `delete from broker_forgets; insert into broker_forgets (project, record) values ('PUBA', 9)`); err != nil {
			t.Fatal(err)
		}
		if err := sendBrokerForgets(ctx, database.Pool, client); err != nil {
			t.Fatalf("send the forgets: %v", err)
		}
		var attempts int
		var wait time.Duration
		var lastError string
		if err := database.Pool.QueryRow(ctx, `select attempts, next_attempt_at - now(), last_error from broker_forgets where project = 'PUBA'`).Scan(&attempts, &wait, &lastError); err != nil {
			t.Fatalf("read the forget the broker answered %d: %v", row.status, err)
		}
		if attempts != 1 || wait < row.wait-2*time.Second || wait > row.wait || !strings.Contains(lastError, row.lastError) {
			t.Errorf("the forget the broker answered %d: attempts %d, next in %s, last error %q; want 1, about %s, naming %q", row.status, attempts, wait, lastError, row.wait, row.lastError)
		}
	}
}

// TestTheMembershipAnswerClosesAProjectToDeveloperBoxes: closed_to_developer_boxes is absent for a
// public project, and for a project private on its first day whose key DISPATCH_PRIVATE_PROJECTS
// does not name (DAYONE); it is true for a private project whose key the list names, before any
// switch (PRIV), for a project private by its full switch whose key the list does not name
// (DAYONE once its full switch is recorded), and for a project in its closing state (PUB).
func TestTheMembershipAnswerClosesAProjectToDeveloperBoxes(t *testing.T) {
	rig := newClosingRig(t, []string{"PRIV"}, true)
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "DAYONE", "name": "DAYONE"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create DAYONE: %d %s", response.Code, response.Body.String())
	}
	rig.makeDayOnePrivate(t, "DAYONE")
	for _, key := range []string{"PUB", "PRIV", "DAYONE"} {
		if raw := rig.answerBody(t, key); strings.Contains(raw, "closed_to_developer_boxes") {
			t.Fatalf("the membership answer of %s = %s, want no closed_to_developer_boxes key: it is public, or private with neither its full switch nor its key listed", key, raw)
		}
	}
	rig.makeDayOnePrivate(t, "PRIV")
	if raw := rig.answerBody(t, "PRIV"); !strings.Contains(raw, `"closed_to_developer_boxes":true`) {
		t.Fatalf("the membership answer of PRIV, private with its key on DISPATCH_PRIVATE_PROJECTS, before any switch = %s, want closed_to_developer_boxes true", raw)
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `update projects set full_switch_at = now() where key = 'DAYONE'`); err != nil {
		t.Fatalf("record DAYONE's full switch: %v", err)
	}
	if raw := rig.answerBody(t, "DAYONE"); !strings.Contains(raw, `"closed_to_developer_boxes":true`) {
		t.Fatalf("the membership answer of DAYONE, private by its full switch with its key not listed = %s, want closed_to_developer_boxes true", raw)
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `update projects set closing_since = now() where key = 'PUB'`); err != nil {
		t.Fatalf("put PUB in its closing state: %v", err)
	}
	if raw := rig.answerBody(t, "PUB"); !strings.Contains(raw, `"closed_to_developer_boxes":true`) {
		t.Fatalf("the membership answer of PUB in its closing state = %s, want closed_to_developer_boxes true", raw)
	}
}

// TestNoPersonalTokenListsAPrivateProject: a personal token's mint and its edit are refused 403
// PRIVATE_PROJECT_TOKEN, naming the project, for a project in its closing state, for one private
// by its full switch, and for one private on its first day whose key DISPATCH_PRIVATE_PROJECTS
// names, and the token is not made; a public project, and one private on its first day whose key
// the list does not name, are listed as before.
func TestNoPersonalTokenListsAPrivateProject(t *testing.T) {
	rig := newClosingRig(t, []string{"PRIV", "PUB"}, true)
	lowerSettle(t, time.Hour)
	if made := rig.mint(t, "while public", []string{"PRIV"}); made.Code != http.StatusCreated {
		t.Fatalf("mint naming PRIV while it is public = %d %s, want 201", made.Code, made.Body.String())
	}
	other := decodeBody[agentTokenResponse](t, rig.mint(t, "of PUB", []string{"PUB"}))
	refusedWith(t, "the switch of PRIV", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
	count := func() int {
		var n int
		if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from agent_tokens`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	refusedWith(t, "a mint naming PRIV while it is closing", rig.mint(t, "while closing", []string{"PRIV"}), http.StatusForbidden, "PRIVATE_PROJECT_TOKEN", "PRIV")
	refusedWith(t, "an edit naming PRIV while it is closing",
		dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+other.ID, map[string]any{"scopes": []string{model.ScopePerson}, "projects": []string{"PRIV", "PUB"}}, "alice"),
		http.StatusForbidden, "PRIVATE_PROJECT_TOKEN", "PRIV")
	lowerSettle(t, 0)
	rig.broker.set("PRIV", 0, false)
	if made := rig.switchPrivate(t, "PRIV"); made.Code != http.StatusOK || rig.fullSwitchAt(t, "PRIV") == nil {
		t.Fatalf("the switch that completes PRIV's full switch = %d %s", made.Code, made.Body.String())
	}
	refusedWith(t, "a mint naming PRIV after its full switch", rig.mint(t, "while private", []string{"PRIV"}), http.StatusForbidden, "PRIVATE_PROJECT_TOKEN", "PRIV")
	if after := count(); after != before {
		t.Fatalf("personal tokens after the refused mints = %d, want the %d there were", after, before)
	}
	var listed []string
	if err := rig.database.Pool.QueryRow(context.Background(), `select projects from agent_tokens where id = $1`, other.ID).Scan(&listed); err != nil || len(listed) != 1 || listed[0] != "PUB" {
		t.Fatalf("the edited token lists %v (%v), want PUB alone: the refused edit wrote nothing", listed, err)
	}

	dayOne := newClosingRig(t, []string{"PUB"}, true)
	dayOne.makeDayOnePrivate(t, "PRIV")
	if made := dayOne.mint(t, "on its first day", []string{"PRIV"}); made.Code != http.StatusCreated {
		t.Fatalf("mint naming PRIV on its first day, its key not listed = %d %s, want 201", made.Code, made.Body.String())
	}
	listedDayOne := newClosingRig(t, []string{"PRIV"}, true)
	listedDayOne.makeDayOnePrivate(t, "PRIV")
	refusedWith(t, "a mint naming PRIV on its first day, its key listed", listedDayOne.mint(t, "listed", []string{"PRIV"}), http.StatusForbidden, "PRIVATE_PROJECT_TOKEN", "PRIV")
}

// TestTheClosingStateRevokesPersonalTokensOfTheProject: a personal token made while PRIV was public
// and listing it reads as itself until PRIV's switch to private starts its closing state, and
// answers 401 from then on; a token of another project reads on.
func TestTheClosingStateRevokesPersonalTokensOfTheProject(t *testing.T) {
	rig := newClosingRig(t, []string{"PRIV"}, true)
	lowerSettle(t, time.Hour)
	priv := decodeBody[agentTokenResponse](t, rig.mint(t, "of PRIV", []string{"PRIV"}))
	both := decodeBody[agentTokenResponse](t, rig.mint(t, "of PRIV and PUB", []string{"PRIV", "PUB"}))
	pub := decodeBody[agentTokenResponse](t, rig.mint(t, "of PUB", []string{"PUB"}))
	for _, token := range []string{priv.Token, both.Token, pub.Token} {
		if status := rig.whoami(t, token); status != http.StatusOK {
			t.Fatalf("a token before the switch reads whoami %d, want 200", status)
		}
	}
	refusedWith(t, "the switch of PRIV", rig.switchPrivate(t, "PRIV"), http.StatusConflict, "DEVELOPER_BOX_TOKENS_LIVE")
	for name, token := range map[string]string{"the token of PRIV": priv.Token, "the token of PRIV and PUB": both.Token} {
		if status := rig.whoami(t, token); status != http.StatusUnauthorized {
			t.Fatalf("%s reads whoami %d after PRIV's closing state began, want 401", name, status)
		}
	}
	if status := rig.whoami(t, pub.Token); status != http.StatusOK {
		t.Fatalf("the token of PUB reads whoami %d after PRIV's closing state began, want 200", status)
	}
}

// TestAKeyThatJoinsTheListRevokesThePersonalTokensOfAProjectAlreadyPrivate: a personal token made
// while PRIV was private on its first day, its key not yet on DISPATCH_PRIVATE_PROJECTS, reads on
// until the boot that names the key there runs access.RevokeClosedProjectTokens, as cmd/dispatch's
// boot does, and answers 401 from then on, its owner's switch or not; a token of a public project
// is left. The pass revokes nothing while the list does not name PRIV.
func TestAKeyThatJoinsTheListRevokesThePersonalTokensOfAProjectAlreadyPrivate(t *testing.T) {
	rig := newClosingRig(t, nil, false)
	rig.makeDayOnePrivate(t, "PRIV")
	priv := decodeBody[agentTokenResponse](t, rig.mint(t, "of PRIV", []string{"PRIV"}))
	pub := decodeBody[agentTokenResponse](t, rig.mint(t, "of PUB", []string{"PUB"}))
	if keys, revoked, err := access.RevokeClosedProjectTokens(context.Background(), rig.database.Pool, nil); err != nil || revoked != 0 || len(keys) != 0 {
		t.Fatalf("the boot pass with no key listed = %v, %d, %v; want nothing revoked", keys, revoked, err)
	}
	if status := rig.whoami(t, priv.Token); status != http.StatusOK {
		t.Fatalf("PRIV's token on its first day reads whoami %d, want 200", status)
	}
	keys, revoked, err := access.RevokeClosedProjectTokens(context.Background(), rig.database.Pool, []string{"PRIV"})
	if err != nil || revoked != 1 || len(keys) != 1 || keys[0] != "PRIV" {
		t.Fatalf("the boot pass naming PRIV = %v, %d, %v; want PRIV's one token revoked", keys, revoked, err)
	}
	if status := rig.whoami(t, priv.Token); status != http.StatusUnauthorized {
		t.Fatalf("PRIV's token after the boot that names PRIV reads whoami %d, want 401", status)
	}
	if status := rig.whoami(t, pub.Token); status != http.StatusOK {
		t.Fatalf("PUB's token after that boot reads whoami %d, want 200", status)
	}
}
