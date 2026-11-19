package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// createAccessProject creates project key as login, who owns it from then on.
func createAccessProject(t *testing.T, handler http.Handler, key, login string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, login); response.Code != http.StatusCreated {
		t.Fatalf("%s creates %s: %d %s", login, key, response.Code, response.Body.String())
	}
}

// wantChannelsUnwritten fails the test unless project key still names no channel, its channels
// time is still null, and no event was appended since before.
func wantChannelsUnwritten(t *testing.T, database *store.Store, key string, before int64) {
	t.Helper()
	var channels []string
	var changedAt *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select slack_channels, slack_channels_changed_at from projects where key = $1`, key).Scan(&channels, &changedAt); err != nil {
		t.Fatalf("read %s's channels: %v", key, err)
	}
	if len(channels) != 0 || changedAt != nil {
		t.Fatalf("%s's channels moved to %v (changed %v) under a refused write", key, channels, changedAt)
	}
	if after := allEvents(t, database); after != before {
		t.Fatalf("events moved from %d to %d under a refused write of %s's channels", before, after, key)
	}
}

// TestASharedSlackChannelIsRefused (c-a-slack-channels): a Slack channel belongs to at most one
// private project. PRIV is private and names C0SHARED001; a second private project's write of the
// same channel is 409 SLACK_CHANNEL_SHARED and writes nothing, and the refusal names PRIV only to
// a caller who sees it. Two writes of one channel to two private projects at once cannot both
// pass, because the channel lock orders them. Controls, each on its own: drop the shared-channel
// check, and the first row's write lands and the test fails naming the channel; drop the channel
// lock, and both racing writes land and the race row fails naming the race.
func TestASharedSlackChannelIsRefused(t *testing.T) {
	handler, database := newAccessServer(t)
	createAccessProject(t, handler, "PRIV", "alice")
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/PRIV/access/people/"+accessDave, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice names dave on PRIV's list: %d %s", response.Code, response.Body.String())
	}
	makePrivate(t, database, "PRIV", "C0SHARED001")
	patch := func(t *testing.T, key, login string, channels ...string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/"+key+"/access", map[string]any{"slack_channels": channels}, login)
	}
	wantShared := func(t *testing.T, response *httptest.ResponseRecorder, who, text string) {
		t.Helper()
		refusal := decodeBody[errorBody](t, response)
		if response.Code != http.StatusConflict || refusal.Code != "SLACK_CHANNEL_SHARED" || refusal.Error != text {
			t.Fatalf("%s's write of C0SHARED001 = %d %s, want 409 SLACK_CHANNEL_SHARED reading %q", who, response.Code, response.Body.String(), text)
		}
	}

	t.Run("PRIV2's owner, who sees PRIV, reads its name", func(t *testing.T) {
		createAccessProject(t, handler, "PRIV2", accessDave)
		makePrivate(t, database, "PRIV2")
		before := allEvents(t, database)
		wantShared(t, patch(t, "PRIV2", accessDave, "C0SHARED001"), accessDave,
			"C0SHARED001 is listed by PRIV, a private project; a Slack channel belongs to one private project")
		wantChannelsUnwritten(t, database, "PRIV2", before)
	})

	t.Run("carol, who does not see PRIV, reads no project's name", func(t *testing.T) {
		createAccessProject(t, handler, "CAROL", accessCarol)
		makePrivate(t, database, "CAROL")
		before := allEvents(t, database)
		response := patch(t, "CAROL", accessCarol, "C0SHARED001")
		wantShared(t, response, accessCarol, "C0SHARED001 is listed by a private project you cannot see")
		if strings.Contains(response.Body.String(), "PRIV") {
			t.Fatalf("carol's refusal names PRIV, which she cannot see: %s", response.Body.String())
		}
		wantChannelsUnwritten(t, database, "CAROL", before)
	})

	t.Run("two writes of one channel at once: exactly one lands", func(t *testing.T) {
		ctx := context.Background()
		for _, key := range []string{"RACEA", "RACEB"} {
			createAccessProject(t, handler, key, "alice")
			makePrivate(t, database, key)
		}
		// Each write of either project is held at its commit, after every statement it makes, until
		// the lock the test holds below is released, so both reach their commit before either
		// commits unless something orders them first.
		if _, err := database.Pool.Exec(ctx, `
			create function hold_race_commit() returns trigger language plpgsql as $$
			begin
				perform pg_advisory_xact_lock(hashtext('the race row holds this commit'));
				return null;
			end
			$$
		`); err != nil {
			t.Fatalf("create the commit-holding trigger function: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			create constraint trigger hold_race_commit after update on projects
			deferrable initially deferred for each row
			when (new.key in ('RACEA', 'RACEB'))
			execute function hold_race_commit()
		`); err != nil {
			t.Fatalf("create the commit-holding trigger: %v", err)
		}
		held, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction that holds both commits: %v", err)
		}
		defer held.Rollback(ctx)
		if _, err := held.Exec(ctx, `select pg_advisory_xact_lock(hashtext('the race row holds this commit'))`); err != nil {
			t.Fatalf("hold both commits: %v", err)
		}
		type answer struct {
			key      string
			response *httptest.ResponseRecorder
		}
		answered := make(chan answer, 2)
		for _, key := range []string{"RACEA", "RACEB"} {
			go func() {
				request := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+key+"/access",
					strings.NewReader(`{"slack_channels":["C0RACE00001"]}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				answered <- answer{key, response}
			}()
		}
		// Both writes queue behind the held lock: each at its commit, or one at its commit and the
		// other behind the channel lock that one holds.
		waitForDatabaseLocks(t, held, 2)
		if err := held.Rollback(ctx); err != nil {
			t.Fatalf("release both commits: %v", err)
		}
		landed := map[string]bool{}
		refused := 0
		for range 2 {
			select {
			case got := <-answered:
				switch {
				case got.response.Code == http.StatusOK:
					landed[got.key] = true
				case got.response.Code == http.StatusConflict && decodeBody[errorBody](t, got.response).Code == "SLACK_CHANNEL_SHARED":
					refused++
				default:
					t.Fatalf("the racing write of C0RACE00001 to %s = %d %s, want 200 or 409 SLACK_CHANNEL_SHARED", got.key, got.response.Code, got.response.Body.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatal("a racing write of C0RACE00001 never answered once both commits were released")
			}
		}
		if len(landed) != 1 || refused != 1 {
			t.Fatalf("the race: two writes of C0RACE00001 to RACEA and RACEB at once landed on %v with %d refused, want exactly one landed and one 409 SLACK_CHANNEL_SHARED: the channel lock must order the two", landed, refused)
		}
		listing := countRows(t, database, `select count(*) from projects where key in ('RACEA', 'RACEB') and 'C0RACE00001' = any(slack_channels)`)
		if listing != 1 {
			t.Fatalf("the race: %d of RACEA and RACEB list C0RACE00001 once both writes answered, want 1", listing)
		}
	})
}

// TestTheSharedChannelRefusalReadsTheCallersCredential: the refusal names the other private
// project only to a caller who sees it by the read rule, read for the request's own credential
// (the viewer ownerWrite resolved), never for the person actingPerson answers. alice owns both
// projects, so at her browser she sees PRIV and reads its name. Her personal token listing PRIV2
// alone sees PRIV2, so its write reaches the check, and not PRIV, so it reads the refusal that
// names no project. A broker-issued token reaches this person's route only when it holds no one
// project (broker_token.go: the person scope), and such a token sees no private project, so its
// write of PRIV2 answers 404 PROJECT_NOT_FOUND before the check, as every write of a project it
// cannot see does; read as its person it would reach the check and read PRIV's name. Controls:
// name PRIV whenever the credential's person sees it, or read the person's viewer in place of the
// request's, and the token rows fail naming the token.
func TestTheSharedChannelRefusalReadsTheCallersCredential(t *testing.T) {
	rig := newBrokerTokenRig(t)
	// alice owns both, so she sees PRIV.
	createAccessProject(t, rig.handler, "PRIV", "alice")
	makePrivate(t, rig.database, "PRIV", "C0SHARED001")
	createAccessProject(t, rig.handler, "PRIV2", "alice")
	makePrivate(t, rig.database, "PRIV2")

	const path = "/api/v1/projects/PRIV2/access"
	body := map[string]any{"slack_channels": []string{"C0SHARED001"}}
	caller := newBrokerCaller(t)
	const brokerToken = "alice-broker-token-holding-no-project"
	says := liveNamedToken(caller.jkt)
	says.Person, says.ProjectScope, says.Projects = "alice", brokertoken.ProjectScopePerson, nil
	rig.broker.Says(brokerToken, says)
	personal := mintListed(t, rig.handler, "alice", "alice's token listing PRIV2", []string{"PRIV2"})
	const unseen = "C0SHARED001 is listed by a private project you cannot see"
	for _, row := range []struct {
		who    string
		send   func() *httptest.ResponseRecorder
		status int
		code   string
		want   string
	}{
		{"alice at her browser", func() *httptest.ResponseRecorder {
			return dispatchRequest(t, rig.handler, http.MethodPatch, path, body, "alice")
		}, http.StatusConflict, "SLACK_CHANNEL_SHARED", "C0SHARED001 is listed by PRIV, a private project; a Slack channel belongs to one private project"},
		{"alice's dsp_ token listing PRIV2", func() *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodPatch, path, body, personal.Token)
		}, http.StatusConflict, "SLACK_CHANNEL_SHARED", unseen},
		{"alice's broker token holding no project", func() *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, http.MethodPatch, path, brokerToken, body))
		}, http.StatusNotFound, "PROJECT_NOT_FOUND", ""},
	} {
		before := allEvents(t, rig.database)
		response := row.send()
		refusal := decodeBody[errorBody](t, response)
		if response.Code != row.status || refusal.Code != row.code || (row.want != "" && refusal.Error != row.want) {
			t.Errorf("%s writes C0SHARED001 on PRIV2 = %d %s, want %d %s reading %q", row.who, response.Code, response.Body.String(), row.status, row.code, row.want)
		}
		if row.status != http.StatusConflict && strings.Contains(response.Body.String(), "PRIV,") {
			t.Errorf("%s writes C0SHARED001 on PRIV2 = %d %s: it names PRIV, which it does not see", row.who, response.Code, response.Body.String())
		}
		wantChannelsUnwritten(t, rig.database, "PRIV2", before)
	}
}

// TestAPrivateProjectKeepsItsOwnSlackChannels: the shared-channel check compares a private
// project's list with every OTHER private project's, never with its own, so the owner of a private
// project that already names a channel can save that list again and add a channel beside it. The
// break it catches: SharedSlackChannel losing `key <> $1`, which refuses every later write of a
// private project's own list with "C0OWN000001 is listed by PRIV, a private project", its own name.
func TestAPrivateProjectKeepsItsOwnSlackChannels(t *testing.T) {
	handler, database := newAccessServer(t)
	createAccessProject(t, handler, "PRIV", "alice")
	makePrivate(t, database, "PRIV", "C0OWN000001")
	for _, channels := range [][]string{{"C0OWN000001"}, {"C0OWN000001", "C0SECOND001"}} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
			map[string]any{"slack_channels": channels}, "alice")
		if got := decodeBody[model.ProjectAccess](t, response); response.Code != http.StatusOK || !slices.Equal(got.SlackChannels, channels) {
			t.Fatalf("alice saves private PRIV's own list as %v = %d %s, want 200 with that list: a project's own channels are never shared with itself",
				channels, response.Code, response.Body.String())
		}
	}
}
