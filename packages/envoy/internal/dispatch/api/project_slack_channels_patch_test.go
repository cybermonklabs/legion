package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// channelSwitchServer is a test server whose deployment provisions OTHER, PRIV and PUB as private
// and proves the hubs' admission through admission. It holds those three, all alice's, and HIDE,
// carol's private project, with the store complete (newPrivateProjectsServer), and the broker's
// service account, whose bearer reads GET /api/v1/access/private-projects and the membership answer.
type channelSwitchServer struct {
	handler  http.Handler
	database *store.Store
	bearer   string
}

func newChannelSwitchServer(t *testing.T, admission *recordingAdmission) channelSwitchServer {
	t.Helper()
	issuer := newServiceTokenIssuer(t)
	handler, database := newPrivateProjectsServer(t, testServerOptions{
		privateProjects: []string{"OTHER", "PRIV", "PUB"},
		oidc:            issuer.verifier(t),
		accessAccounts:  []string{listedServiceAccount},
	}, admission)
	createAccessProject(t, handler, "PUB", "alice")
	return channelSwitchServer{handler: handler, database: database,
		bearer: issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))}
}

// patch is alice's PATCH /api/v1/projects/{key}/access with body.
func (s channelSwitchServer) patch(t *testing.T, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, s.handler, http.MethodPatch, "/api/v1/projects/"+key+"/access", body, "alice")
}

// privateList is the broker's read of the private projects, by key.
func (s channelSwitchServer) privateList(t *testing.T) map[string]model.PrivateProjectChannels {
	t.Helper()
	response := agentRequest(t, s.handler, http.MethodGet, privateProjectsPath, nil, s.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("the broker's GET %s = %d %s, want 200", privateProjectsPath, response.Code, response.Body.String())
	}
	listed := map[string]model.PrivateProjectChannels{}
	for _, project := range decodeBody[privateProjectsAnswer](t, response).Projects {
		listed[project.Project] = project
	}
	return listed
}

// channelsTimeOf is project key's slack_channels_changed_at as committed, nil while it is null.
func channelsTimeOf(t *testing.T, database *store.Store, key string) *time.Time {
	t.Helper()
	var changedAt *time.Time
	if err := database.Pool.QueryRow(context.Background(), `select slack_channels_changed_at from projects where key = $1`, key).Scan(&changedAt); err != nil {
		t.Fatalf("read %s's channels time: %v", key, err)
	}
	return changedAt
}

// accessPatch is one PATCH /api/v1/projects/{key}/access with body as login, built with no test
// handle, so a goroutine of a test sends it, bounded by ctx.
func accessPatch(ctx context.Context, handler http.Handler, key, body, login string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(ctx, http.MethodPatch, "/api/v1/projects/"+key+"/access", bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestAChannelPatchMakesNoBusCall: no copy of a project's Slack channels exists outside Dispatch's
// database, so a write of them is the commit alone and makes no hub-admission proof, for a private
// project as for a public one, and the broker's next read of the private projects lists the new
// channels. Control: call Prove in the slack_channels write, and the recording notes a call, and the
// test fails naming the PATCH.
func TestAChannelPatchMakesNoBusCall(t *testing.T) {
	admission := &recordingAdmission{}
	server := newChannelSwitchServer(t, admission)
	makePrivate(t, server.database, "PRIV", "C0PRIV00001")
	for _, row := range []struct {
		key, channel, visibility string
	}{
		{"PRIV", "C0PRIV00002", model.ProjectPrivate},
		{"OTHER", "C0OTHER0001", model.ProjectPublic},
	} {
		response := server.patch(t, row.key, map[string]any{"slack_channels": []string{row.channel}})
		if response.Code != http.StatusOK {
			t.Fatalf("alice's PATCH of %s's channels to [%s] = %d %s, want 200", row.key, row.channel, response.Code, response.Body.String())
		}
		if calls := admission.taken(); len(calls) != 0 {
			t.Fatalf("PATCH %s {slack_channels: [%s]}, a %s project, made the hub-admission proof %+v: a channel write makes no bus call", row.key, row.channel, row.visibility, calls)
		}
	}
	if listed := server.privateList(t); !slices.Equal(listed["PRIV"].SlackChannels, []string{"C0PRIV00002"}) {
		t.Fatalf("the broker's read after PRIV's channel write lists %v for PRIV, want [C0PRIV00002]: the committed rows", listed["PRIV"].SlackChannels)
	}
}

// TestASwitchToPrivateRefusesASharedSlackChannel: a Slack channel belongs to one private project.
// PRIV is private and names C0SHARED001, and PUB named the same channel while public, which no
// write refuses. PUB's owner's switch of PUB to private is 409 SLACK_CHANNEL_SHARED, PUB stays
// public with nothing appended and no proof made. The refusal names PRIV to alice at her browser,
// who sees it, and to her dsp_ token listing no project, which sees only public projects, it names
// no project. Controls, each on its own: drop the check, and the switch lands and the test names
// PUB; read "sees" as the person sees, and the token's row names PRIV and the test names it.
func TestASwitchToPrivateRefusesASharedSlackChannel(t *testing.T) {
	admission := &recordingAdmission{project: "PUB"}
	server := newChannelSwitchServer(t, admission)
	makePrivate(t, server.database, "PRIV", "C0SHARED001")
	if response := server.patch(t, "PUB", map[string]any{"slack_channels": []string{"C0SHARED001"}}); response.Code != http.StatusOK {
		t.Fatalf("alice names C0SHARED001 on PUB while it is public: %d %s, want 200", response.Code, response.Body.String())
	}
	unlisted := mintListed(t, server.handler, "alice", "alice's token listing no project", nil)
	private := map[string]string{"visibility": model.ProjectPrivate}
	for _, row := range []struct {
		who  string
		send func() *httptest.ResponseRecorder
		want string
	}{
		{"alice at her browser, who sees PRIV", func() *httptest.ResponseRecorder { return server.patch(t, "PUB", private) },
			"C0SHARED001 is listed by PRIV, a private project; a Slack channel belongs to one private project"},
		{"alice's dsp_ token listing no project", func() *httptest.ResponseRecorder {
			return agentRequest(t, server.handler, http.MethodPatch, "/api/v1/projects/PUB/access", private, unlisted.Token)
		}, "C0SHARED001 is listed by a private project you cannot see"},
	} {
		before := allEvents(t, server.database)
		response := row.send()
		if visibilityOf(t, server.database, "PUB") != model.ProjectPublic {
			t.Fatalf("%s: the switch of PUB to private landed (%d %s) while PRIV lists its channel C0SHARED001", row.who, response.Code, response.Body.String())
		}
		requireRefused(t, server.database, response, "PUB", before, http.StatusConflict, "SLACK_CHANNEL_SHARED")
		if refusal := decodeBody[errorBody](t, response); refusal.Error != row.want {
			t.Fatalf("%s: the refusal reads %q, want %q", row.who, refusal.Error, row.want)
		}
		if row.want != "" && !strings.Contains(row.want, "PRIV") && strings.Contains(response.Body.String(), "PRIV") {
			t.Fatalf("%s: the refusal %s names PRIV, which the credential does not see", row.who, response.Body.String())
		}
	}
	if calls := admission.taken(); len(calls) != 0 {
		t.Fatalf("the refused switches of PUB made the proof: %+v", calls)
	}
}

// TestASwitchToPrivateSetsTheChannelsTime: the channels' time is when they became a private
// project's. PUB names C0PUB000001 at T0 while public and switches to private at T, and its access
// answer, the membership answer and the broker's list each carry slack_channels_changed_at = T,
// after T0. OTHER names no channel, and its switch leaves its time null. Control: leave the time
// unset at the switch, and PUB's answers carry T0, and the test names PUB.
func TestASwitchToPrivateSetsTheChannelsTime(t *testing.T) {
	server := newChannelSwitchServer(t, &recordingAdmission{project: "PUB"})
	if response := server.patch(t, "PUB", map[string]any{"slack_channels": []string{"C0PUB000001"}}); response.Code != http.StatusOK {
		t.Fatalf("alice names C0PUB000001 on PUB while it is public: %d %s, want 200", response.Code, response.Body.String())
	}
	named := channelsTimeOf(t, server.database, "PUB")
	switched := server.patch(t, "PUB", map[string]string{"visibility": model.ProjectPrivate})
	if switched.Code != http.StatusOK {
		t.Fatalf("PATCH PUB private = %d %s, want 200", switched.Code, switched.Body.String())
	}
	at := channelsTimeOf(t, server.database, "PUB")
	if named == nil || at == nil || !at.After(*named) {
		t.Fatalf("PUB's channels time: %v when it named C0PUB000001, %v after its switch to private; want the switch to start it again, later", named, at)
	}
	answers := map[string]*time.Time{
		"the switch's own answer": decodeBody[model.ProjectAccess](t, switched).SlackChannelsChangedAt,
		"the broker's list":       server.privateList(t)["PUB"].SlackChannelsChangedAt,
	}
	membership := agentRequest(t, server.handler, http.MethodGet, "/api/v1/access/projects/PUB?person=alice", nil, server.bearer)
	if membership.Code != http.StatusOK {
		t.Fatalf("the membership answer of PUB = %d %s, want 200", membership.Code, membership.Body.String())
	}
	answers["the membership answer"] = decodeBody[model.AccessAnswer](t, membership).SlackChannelsChangedAt
	for where, carried := range answers {
		if carried == nil || !carried.Equal(*at) {
			t.Errorf("PUB: %s carries slack_channels_changed_at %v, want %v, the moment of its switch to private", where, carried, at)
		}
	}
	if response := server.patch(t, "OTHER", map[string]string{"visibility": model.ProjectPrivate}); response.Code != http.StatusOK {
		t.Fatalf("PATCH OTHER private = %d %s, want 200", response.Code, response.Body.String())
	}
	if at := channelsTimeOf(t, server.database, "OTHER"); at != nil {
		t.Errorf("OTHER, which names no channel, has the channels time %v after its switch, want null", at)
	}
}

// TestBothPatchShapesTakeLocksInOneOrder: every PATCH of a project's access takes the project's
// owner lock, then the Slack channel lock. A PATCH that sets both the channels and private, and a
// plain PATCH of private, run at once on PRIV 200 times: neither ever answers PostgreSQL's 40P01,
// and each finishes 200. Control: take the channel lock before the owner lock in the both-fields
// PATCH (SetSlackChannels before ownerWrite); the two writers wait on each other, PostgreSQL
// reports deadlock detected, and the test fails naming the PATCH.
func TestBothPatchShapesTakeLocksInOneOrder(t *testing.T) {
	server := newChannelSwitchServer(t, &recordingAdmission{})
	shapes := map[string]string{
		"the PATCH of the channels and private": `{"slack_channels":["C0BOTH00001"],"visibility":"private"}`,
		"the PATCH of private":                  `{"visibility":"private"}`,
	}
	for round := range 200 {
		if _, err := server.database.Pool.Exec(context.Background(), `update projects set visibility = 'public', slack_channels = '{}' where key = 'PRIV'`); err != nil {
			t.Fatalf("round %d: make PRIV public again: %v", round, err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		answered := make(map[string]*httptest.ResponseRecorder, len(shapes))
		var mu sync.Mutex
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		for shape, body := range shapes {
			wg.Go(func() {
				<-start
				response := accessPatch(ctx, server.handler, "PRIV", body, "alice")
				mu.Lock()
				answered[shape] = response
				mu.Unlock()
			})
		}
		close(start)
		wg.Wait()
		cancel()
		for shape, response := range answered {
			if body := response.Body.String(); response.Code != http.StatusOK {
				t.Fatalf("round %d: %s on PRIV = %d %s, want 200: the two shapes must take their locks in one order (40P01 is a deadlock)", round, shape, response.Code, body)
			}
		}
	}
}

// TestALockNotTakenInTimeIsNamed: a PATCH of a project's access that sets its visibility or its
// channels waits for each of its locks at most patchLockTimeout, lowered here to 1 s, and then
// answers 409 LOCK_TIMEOUT naming the lock it did not take, with nothing changed. Each row holds one
// lock in a transaction of the test's own while alice's PATCH of PRIV's channels runs, bounded by a
// 10 s deadline: the project's owner lock every writer of it takes (owner-row), the project's row
// as a build that orders writers on the row holds it (owner-row), and the Slack channel lock
// (slack-channel). The answer comes within 3 s, PRIV's channels and events are as they were, and
// once the test's transaction ends the same PATCH answers 200. Control: drop `set local
// lock_timeout`, and each PATCH waits to its 10 s deadline, and the test fails naming the lock.
func TestALockNotTakenInTimeIsNamed(t *testing.T) {
	previous := patchLockTimeout
	t.Cleanup(func() { patchLockTimeout = previous })
	patchLockTimeout = time.Second
	server := newChannelSwitchServer(t, &recordingAdmission{})
	makePrivate(t, server.database, "PRIV", "C0PRIV00001")
	for _, row := range []struct {
		name, lock string
		hold       func(ctx context.Context, tx pgx.Tx) error
	}{
		{"the project's owner lock", "owner-row", func(ctx context.Context, tx pgx.Tx) error {
			return store.LockOwner(ctx, tx, store.ProjectOwner("PRIV"))
		}},
		{"the project's row", "owner-row", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select 1 from projects where key = 'PRIV' for update`)
			return err
		}},
		{"the Slack channel lock", "slack-channel", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-slack-channel'), 0)`)
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			held, err := server.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the transaction that holds %s: %v", row.name, err)
			}
			defer held.Rollback(ctx)
			if err := row.hold(ctx, held); err != nil {
				t.Fatalf("hold %s: %v", row.name, err)
			}
			before := allEvents(t, server.database)
			deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			sent := time.Now()
			response := accessPatch(deadline, server.handler, "PRIV", `{"slack_channels":["C0PRIV00002"]}`, "alice")
			took := time.Since(sent)
			refusal := errorBody{}
			_ = json.Unmarshal(response.Body.Bytes(), &refusal)
			if response.Code != http.StatusConflict || refusal.Code != "LOCK_TIMEOUT" || !strings.Contains(refusal.Error, "the "+row.lock+" lock") || took > 3*time.Second {
				t.Fatalf("with %s held, alice's PATCH of PRIV's channels answered %d %s after %s, want 409 LOCK_TIMEOUT naming the %s lock within 3s",
					row.name, response.Code, response.Body.String(), took.Round(time.Millisecond), row.lock)
			}
			var channels []string
			if err := server.database.Pool.QueryRow(ctx, `select slack_channels from projects where key = 'PRIV'`).Scan(&channels); err != nil || !slices.Equal(channels, []string{"C0PRIV00001"}) {
				t.Fatalf("with %s held, the refused PATCH left PRIV's channels %v (%v), want [C0PRIV00001]", row.name, channels, err)
			}
			if after := allEvents(t, server.database); after != before {
				t.Fatalf("with %s held, the refused PATCH appended %d events", row.name, after-before)
			}
			if err := held.Rollback(ctx); err != nil {
				t.Fatalf("release %s: %v", row.name, err)
			}
			if again := accessPatch(ctx, server.handler, "PRIV", `{"slack_channels":["C0PRIV00002"]}`, "alice"); again.Code != http.StatusOK {
				t.Fatalf("once %s is released, alice's PATCH of PRIV's channels = %d %s, want 200", row.name, again.Code, again.Body.String())
			}
			if _, err := server.database.Pool.Exec(ctx, `update projects set slack_channels = '{C0PRIV00001}' where key = 'PRIV'`); err != nil {
				t.Fatal(fmt.Errorf("put PRIV's channels back for the next row: %w", err))
			}
		})
	}
}
